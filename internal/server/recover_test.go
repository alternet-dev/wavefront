package server_test

import (
	"bytes"
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
