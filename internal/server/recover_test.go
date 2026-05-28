package server_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/server"
)

// wavefrontErrorMD builds the canonical wavefront.v0.Error descriptor inline.
// Decoding the panic response through an independently-constructed descriptor
// proves the wire shape from the outside — the test does not depend on
// wireerror internals or on protoc-generated code.
func wavefrontErrorMD(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("wavefront/v0/error.proto"),
		Package: proto.String("wavefront.v0"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Error"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:     proto.String("code"),
					Number:   proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					JsonName: proto.String("code"),
				},
				{
					Name:     proto.String("message"),
					Number:   proto.Int32(2),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					JsonName: proto.String("message"),
				},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("building wavefront.v0.Error descriptor: %v", err)
	}
	return fd.Messages().Get(0)
}

func TestPanicReturnsInternalErrorEnvelope(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name:    "string panic",
			handler: func(_ http.ResponseWriter, _ *http.Request) { panic("boom") },
		},
		{
			name:    "error panic",
			handler: func(_ http.ResponseWriter, _ *http.Request) { panic(io.EOF) },
		},
		{
			name: "nil-deref panic",
			handler: func(_ http.ResponseWriter, _ *http.Request) {
				var p *int
				_ = *p
			},
		},
	}
	md := wavefrontErrorMD(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Capture the slog output so the test can assert the panic is logged.
			var logbuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			s := server.New(baseCfg("http://unused"))
			s.SetLogger(logger)

			ts := httptest.NewServer(s.Recover(tc.handler))
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/anything")
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("status=%d want 500", resp.StatusCode)
			}
			if got := resp.Header.Get("X-Wavefront-Error"); got != "internal_error" {
				t.Errorf("X-Wavefront-Error=%q want internal_error", got)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
				t.Errorf("Content-Type=%q want application/protobuf", got)
			}
			// X-Wavefront-Contract-Version must be set on every response — even
			// when no negotiation has occurred — carrying the "unknown" sentinel
			// to match the pre-negotiate error path.
			if got := resp.Header.Get("X-Wavefront-Contract-Version"); got == "" {
				t.Errorf("X-Wavefront-Contract-Version must be set on every response, got empty")
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			m := dynamicpb.NewMessage(md)
			if err := proto.Unmarshal(body, m); err != nil {
				t.Fatalf("response body is not wavefront.v0.Error: %v (raw=%q)", err, body)
			}
			if got := m.Get(md.Fields().ByName("code")).String(); got != "internal_error" {
				t.Errorf("decoded code=%q want internal_error", got)
			}
			if got := m.Get(md.Fields().ByName("message")).String(); got == "" {
				t.Errorf("decoded message is empty; want a non-empty default")
			}

			// The panic must surface in the structured log.
			if !strings.Contains(logbuf.String(), "panic") {
				t.Errorf("log output did not mention panic; got:\n%s", logbuf.String())
			}
		})
	}
}

// http.ErrAbortHandler is the stdlib's "abort the connection silently"
// sentinel — used by http.MaxBytesReader, timeout handlers, etc. Swallowing
// it would violate the contract: the recover middleware must re-panic, never
// log, never write a wire-error envelope, and never count it on the errors
// metric.
func TestRecoverRePanicsErrAbortHandler(t *testing.T) {
	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s := server.New(baseCfg("http://unused"))
	s.SetLogger(logger)

	handler := s.Recover(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)

	var got any
	func() {
		defer func() { got = recover() }()
		handler.ServeHTTP(rec, req)
	}()

	gotErr, ok := got.(error)
	if !ok || !errors.Is(gotErr, http.ErrAbortHandler) {
		t.Fatalf("expected ErrAbortHandler to be re-panicked, got %v (%T)", got, got)
	}
	if rec.Header().Get("X-Wavefront-Error") != "" {
		t.Errorf("X-Wavefront-Error must not be set on ErrAbortHandler abort, got %q",
			rec.Header().Get("X-Wavefront-Error"))
	}
	if rec.Body.Len() != 0 {
		t.Errorf("response body must be empty on ErrAbortHandler abort, got %d bytes", rec.Body.Len())
	}
	if logbuf.Len() != 0 {
		t.Errorf("no log output expected on ErrAbortHandler abort, got:\n%s", logbuf.String())
	}
}

// If a handler started writing the response before panicking, the recover
// middleware must NOT call wireerror.Write — doing so would trigger a
// "superfluous response.WriteHeader" warning, leave the original status on
// the wire, and append the protobuf body to whatever the handler had
// already written, producing a malformed response. The middleware must
// still log and count the panic; only the envelope emission is suppressed.
func TestRecoverSkipsEnvelopeAfterResponseStarted(t *testing.T) {
	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s := server.New(baseCfg("http://unused"))
	s.SetLogger(logger)

	const writtenBody = "partial body before panic"
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, writtenBody)
		panic("boom after headers")
	})

	ts := httptest.NewServer(s.Recover(handler))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/anything")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	// Status must remain what the handler wrote — the recover block must NOT
	// have called WriteHeader again.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200 (handler's original status, not the envelope's 500)", resp.StatusCode)
	}
	// X-Wavefront-Error would have been set by wireerror.Write — its absence
	// proves the envelope was suppressed.
	if got := resp.Header.Get("X-Wavefront-Error"); got != "" {
		t.Errorf("X-Wavefront-Error must not be set when response was already written, got %q", got)
	}
	// Content-Type must be the one the handler set, not the envelope's
	// application/protobuf.
	if got := resp.Header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type=%q; want the handler's text/plain, not the envelope's application/protobuf", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Body must be exactly what the handler wrote — no protobuf bytes appended.
	if string(body) != writtenBody {
		t.Errorf("body=%q; want %q (no envelope bytes should be appended)", body, writtenBody)
	}
	// The panic still must be observed in the log — the envelope is suppressed,
	// not the forensic trail.
	log := logbuf.String()
	if !strings.Contains(log, "panic recovered in proxy handler") {
		t.Errorf("primary panic-recovered log line missing; got:\n%s", log)
	}
	if !strings.Contains(log, "cannot emit") {
		t.Errorf("expected a log line explaining the envelope was suppressed; got:\n%s", log)
	}
}

// When the client sent an X-Api-Contract-Version on the request, the
// panic envelope must echo that raw value in X-Wavefront-Contract-Version
// — not the `unknown` sentinel — so the caller can correlate the failure
// with the version it asked for. This mirrors the pre-negotiate error
// path on the proxy handler; the recover middleware sees the request
// from a different angle but must adopt the same split (raw header on
// the wire, bounded `unknown` on the metric label).
func TestRecoverEchoesContractVersionHeader(t *testing.T) {
	var logbuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s := server.New(baseCfg("http://unused"))
	s.SetLogger(logger)

	handler := s.Recover(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	}))

	ts := httptest.NewServer(handler)
	defer ts.Close()

	const clientVersion = "2024-11"
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/anything", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Api-Contract-Version", clientVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", resp.StatusCode)
	}
	// The raw client-sent value must surface on the response header — NOT
	// the `unknown` sentinel — so the caller can correlate the failure
	// with the version it asked for.
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != clientVersion {
		t.Errorf("X-Wavefront-Contract-Version=%q want %q (raw client value, not unknown)", got, clientVersion)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != "internal_error" {
		t.Errorf("X-Wavefront-Error=%q want internal_error", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
		t.Errorf("Content-Type=%q want application/protobuf", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	md := wavefrontErrorMD(t)
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(body, m); err != nil {
		t.Fatalf("response body is not wavefront.v0.Error: %v (raw=%q)", err, body)
	}
	if got := m.Get(md.Fields().ByName("code")).String(); got != "internal_error" {
		t.Errorf("decoded code=%q want internal_error", got)
	}
}

func TestRecoverPassesThroughNormalResponse(t *testing.T) {
	// The middleware must be a no-op for a handler that returns normally —
	// it must not flush headers prematurely or interfere with the response.
	s := server.New(baseCfg("http://unused"))
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "i am a teapot")
	})
	ts := httptest.NewServer(s.Recover(handler))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/whatever")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status=%d want 418", resp.StatusCode)
	}
	if resp.Header.Get("X-Wavefront-Error") != "" {
		t.Errorf("X-Wavefront-Error must not be set on a normal response, got %q", resp.Header.Get("X-Wavefront-Error"))
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "i am a teapot" {
		t.Errorf("body=%q want %q", body, "i am a teapot")
	}
}
