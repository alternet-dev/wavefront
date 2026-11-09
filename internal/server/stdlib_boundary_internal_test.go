package server

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// nullConn is a no-op net.Conn used by the sniffing-conn unit tests. Writes
// succeed without touching anything; reads return EOF; everything else is a
// trivial stub. The sniffing-conn tests only exercise Write, so the rest is
// there to satisfy the interface.
type nullConn struct{}

func (nullConn) Read(_ []byte) (int, error)         { return 0, net.ErrClosed }
func (nullConn) Write(p []byte) (int, error)        { return len(p), nil }
func (nullConn) Close() error                       { return nil }
func (nullConn) LocalAddr() net.Addr                { return nullAddr{} }
func (nullConn) RemoteAddr() net.Addr               { return nullAddr{} }
func (nullConn) SetDeadline(_ time.Time) error      { return nil }
func (nullConn) SetReadDeadline(_ time.Time) error  { return nil }
func (nullConn) SetWriteDeadline(_ time.Time) error { return nil }

type nullAddr struct{}

func (nullAddr) Network() string { return "null" }
func (nullAddr) String() string  { return "null" }

// TestParseStatusLine pins parseStatusLine's behaviour on the kinds of byte
// prefixes net/http actually writes (real status lines) and on the kinds of
// follow-on writes the function must NOT misinterpret as a new status line.
// The connection wrapper parses each write, so status-line inputs are fresh
// response prefixes and negative cases cover follow-on bytes that must not be
// mistaken for a response line.
func TestParseStatusLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"200 minimal", "HTTP/1.1 200 OK\r\n", 200},
		{"404 with reason", "HTTP/1.1 404 Not Found\r\n", 404},
		{"500 with reason", "HTTP/1.1 500 Internal Server Error\r\n", 500},
		{"431 net/http real", "HTTP/1.1 431 Request Header Fields Too Large\r\n", 431},
		{"417 net/http real", "HTTP/1.1 417 Expectation Failed\r\nConnection: close\r\n\r\n", 417},
		{"HTTP/1.0 still parsed", "HTTP/1.0 200 OK\r\n", 200},

		// Negative cases: each must return 0.
		{"empty", "", 0},
		{"body bytes", "hello world\r\n", 0},
		{"close to a status line but wrong prefix", "HTTX/1.1 200 OK\r\n", 0},
		{"too short", "HTTP/1.1 20", 0},
		{"no space after version", "HTTP/1.1\t200 OK\r\n", 0},
		{"non-digit in code", "HTTP/1.1 2O0 OK\r\n", 0}, // letter O instead of zero
		{"only two digits before space", "HTTP/1.1 20 OK\r\n", 0},
		{"junk that happens to be 13 bytes", strings.Repeat("x", 13), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStatusLine([]byte(tc.in))
			if got != tc.want {
				t.Fatalf("parseStatusLine(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestSniffingConnWriteCapturesOnce confirms that the first response status is
// retained and subsequent response lines cannot overwrite it. This keeps the
// metric label stable if a future net/http refactor splits a response across
// multiple writes.
func TestSniffingConnWriteCapturesOnce(t *testing.T) {
	c := &sniffingConn{Conn: &nullConn{}}
	if _, err := c.Write([]byte("HTTP/1.1 418 I'm a teapot\r\n")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if got := c.capturedStatus(); got != 418 {
		t.Fatalf("after first write: capturedStatus = %d, want 418", got)
	}
	// A second write — looks like another response line — must NOT
	// overwrite the captured status.
	if _, err := c.Write([]byte("HTTP/1.1 500 Internal Server Error\r\n")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := c.capturedStatus(); got != 418 {
		t.Fatalf("after second write: capturedStatus = %d, want 418 (first status must remain)", got)
	}
	// A non-status follow-on (body bytes) similarly must not reset to 0.
	if _, err := c.Write([]byte("teapot body bytes")); err != nil {
		t.Fatalf("third write: %v", err)
	}
	if got := c.capturedStatus(); got != 418 {
		t.Fatalf("after third write: capturedStatus = %d, want 418", got)
	}
}

// TestSniffingConnWriteEmptyDoesNotCaptureStatus confirms a zero-length Write
// does not lock out a subsequent real status-line write.
func TestSniffingConnWriteEmptyDoesNotCaptureStatus(t *testing.T) {
	c := &sniffingConn{Conn: &nullConn{}}
	if _, err := c.Write(nil); err != nil {
		t.Fatalf("nil write: %v", err)
	}
	if _, err := c.Write([]byte{}); err != nil {
		t.Fatalf("empty write: %v", err)
	}
	if got := c.capturedStatus(); got != 0 {
		t.Fatalf("after empty writes: capturedStatus = %d, want 0", got)
	}
	if _, err := c.Write([]byte("HTTP/1.1 200 OK\r\n")); err != nil {
		t.Fatalf("real write: %v", err)
	}
	if got := c.capturedStatus(); got != 200 {
		t.Fatalf("after real write: capturedStatus = %d, want 200", got)
	}
}

// TestIsInterceptedBoundaryStatus confirms that exactly 408, 417, and 431 are
// classified as stdlib-boundary statuses and that no other code is.
func TestIsInterceptedBoundaryStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusRequestTimeout, true},              // 408
		{http.StatusExpectationFailed, true},           // 417
		{http.StatusRequestHeaderFieldsTooLarge, true}, // 431
		// Non-boundary codes that must not be intercepted.
		{http.StatusOK, false},
		{http.StatusBadRequest, false},
		{http.StatusNotFound, false},
		{http.StatusInternalServerError, false},
		{0, false},
	}
	for _, tc := range cases {
		if got := isInterceptedBoundaryStatus(tc.status); got != tc.want {
			t.Errorf("isInterceptedBoundaryStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// TestBoundaryError confirms boundaryError returns the right wire-error shape
// for each intercepted status and nil for any status that is not a known
// stdlib boundary.
func TestBoundaryError(t *testing.T) {
	cases := []struct {
		status   int
		wantCode string
		wantNil  bool
	}{
		{http.StatusRequestTimeout, "request_header_timeout", false},
		{http.StatusExpectationFailed, "expectation_failed", false},
		{http.StatusRequestHeaderFieldsTooLarge, "request_header_too_large", false},
		// Non-boundary statuses must return nil so emitBoundary is a no-op.
		{http.StatusBadRequest, "", true},
		{http.StatusInternalServerError, "", true},
		{0, "", true},
	}
	for _, tc := range cases {
		got := boundaryError(tc.status)
		if tc.wantNil {
			if got != nil {
				t.Errorf("boundaryError(%d) = %v, want nil", tc.status, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("boundaryError(%d) = nil, want code %q", tc.status, tc.wantCode)
			continue
		}
		if got.Code() != tc.wantCode {
			t.Errorf("boundaryError(%d).Code() = %q, want %q", tc.status, got.Code(), tc.wantCode)
		}
		if got.HTTPStatus() != tc.status {
			t.Errorf("boundaryError(%d).HTTPStatus() = %d, want %d", tc.status, got.HTTPStatus(), tc.status)
		}
	}
}

// bufConn is a net.Conn whose Write goes to a bytes.Buffer so emitBoundary
// can be tested without a real network connection.
type bufConn struct {
	nullConn
	buf bytes.Buffer
}

func (c *bufConn) Write(p []byte) (int, error) { return c.buf.Write(p) }

// TestSniffingConnEmitBoundaryIdempotent verifies that the emitted flag
// prevents a second emitBoundary call from writing a second envelope, even
// when both calls reference the same boundary status.
func TestSniffingConnEmitBoundaryIdempotent(t *testing.T) {
	bc := &bufConn{}
	c := &sniffingConn{Conn: bc}
	if err := c.emitBoundary(http.StatusRequestTimeout); err != nil {
		t.Fatalf("first emitBoundary: %v", err)
	}
	firstLen := bc.buf.Len()
	if firstLen == 0 {
		t.Fatal("first emitBoundary wrote nothing to the connection")
	}
	// Second call with the same status must be a no-op.
	if err := c.emitBoundary(http.StatusRequestTimeout); err != nil {
		t.Fatalf("second emitBoundary: %v", err)
	}
	if bc.buf.Len() != firstLen {
		t.Fatalf("second emitBoundary wrote %d extra bytes, want 0 (must be idempotent)",
			bc.buf.Len()-firstLen)
	}
}

// TestSniffingConnEmitBoundaryUnknownStatusIsNoop confirms that emitBoundary
// for a status that boundaryError does not recognize writes nothing and returns
// nil — not a half-formed envelope.
func TestSniffingConnEmitBoundaryUnknownStatusIsNoop(t *testing.T) {
	bc := &bufConn{}
	c := &sniffingConn{Conn: bc}
	if err := c.emitBoundary(http.StatusBadRequest); err != nil {
		t.Fatalf("emitBoundary(400): %v", err)
	}
	if bc.buf.Len() != 0 {
		t.Fatalf("emitBoundary(400) wrote %d bytes, want 0", bc.buf.Len())
	}
}

// TestSniffingConnWriteInterceptsBoundaryStatusBeforeHandler verifies that
// Write emits a wire-error envelope — not the raw stdlib text — when the
// handler has not yet claimed the connection and the status is an intercepted
// boundary code. The underlying conn must receive the envelope bytes, the
// original p bytes must not appear there, and the method must return
// len(p), nil so net/http's error path continues normally.
func TestSniffingConnWriteInterceptsBoundaryStatusBeforeHandler(t *testing.T) {
	bc := &bufConn{}
	c := &sniffingConn{Conn: bc}

	raw := []byte("HTTP/1.1 431 Request Header Fields Too Large\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n431\n")
	n, err := c.Write(raw)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(raw) {
		t.Fatalf("Write n = %d, want %d", n, len(raw))
	}

	written := bc.buf.Bytes()
	if len(written) == 0 {
		t.Fatal("underlying conn received nothing; expected a wire-error envelope")
	}
	// The envelope carries application/protobuf, not text/plain.
	if !bytes.Contains(written, []byte("application/protobuf")) {
		t.Errorf("envelope missing application/protobuf Content-Type; got:\n%s", written)
	}
	// The raw stdlib body must not have leaked through.
	if bytes.Contains(written, []byte("text/plain")) {
		t.Errorf("stdlib Content-Type leaked to underlying conn; raw bytes must not pass through")
	}
	if c.capturedStatus() != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("capturedStatus = %d, want 431", c.capturedStatus())
	}
}

// TestSniffingConnWritePassthroughAfterHandlerSeen confirms that Write does
// not intercept boundary status bytes once the handler has claimed the
// connection. The original bytes must reach the underlying conn unchanged.
func TestSniffingConnWritePassthroughAfterHandlerSeen(t *testing.T) {
	bc := &bufConn{}
	c := &sniffingConn{Conn: bc}
	c.handlerSeen.Store(true)

	raw := []byte("HTTP/1.1 431 Request Header Fields Too Large\r\nContent-Type: text/plain\r\n\r\n")
	n, err := c.Write(raw)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(raw) {
		t.Fatalf("Write n = %d, want %d", n, len(raw))
	}
	if !bytes.Equal(bc.buf.Bytes(), raw) {
		t.Fatalf("passthrough mismatch:\n got %q\nwant %q", bc.buf.Bytes(), raw)
	}
	// Status is still captured even when passing through.
	if c.capturedStatus() != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("capturedStatus = %d, want 431", c.capturedStatus())
	}
}

// timeoutErr is a net.Error that reports Timeout() == true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

// timeoutConn wraps nullConn and returns a timeoutErr on the first Read.
type timeoutConn struct {
	nullConn
	buf bufConn
}

func (c *timeoutConn) Read(_ []byte) (int, error) { return 0, timeoutErr{} }
func (c *timeoutConn) Write(p []byte) (int, error) { return c.buf.buf.Write(p) }

// TestSniffingConnReadEmitsBoundaryOnTimeout verifies that Read calls
// emitBoundary(408) when the underlying conn returns a timeout error and the
// handler has not yet been seen. The 408 wire-error envelope must be written
// to the underlying conn; the timeout error must still be returned to the
// caller so net/http can close the connection normally.
func TestSniffingConnReadEmitsBoundaryOnTimeout(t *testing.T) {
	tc := &timeoutConn{}
	c := &sniffingConn{Conn: tc}

	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if n != 0 {
		t.Fatalf("Read n = %d, want 0", n)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Read err = %v, want a timeout net.Error", err)
	}

	written := tc.buf.buf.Bytes()
	if len(written) == 0 {
		t.Fatal("underlying conn received nothing; expected a 408 wire-error envelope")
	}
	if !bytes.Contains(written, []byte("408")) {
		t.Errorf("408 status missing from emitted envelope:\n%s", written)
	}
	if !bytes.Contains(written, []byte("application/protobuf")) {
		t.Errorf("application/protobuf Content-Type missing from emitted envelope:\n%s", written)
	}
	if c.capturedStatus() != http.StatusRequestTimeout {
		t.Fatalf("capturedStatus = %d, want 408", c.capturedStatus())
	}
}

// TestSniffingConnReadNoEmitWhenHandlerSeen confirms that Read does NOT call
// emitBoundary when the handler has already been seen, even on a timeout.
func TestSniffingConnReadNoEmitWhenHandlerSeen(t *testing.T) {
	tc := &timeoutConn{}
	c := &sniffingConn{Conn: tc}
	c.handlerSeen.Store(true)

	buf := make([]byte, 64)
	_, err := c.Read(buf)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Read err = %v, want a timeout net.Error", err)
	}

	if tc.buf.buf.Len() != 0 {
		t.Fatalf("unexpected envelope written to underlying conn after handler-seen timeout: %d bytes", tc.buf.buf.Len())
	}
	// No status should be captured on the Read path.
	if c.capturedStatus() != 0 {
		t.Fatalf("capturedStatus = %d, want 0 (read path does not capture status)", c.capturedStatus())
	}
}
