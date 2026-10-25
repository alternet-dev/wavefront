package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// stdlib-boundary interception — issue #91.
//
// A small set of HTTP statuses are emitted by net/http's request reader
// BEFORE the wavefront handler runs: 408 (slow header), 431 (oversized
// headers), 417 (non-100-continue Expect). The data-plane listener preserves
// the stdlib parser and its limits, but replaces those replies with normal
// wavefront wire-error envelopes at the connection boundary. Because no
// http.Request exists for these replies, their contract version is unknown.
//
// State model (HTTP/1.1; wavefront is plaintext-only, no HTTP/2):
//
//   StateNew      — connection accepted; retain a passive entry so a
//                   zero-byte header timeout can be counted.
//   StateActive   — net/http read at least one byte. Set/reset the conn's
//                   entry with handlerRan=false. Reset on EVERY Active so
//                   keepalive boundary failures (request N succeeds, request
//                   N+1 fails the stdlib parser) are still observed.
//   StateIdle     — handler completed and keepalive is parked. Clear the
//                   connection-local handler marker for the next request.
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

// connEntry tracks per-connection stdlib-boundary state. Atomic fields keep
// ConnState and the request middleware race-free.
type connEntry struct {
	active     atomic.Bool
	handlerRan atomic.Bool
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
		// Keep a passive entry so a client that sends no bytes and times out
		// can still be observed as a 408. A client that only dials and drops
		// has neither an active entry nor a captured status and is ignored.
		s.connStateMap.Store(c, &connEntry{})
	case http.StateActive:
		// Set or reset the entry. Reset matters for keepalive: request N
		// may have run the handler (handlerRan=true), but request N+1 may
		// fail in net/http's parser before reaching the handler. Each
		// StateActive starts a fresh "did the handler run this round?"
		// observation.
		entry := &connEntry{}
		entry.active.Store(true)
		s.connStateMap.Store(c, entry)
	case http.StateIdle:
		// The next request on a keepalive connection begins after this state.
		// Clear the connection-local marker while retaining the entry for a
		// normal close after a successfully handled request.
		if sc, ok := c.(*sniffingConn); ok {
			sc.resetHandlerSeen()
		}
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
		if entry.handlerRan.Load() {
			return
		}
		status := boundaryStatusLabel(c)
		if entry.active.Load() || status != stdlibBoundaryStatusUnknown {
			// net/http handled this connection's last request before the
			// wavefront handler ran. Label the counter by the intercepted
			// status, or unknown if no response line was captured.
			s.metrics.stdlibBoundary.WithLabelValues(status).Inc()
		}
	}
}

// boundaryStatusLabel returns the label value for the stdlib-boundary counter
// based on the status that the sniffing conn captured on the first response
// write. Returns the literal "unknown" when c is not wrapped or no response
// status was captured.
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

// stdlibBoundaryStatusUnknown is the label for a boundary close without a
// captured response status.
const stdlibBoundaryStatusUnknown = "unknown"

// markHandlerSeen wraps the data-plane handler so the wavefront chain marks
// the connection's entry as "handler ran" the moment it enters. The ConnState
// callback then knows to count a StateClosed-without-handler as a stdlib
// boundary. The wrap sits OUTSIDE Recover so a recovered panic still counts
// as a handler run (the wavefront-shaped 500 envelope is not a stdlib boundary).
func (s *Server) markHandlerSeen(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := r.Context().Value(connContextKey{}).(net.Conn); ok {
			if sc, ok := c.(*sniffingConn); ok {
				sc.markHandlerSeen()
			}
			if raw, ok := s.connStateMap.Load(c); ok {
				if entry, ok := raw.(*connEntry); ok && entry != nil {
					entry.handlerRan.Store(true)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// markBoundaryEmitted makes a slow-header timeout on a keepalive connection
// observable even though net/http does not transition to StateActive until it
// has read request bytes. The response itself has already identified this as
// a fresh pre-handler failure, so replacing the prior request's marker is safe.
func (s *Server) markBoundaryEmitted(c net.Conn) {
	if raw, ok := s.connStateMap.Load(c); ok {
		if entry, ok := raw.(*connEntry); ok && entry != nil {
			entry.handlerRan.Store(false)
		}
	}
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
// conn is a *sniffingConn that intercepts the boundary response — is
// WrapDataListener. Run wires both; boundary tests wrap the
// httptest.Server.Listener before Start() so the same path is exercised.
func (s *Server) WireDataServer(srv *http.Server) {
	srv.ConnContext = s.connContext
	srv.ConnState = s.connState
}

// WrapDataListener returns l wrapped so every Accept produces a *sniffingConn.
// The wrap lets the StateClosed branch of connState read the intercepted
// status code (e.g. 431, 417) and label the
// `wavefront_stdlib_boundary_total` counter with it. The wrap MUST be in place
// before the conn is passed to ConnContext / ConnState: net/http accepts a
// conn, calls ConnContext with it, then setState(c.rwc, StateNew) — so the
// `net.Conn` that lands in the ConnState callback is whatever Accept returned.
func (s *Server) WrapDataListener(l net.Listener) net.Listener {
	return &sniffingListener{Listener: l, server: s}
}

// sniffingListener wraps a net.Listener so every Accept returns a *sniffingConn.
// The wrap is transparent to net/http: Addr and Close pass straight through.
type sniffingListener struct {
	net.Listener
	server *Server
}

func (l *sniffingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &sniffingConn{Conn: c, server: l.server}, nil
}

// sniffingConn intercepts replies emitted before wavefront's handler runs.
// It is otherwise a transparent net.Conn wrapper.
type sniffingConn struct {
	net.Conn
	server      *Server
	handlerSeen atomic.Bool
	status      atomic.Int32 // 0 if no response status has been captured
	emitted     atomic.Bool
}

func (c *sniffingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil && !c.handlerSeen.Load() && isTimeout(err) {
		// net/http closes a slow-header connection without writing a response.
		// Emit the envelope before returning its timeout error so the stdlib
		// parser keeps ownership of connection shutdown.
		_ = c.emitBoundary(http.StatusRequestTimeout)
	}
	return n, err
}

func (c *sniffingConn) Write(p []byte) (int, error) {
	status := parseStatusLine(p)
	if !c.handlerSeen.Load() && isInterceptedBoundaryStatus(status) {
		if err := c.emitBoundary(status); err != nil {
			return 0, err
		}
		// net/http will close this connection after its own text response. The
		// envelope already replaced that response on the underlying connection.
		return len(p), nil
	}
	if status != 0 {
		c.status.CompareAndSwap(0, int32(status))
	}
	return c.Conn.Write(p)
}

func (c *sniffingConn) markHandlerSeen() { c.handlerSeen.Store(true) }

func (c *sniffingConn) resetHandlerSeen() { c.handlerSeen.Store(false) }

func (c *sniffingConn) capturedStatus() int { return int(c.status.Load()) }

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isInterceptedBoundaryStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusExpectationFailed, http.StatusRequestHeaderFieldsTooLarge:
		return true
	}
	return false
}

func boundaryError(status int) *wireerror.Error {
	switch status {
	case http.StatusRequestTimeout:
		return wireerror.RequestHeaderTimeout("")
	case http.StatusExpectationFailed:
		return wireerror.ExpectationFailed("")
	case http.StatusRequestHeaderFieldsTooLarge:
		return wireerror.RequestHeaderTooLarge("")
	default:
		return nil
	}
}

// emitBoundary writes one complete envelope to the underlying connection. It
// bypasses c.Write to avoid re-entering the interception path.
func (c *sniffingConn) emitBoundary(status int) error {
	if !c.emitted.CompareAndSwap(false, true) {
		return nil
	}
	werr := boundaryError(status)
	if werr == nil {
		return nil
	}
	c.status.Store(int32(status))
	if c.server != nil {
		c.server.markBoundaryEmitted(c)
		c.server.metrics.errors.WithLabelValues(werr.Code(), versionUnknown, "").Inc()
	}
	body := werr.ProtoBody()
	header := werr.Headers()
	header.Set(wireerror.HeaderWavefrontError, werr.Code())
	header.Set(wireerror.HeaderContractVersion, wireerror.VersionUnknown)
	header.Set("Connection", "close")
	resp := &http.Response{
		StatusCode:    werr.HTTPStatus(),
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         true,
	}
	return resp.Write(c.Conn)
}

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
