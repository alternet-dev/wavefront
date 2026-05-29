package server

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strconv"
	"sync"
)

// stdlib-boundary observability — issue #91.
//
// A small set of HTTP statuses are emitted by net/http's request reader
// BEFORE the wavefront handler runs: 408 (slow header), 431 (oversized
// headers), 417 (non-100-continue Expect). These responses are
// `Content-Type: text/plain`, carry no `X-Wavefront-Error` header, and are
// documented as out of the wavefront wire-error envelope per issue #39's
// status-code matrix. Intercepting them to replace them with the wavefront
// envelope would require reimplementing parts of net/http's request reader —
// tracked as future work in issue #91.
//
// Rather than replacing the responses, this surfaces them on the `/metrics`
// endpoint via `wavefront_stdlib_boundary_total{status=...}`. Detection is
// structural: a connection that transitions to StateActive (net/http read
// bytes off the wire) and then to StateClosed WITHOUT the wavefront handler
// ever running is a stdlib-boundary failure. The status code is captured by
// a wrapped net.Conn that sniffs the first response-line write on the way to
// the wire; net/http writes status lines (e.g. for 431 and 417) directly to
// `c.rwc` via `fmt.Fprintf`, which lands in `sniffingConn.Write`. When net/http
// closes the connection without writing a response — as it does on the
// slow-header path — the captured status stays zero and the metric records
// the literal `"unknown"`.
//
// State model (HTTP/1.1; wavefront is plaintext-only, no HTTP/2):
//
//   StateNew      — connection accepted; no entry yet (keeps the map small
//                   when a client connects and drops without sending bytes).
//   StateActive   — net/http read at least one byte. Set/reset the conn's
//                   entry with handlerRan=false. Reset on EVERY Active so
//                   keepalive boundary failures (request N succeeds, request
//                   N+1 fails the stdlib parser) are still observed.
//   StateIdle     — handler completed and keepalive is parked. No-op: the
//                   entry stays so a later Active→Closed without a handler
//                   run can still be observed; the next Active resets it.
//   StateHijacked — terminal per net/http docs. Delete the entry so the map
//                   does not leak even though hijack is not a wavefront code
//                   path today.
//   StateClosed   — terminal. If an entry exists AND handlerRan is false,
//                   net/http handled this connection's last (or only) request
//                   itself; increment the counter. Delete the entry in all
//                   cases.
//
// The middleware sets handlerRan=true the moment the wavefront chain enters
// the conn's request. It sits OUTSIDE Recover so a panic earlier in our chain
// is still counted as a handler run (the recovered envelope is a
// wavefront-shaped 500, not a stdlib-boundary response).

// connContextKey is the context key under which the data-plane http.Server's
// ConnContext callback stashes the net.Conn so the markHandlerSeen middleware
// can find it. Using a typed key (rather than a bare string) avoids collisions
// with any other context.WithValue keys downstream.
type connContextKey struct{}

// connEntry tracks per-connection observability state for the stdlib-boundary
// detection. The zero value is `handlerRan=false`, which is the correct
// initial state set by StateActive.
type connEntry struct {
	handlerRan bool
}

// connContext attaches the net.Conn to the connection-scoped context so the
// per-request middleware can look it up and mark the conn as "handler ran".
// Wired as `http.Server.ConnContext` on the data plane only.
func (s *Server) connContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connContextKey{}, c)
}

// connState is the data-plane `http.Server.ConnState` callback. See the file
// header for the detection model.
func (s *Server) connState(c net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		// No-op: skipping StateNew keeps a "client dialed then dropped"
		// connection out of the map, since net/http will transition it
		// directly to StateClosed without a StateActive in between.
	case http.StateActive:
		// Set or reset the entry. Reset matters for keepalive: request N
		// may have run the handler (handlerRan=true), but request N+1 may
		// fail in net/http's parser before reaching the handler. Each
		// StateActive starts a fresh "did the handler run this round?"
		// observation.
		entry := &connEntry{}
		s.connStateMap.Store(c, entry)
	case http.StateIdle:
		// No-op: keep the existing entry. The next StateActive will reset
		// it, and the next StateClosed will check + delete it.
	case http.StateHijacked:
		// Terminal per net/http docs ("does not transition to StateClosed").
		// Delete the entry so the map does not leak.
		s.connStateMap.Delete(c)
	case http.StateClosed:
		raw, ok := s.connStateMap.LoadAndDelete(c)
		if !ok {
			// No entry — either the conn went StateNew→StateClosed
			// without bytes (client dialed and dropped), or some
			// state we never observed. No-op.
			return
		}
		entry, ok := raw.(*connEntry)
		if !ok || entry == nil {
			return
		}
		if !entry.handlerRan {
			// StateActive fired (we have an entry) but the wavefront
			// handler never ran on this conn's current request — that
			// is the stdlib-boundary failure mode. Label by the status
			// code the sniffing conn captured off the wire; fall back
			// to "unknown" when net/http closed without writing a
			// response line (slow-header path).
			s.metrics.stdlibBoundary.WithLabelValues(boundaryStatusLabel(c)).Inc()
		}
	}
}

// boundaryStatusLabel returns the label value for the stdlib-boundary counter
// based on the status that the sniffing conn captured on the first response
// write. Returns the literal "unknown" when c is not wrapped (defensive — Run
// and tests always wrap), or when the conn closed without a response (the
// net/http slow-header path returns from `serve()` without writing).
func boundaryStatusLabel(c net.Conn) string {
	sc, ok := c.(*sniffingConn)
	if !ok {
		return stdlibBoundaryStatusUnknown
	}
	status := sc.capturedStatus()
	if status == 0 {
		return stdlibBoundaryStatusUnknown
	}
	return strconv.Itoa(status)
}

// stdlibBoundaryStatusUnknown is the `status` label value used when the
// sniffing conn did not capture a response line — most commonly because
// net/http closed the connection on the slow-header path without writing.
const stdlibBoundaryStatusUnknown = "unknown"

// markHandlerSeen wraps the data-plane handler so the wavefront chain marks
// the connection's entry as "handler ran" the moment it enters. The ConnState
// callback then knows to count a StateClosed-without-handler as a stdlib
// boundary. The wrap sits OUTSIDE Recover so a recovered panic still counts
// as a handler run (the wavefront-shaped 500 envelope is not a stdlib boundary).
func (s *Server) markHandlerSeen(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := r.Context().Value(connContextKey{}).(net.Conn); ok {
			if raw, ok := s.connStateMap.Load(c); ok {
				if entry, ok := raw.(*connEntry); ok && entry != nil {
					entry.handlerRan = true
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// WireDataServer attaches the data-plane connection-state callbacks
// (ConnContext + ConnState) to srv so the stdlib-boundary observability is in
// effect. Run() wires this on the production listener; tests that exercise
// the boundary detection wire it on an httptest.NewUnstartedServer's Config
// before Start(). The ops listener must NOT receive this wiring — only the
// data plane.
//
// WireDataServer covers the half of the observability that lives on the
// *http.Server. The other half — wrapping the listener so every accepted
// conn is a *sniffingConn that captures the first response status line —
// is WrapDataListener. Run wires both; boundary tests wrap the
// httptest.Server.Listener before Start() so the same path is exercised.
func (s *Server) WireDataServer(srv *http.Server) {
	srv.ConnContext = s.connContext
	srv.ConnState = s.connState
}

// WrapDataListener returns l wrapped so every Accept produces a *sniffingConn.
// The wrap lets the StateClosed branch of connState read the status code
// net/http wrote on the wire (e.g. 431, 417) and label the
// `wavefront_stdlib_boundary_total` counter with it. The wrap MUST be in place
// before the conn is passed to ConnContext / ConnState: net/http accepts a
// conn, calls ConnContext with it, then setState(c.rwc, StateNew) — so the
// `net.Conn` that lands in the ConnState callback is whatever Accept returned.
func (s *Server) WrapDataListener(l net.Listener) net.Listener {
	return &sniffingListener{Listener: l}
}

// sniffingListener wraps a net.Listener so every Accept returns a *sniffingConn.
// The wrap is transparent to net/http: Addr and Close pass straight through.
type sniffingListener struct {
	net.Listener
}

func (l *sniffingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &sniffingConn{Conn: c}, nil
}

// sniffingConn captures the status code from the first Write that begins with
// an HTTP/1.x response line. The capture is "first write only" — sync.Once
// guards against later body-byte writes accidentally re-parsing. Reads are
// untouched; Close is untouched.
//
// Concurrency: net/http writes to a conn from the goroutine running serve()
// for that conn, and StateClosed fires from that same goroutine after the
// conn close completes (`setState(c.rwc, StateClosed, ...)` in
// net/http/server.go). The sync.Once + read-then-return in capturedStatus is
// race-free under that contract; the Once is still belt-and-suspenders, since
// nothing in the wavefront pipeline writes to the bare conn from another
// goroutine.
type sniffingConn struct {
	net.Conn
	statusOnce sync.Once
	status     int // 0 if not captured; set under statusOnce
}

func (c *sniffingConn) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.statusOnce.Do(func() {
			c.status = parseStatusLine(p)
		})
	}
	return c.Conn.Write(p)
}

// capturedStatus returns the parsed status code, or 0 if none was captured.
// The first-write capture is gated by statusOnce; reading after StateClosed
// is safe because by then no further write can fire the Once.
func (c *sniffingConn) capturedStatus() int { return c.status }

// parseStatusLine extracts the numeric status code from the start of an
// HTTP/1.x response line, of the form "HTTP/<version> <code> <reason>\r\n".
// Returns 0 when p does not begin with "HTTP/", does not have enough bytes
// for "HTTP/x.x NNN", or has non-digits where the status code should be.
// This is intentionally permissive on HTTP version digits (so 1.0 or 1.1 are
// both fine) and strict on the three digits that follow the first space.
func parseStatusLine(p []byte) int {
	// Minimum prefix "HTTP/1.1 200" is 12 bytes; we also need the byte after
	// the three digits to exist as either a space or a CR for the line to be
	// well-formed, so require 13.
	const minLen = len("HTTP/1.1 200 ")
	if len(p) < minLen || !bytes.HasPrefix(p, []byte("HTTP/")) {
		return 0
	}
	sp := bytes.IndexByte(p, ' ')
	if sp < 0 || sp+3 >= len(p) {
		return 0
	}
	digits := p[sp+1 : sp+4]
	n := 0
	for _, b := range digits {
		if b < '0' || b > '9' {
			return 0
		}
		n = n*10 + int(b-'0')
	}
	return n
}

// connStateMap (declared on *Server in server.go) holds the per-connection
// observability state. sync.Map is appropriate because:
//   - reads/writes are independent per-key (no cross-key invariants);
//   - StateClosed and the middleware both touch the same key, and the
//     ConnState callback may fire from a different goroutine than the one
//     that ran the handler;
//   - the keyset is unbounded (one entry per live connection) but every
//     terminal state (Closed / Hijacked) deletes the entry, so the map's
//     working set tracks the live-connection count.
