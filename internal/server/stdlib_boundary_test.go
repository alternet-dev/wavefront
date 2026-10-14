package server_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alternet-dev/wavefront/internal/server"
	"google.golang.org/protobuf/encoding/protowire"
)

// scrapeBoundaryCount asks the ops /metrics endpoint for the value of
// wavefront_stdlib_boundary_total for the given `status` label. The counter
// is a CounterVec labelled by HTTP status code (or the literal "unknown" when
// net/http closes the connection without writing a response line, e.g. the
// slow-header path). Returns 0 when the labelled line is absent (e.g. before
// the cohort has been observed).
func scrapeBoundaryCount(t *testing.T, opsURL, status string) float64 {
	t.Helper()
	resp, err := http.Get(opsURL + "/metrics")
	if err != nil {
		t.Fatalf("scrape /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	prefix := fmt.Sprintf(`wavefront_stdlib_boundary_total{status="%s"} `, status)
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
		if err != nil {
			t.Fatalf("parse stdlib_boundary value from %q: %v", line, err)
		}
		return v
	}
	return 0
}

// waitForBoundaryCount polls the metric for up to d for the labelled counter
// to reach want. StateClosed fires from net/http's serve goroutine after the
// client observes the conn close, so the test has to give it a brief moment.
func waitForBoundaryCount(t *testing.T, opsURL, status string, want float64, d time.Duration) float64 {
	t.Helper()
	deadline := time.Now().Add(d)
	var got float64
	for time.Now().Before(deadline) {
		got = scrapeBoundaryCount(t, opsURL, status)
		if got >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return got
}

// newBoundaryTestServer wires up a data-plane server with a tight
// ReadHeaderTimeout suitable for the boundary tests, plus its companion ops
// listener for scraping. The data plane uses an unstarted httptest server
// whose underlying *http.Server is mutated to copy ReadHeaderTimeout AND
// receive the stdlib-boundary ConnState wiring, AND whose listener is wrapped
// so accepted conns become *sniffingConn — exactly the shape Run() uses in
// production.
func newBoundaryTestServer(t *testing.T, headerTimeout time.Duration, opts ...func(*http.Server)) (data, ops *httptest.Server, s *server.Server) {
	t.Helper()
	cfg := baseCfg("http://unused")
	cfg.ReadHeaderTimeout = headerTimeout
	s = server.New(cfg)
	s.SetBundle(loadBundle(t))

	dataSrv := httptest.NewUnstartedServer(s.DataHandler())
	dataSrv.Config.ReadHeaderTimeout = headerTimeout
	s.WireDataServer(dataSrv.Config)
	for _, opt := range opts {
		opt(dataSrv.Config)
	}
	dataSrv.Listener = s.WrapDataListener(dataSrv.Listener)
	dataSrv.Start()
	t.Cleanup(dataSrv.Close)

	opsSrv := httptest.NewServer(s.OpsHandler())
	t.Cleanup(opsSrv.Close)
	return dataSrv, opsSrv, s
}

func assertBoundaryEnvelope(t *testing.T, br *bufio.Reader, wantStatus int, wantCode string) {
	t.Helper()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read boundary response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d", resp.StatusCode, wantStatus)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
		t.Errorf("Content-Type = %q, want application/protobuf", got)
	}
	if got := resp.Header.Get("X-Wavefront-Error"); got != wantCode {
		t.Errorf("X-Wavefront-Error = %q, want %q", got, wantCode)
	}
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "unknown" {
		t.Errorf("X-Wavefront-Contract-Version = %q, want unknown", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read wire-error body: %v", err)
	}
	if got := wireErrorCode(t, body); got != wantCode {
		t.Errorf("wire-error body code = %q, want %q", got, wantCode)
	}
}

func wireErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			t.Fatalf("consume wire-error tag: %v", protowire.ParseError(n))
		}
		body = body[n:]
		if typ != protowire.BytesType {
			n = protowire.ConsumeFieldValue(num, typ, body)
			if n < 0 {
				t.Fatalf("consume wire-error field: %v", protowire.ParseError(n))
			}
			body = body[n:]
			continue
		}
		value, n := protowire.ConsumeBytes(body)
		if n < 0 {
			t.Fatalf("consume wire-error bytes: %v", protowire.ParseError(n))
		}
		body = body[n:]
		if num == 1 {
			return string(value)
		}
	}
	t.Fatal("wire-error body has no code field")
	return ""
}

// TestStdlibBoundarySlowHeaderReturns408Envelope verifies the listener emits
// a wire-error envelope even though net/http ordinarily closes this path
// without writing a response.
func TestStdlibBoundarySlowHeaderReturns408Envelope(t *testing.T) {
	const headerTimeout = 50 * time.Millisecond
	dataSrv, opsSrv, _ := newBoundaryTestServer(t, headerTimeout)

	before := scrapeBoundaryCount(t, opsSrv.URL, "408")

	u, err := url.Parse(dataSrv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Send a partial request line and no terminating headers. net/http's
	// request reader will time out; the listener replaces its silent close.
	if _, err := conn.Write([]byte("GET /v3/echo HTTP/1.1\r\n")); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	assertBoundaryEnvelope(t, bufio.NewReader(conn), http.StatusRequestTimeout, "request_header_timeout")

	// StateClosed fires from a goroutine after the conn close completes.
	// Poll briefly for the labelled counter to advance.
	got := waitForBoundaryCount(t, opsSrv.URL, "408", before+1, 2*time.Second)
	if got != before+1 {
		t.Fatalf(`wavefront_stdlib_boundary_total{status="408"} = %v, want %v (before=%v)`,
			got, before+1, before)
	}

	for _, status := range []string{"unknown", "431", "417", "400"} {
		if v := scrapeBoundaryCount(t, opsSrv.URL, status); v != 0 {
			t.Errorf(`wavefront_stdlib_boundary_total{status=%q} = %v, want 0 (slow-header should land on 408)`, status, v)
		}
	}
}

// TestStdlibBoundaryOversizedHeadersIncrements431 exercises the second of
// the three known stdlib-boundary status codes. net/http enforces a header
// limit in its request reader (errTooLarge) and writes a literal
// `HTTP/1.1 431 Request Header Fields Too Large\r\n...` to the conn before
// closing it (net/http/server.go, the `err == errTooLarge` case). The
// sniffing conn captures the `431` and the counter is labelled accordingly.
//
// MaxHeaderBytes on the http.Server controls the limit; the default is 1MB,
// so the test reduces it via an opts callback to keep the request small.
func TestStdlibBoundaryOversizedHeadersIncrements431(t *testing.T) {
	dataSrv, opsSrv, _ := newBoundaryTestServer(t, 5*time.Second, func(srv *http.Server) {
		// Tiny header budget so a single fat header overflows. net/http's
		// effective limit is MaxHeaderBytes + 4096 (bufio slop, see
		// initialReadLimitSize in net/http/server.go), so MaxHeaderBytes=1
		// gives an effective limit just over 4KB.
		srv.MaxHeaderBytes = 1
	})

	before := scrapeBoundaryCount(t, opsSrv.URL, "431")

	u, err := url.Parse(dataSrv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Build a request whose headers (well) exceed MaxHeaderBytes + bufio
	// slop. net/http applies the limit during readRequest; the response is
	// written to the conn directly via fmt.Fprintf(c.rwc, ...), which
	// lands in our sniffing conn's Write.
	fatHeader := strings.Repeat("a", 8192)
	req := fmt.Sprintf(
		"GET /v3/echo HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"X-Fat-Header: %s\r\n"+
			"\r\n",
		u.Host, fatHeader,
	)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write fat request: %v", err)
	}

	assertBoundaryEnvelope(t, bufio.NewReader(conn), http.StatusRequestHeaderFieldsTooLarge, "request_header_too_large")

	got := waitForBoundaryCount(t, opsSrv.URL, "431", before+1, 2*time.Second)
	if got != before+1 {
		t.Fatalf(`wavefront_stdlib_boundary_total{status="431"} = %v, want %v (before=%v)`,
			got, before+1, before)
	}
}

func TestStdlibBoundaryUnexpectedExpectReturns417Envelope(t *testing.T) {
	dataSrv, opsSrv, _ := newBoundaryTestServer(t, 5*time.Second)
	before := scrapeBoundaryCount(t, opsSrv.URL, "417")

	u, err := url.Parse(dataSrv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	req := fmt.Sprintf(
		"POST /v3/echo HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Content-Length: 1\r\n"+
			"Expect: no-thanks\r\n"+
			"Connection: close\r\n\r\n",
		u.Host,
	)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	assertBoundaryEnvelope(t, bufio.NewReader(conn), http.StatusExpectationFailed, "expectation_failed")

	got := waitForBoundaryCount(t, opsSrv.URL, "417", before+1, 2*time.Second)
	if got != before+1 {
		t.Fatalf(`wavefront_stdlib_boundary_total{status="417"} = %v, want %v (before=%v)`,
			got, before+1, before)
	}
}

// TestStdlibBoundaryKeepaliveSlowHeaderReturns408Envelope verifies the
// connection-local handler marker resets after a completed keepalive request.
func TestStdlibBoundaryKeepaliveSlowHeaderReturns408Envelope(t *testing.T) {
	const headerTimeout = 50 * time.Millisecond
	dataSrv, opsSrv, _ := newBoundaryTestServer(t, headerTimeout)
	before := scrapeBoundaryCount(t, opsSrv.URL, "408")

	u, err := url.Parse(dataSrv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /not-in-the-bundle HTTP/1.1\r\nHost: %s\r\n\r\n", u.Host); err != nil {
		t.Fatalf("write handled request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read handled response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("handled response status = %d, want 404", resp.StatusCode)
	}

	if _, err := conn.Write([]byte("GET /v3/echo HTTP/1.1\r\n")); err != nil {
		t.Fatalf("write partial keepalive request: %v", err)
	}
	assertBoundaryEnvelope(t, br, http.StatusRequestTimeout, "request_header_timeout")

	got := waitForBoundaryCount(t, opsSrv.URL, "408", before+1, 2*time.Second)
	if got != before+1 {
		t.Fatalf(`wavefront_stdlib_boundary_total{status="408"} = %v, want %v (before=%v)`,
			got, before+1, before)
	}
}

// TestStdlibBoundaryNormalRequestDoesNotIncrement is the negative complement:
// a healthy request that reaches the wavefront handler must NOT bump the
// counter under any label. The boundary metric is supposed to fire only
// when net/http handled the response itself.
func TestStdlibBoundaryNormalRequestDoesNotIncrement(t *testing.T) {
	// Long-enough ReadHeaderTimeout that the normal request can complete.
	dataSrv, opsSrv, _ := newBoundaryTestServer(t, 5*time.Second)

	// Snapshot every label that could conceivably be touched. "unknown" is
	// pre-seeded at registration, so its starting value may be > 0; the
	// others should be 0 but reading them defensively still works.
	statuses := []string{"unknown", "408", "431", "417", "400"}
	before := make(map[string]float64, len(statuses))
	for _, s := range statuses {
		before[s] = scrapeBoundaryCount(t, opsSrv.URL, s)
	}

	u, err := url.Parse(dataSrv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Send a complete request that wavefront WILL handle (it'll fail at
	// the bundle stage with no upstream wired, but that's a wavefront
	// envelope, not a stdlib-boundary response).
	req := fmt.Sprintf(
		"GET /v3/echo HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Connection: close\r\n"+
			"\r\n",
		u.Host,
	)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Wavefront DID handle this — there's an X-Wavefront-Contract-Version
	// header on the response. Sanity-check we got a wavefront-shaped
	// response.
	if got := resp.Header.Get("X-Wavefront-Contract-Version"); got == "" {
		t.Errorf("expected wavefront-shaped response (X-Wavefront-Contract-Version present); got headers %v", resp.Header)
	}

	// Give the conn close goroutine a moment, then verify NO label moved.
	time.Sleep(100 * time.Millisecond)
	for _, s := range statuses {
		if got := scrapeBoundaryCount(t, opsSrv.URL, s); got != before[s] {
			t.Fatalf(`wavefront_stdlib_boundary_total{status=%q} = %v, want %v (normal request must not bump any label)`,
				s, got, before[s])
		}
	}
}
