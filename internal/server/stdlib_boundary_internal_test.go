package server

import (
	"net"
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
