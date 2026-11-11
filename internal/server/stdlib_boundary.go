package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// stdlib-boundary interception — issue #91.
//
// A small set of HTTP statuses can arise in net/http's request reader BEFORE
// the wavefront handler runs: 431 (oversized headers) and 417
// (non-100-continue Expect) are replies from net/http; 408 is synthesized by
// this listener when net/http closes a slow-header connection without a reply.
// The data-plane listener preserves the stdlib parser and its limits while
// replacing or synthesizing those replies as wavefront wire-error envelopes.
// Because no http.Request exists for these replies, their contract version is
// unknown.
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
//                   connection-local handler marker and response status, and
//                   mark the conn as awaiting the first bytes of its next
//                   request.
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
		// Confirm a fresh "did the handler run this round?" observation. A
		// keepalive probe can buffer a complete malformed request before this
		// callback fires, so sniffingConn starts the same observation as soon
		// as it sees that probe receive request bytes.
		s.markRequestStarted(c)
	case http.StateIdle:
		// The next request on a keepalive connection begins after this state.
		// Clear per-request state while retaining the entry for a normal close
		// after a successfully handled request.
		if sc, ok := c.(*sniffingConn); ok {
			sc.beginIdle()
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

// markRequestStarted records a fresh request observation before the handler
// can run. It is called by ConnState on a normal StateActive transition and by
// sniffingConn as soon as a keepalive probe proves that a next request began.
// The latter covers requests fully buffered by the probe: net/http can parse a
// malformed one without emitting StateActive during that parse.
func (s *Server) markRequestStarted(c net.Conn) {
	entry := &connEntry{}
	entry.active.Store(true)
	s.connStateMap.Store(c, entry)
}

// boundaryStatusLabel returns the label value for the stdlib-boundary counter
// based on the status that the sniffing conn captured while handling the
// current request. The status may come from a response write or directly from
// the synthesized 408 path. Returns the literal "unknown" when c is not
// wrapped or no response status was captured.
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
	server            *Server
	handlerSeen       atomic.Bool
	waitingForRequest atomic.Bool
	idleDeadlineSet   atomic.Bool
	pendingHeaderRead atomic.Bool
	status            atomic.Int32 // 0 if no response status has been captured
	emitted           atomic.Bool
	requestBytes      requestBytesTracker
}

func (c *sniffingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.requestBytes.observe(p[:n])
		// A successful idle probe means a new request has started. The next
		// timeout, if any, belongs to that request's header read rather than
		// the keepalive wait.
		c.requestStarted()
	}
	if err != nil && !c.handlerSeen.Load() && !c.waitingForRequest.Load() && isTimeout(err) {
		// net/http closes a slow-header connection without writing a response.
		// Emit the envelope before returning its timeout error so the stdlib
		// parser keeps ownership of connection shutdown.
		_ = c.emitBoundary(http.StatusRequestTimeout)
	}
	return n, err
}

// SetReadDeadline distinguishes net/http's first idle-probe deadline from the
// zero deadline it sets after Peek(4) has observed a new request. A prefix of
// one to three bytes may already be in net/http's bufio.Reader, so Peek waits
// for a fourth byte without calling Read. requestBytes detects that prefix and
// preserves a header deadline across the first zero-deadline call.
func (c *sniffingConn) SetReadDeadline(t time.Time) error {
	if c.waitingForRequest.Load() {
		if !c.idleDeadlineSet.Swap(true) {
			return c.Conn.SetReadDeadline(t)
		}
		if t.IsZero() {
			c.requestStarted()
		}
		return c.Conn.SetReadDeadline(t)
	}
	if c.pendingHeaderRead.Load() && !c.idleDeadlineSet.Swap(true) && t.IsZero() {
		if d := c.readHeaderTimeout(); d > 0 {
			return c.Conn.SetReadDeadline(time.Now().Add(d))
		}
	}
	return c.Conn.SetReadDeadline(t)
}

func (c *sniffingConn) readHeaderTimeout() time.Duration {
	if c.server == nil {
		return 0
	}
	if c.server.cfg.ReadHeaderTimeout > 0 {
		return c.server.cfg.ReadHeaderTimeout
	}
	return c.server.cfg.ReadTimeout
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

// beginIdle resets request-local state after a handled response and marks the
// connection's next read as an idle keepalive probe. A timeout during that
// probe is a normal close, not a 408. Read (or SetReadDeadline for buffered
// bytes) clears waitingForRequest once a new request begins.
func (c *sniffingConn) beginIdle() {
	c.handlerSeen.Store(false)
	c.status.Store(0)
	c.waitingForRequest.Store(true)
	c.idleDeadlineSet.Store(false)
	c.pendingHeaderRead.Store(false)
	if c.requestBytes.resetAndHasNextRequest() {
		// net/http's buffered reader already holds a prefix of the next
		// request. Its idle Peek(4) can block before reaching c.Read, so keep
		// a header deadline active until that probe either succeeds or times
		// out.
		c.pendingHeaderRead.Store(true)
		c.requestStarted()
	}
}

// requestStarted clears the idle-probe marker exactly once and starts a fresh
// ConnState observation. It may be called by Read after the probe obtains
// bytes, or by SetReadDeadline when the probe found bytes already buffered.
func (c *sniffingConn) requestStarted() {
	if !c.waitingForRequest.CompareAndSwap(true, false) {
		return
	}
	if c.server != nil {
		c.server.markRequestStarted(c)
	}
}

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
	werr := boundaryError(status)
	if werr == nil {
		return nil
	}
	if !c.emitted.CompareAndSwap(false, true) {
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

// requestBytesTracker recognizes the end of the request currently reaching
// the handler. It only needs to retain enough information to notice bytes for
// a following request that net/http has already read into its private buffer.
// Requests with chunked or malformed length metadata are intentionally not
// inferred: false positives would turn an ordinary idle close into a 408.
type requestBytesTracker struct {
	header        []byte
	bodyRemaining int64
	tracking      bool
	complete      bool
	nextRequest   bool
}

const maxTrackedRequestHeader = 1 << 20

func (t *requestBytesTracker) observe(p []byte) {
	if !t.tracking && !t.complete {
		t.tracking = true
		t.bodyRemaining = -1
	}
	for len(p) > 0 && t.tracking {
		if t.complete {
			t.nextRequest = true
			return
		}
		if t.bodyRemaining >= 0 {
			n := int64(len(p))
			if n > t.bodyRemaining {
				n = t.bodyRemaining
			}
			p = p[n:]
			t.bodyRemaining -= n
			if t.bodyRemaining == 0 {
				t.complete = true
			}
			continue
		}
		remaining := maxTrackedRequestHeader - len(t.header)
		if remaining <= 0 {
			t.tracking = false
			return
		}
		if len(p) > remaining {
			t.header = append(t.header, p[:remaining]...)
			t.tracking = false
			return
		}
		t.header = append(t.header, p...)
		p = nil
		end := bytes.Index(t.header, []byte("\r\n\r\n"))
		if end < 0 {
			return
		}
		body, ok := requestBodyLength(t.header[:end+4])
		if !ok {
			t.tracking = false
			return
		}
		p = t.header[end+4:]
		t.header = t.header[:end+4]
		t.bodyRemaining = body
		if body == 0 {
			t.complete = true
		}
	}
}

func (t *requestBytesTracker) resetAndHasNextRequest() bool {
	next := t.nextRequest
	*t = requestBytesTracker{tracking: true, bodyRemaining: -1}
	return next
}

func requestBodyLength(header []byte) (int64, bool) {
	lines := strings.Split(string(header), "\r\n")
	var length int64
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(name, "Transfer-Encoding") && strings.Contains(strings.ToLower(value), "chunked") {
			return 0, false
		}
		if !strings.EqualFold(name, "Content-Length") {
			continue
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || parsed < 0 || (length != 0 && length != parsed) {
			return 0, false
		}
		length = parsed
	}
	return length, true
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
