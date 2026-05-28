package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// responseTracker wraps an http.ResponseWriter so the recover middleware can
// tell whether the handler already started a response before panicking.
// If it did, the recover block must NOT call wireerror.Write — doing so would
// trigger a "superfluous response.WriteHeader" warning, leave the original
// status on the wire, and append the error body to whatever the handler had
// already written, producing a malformed reply with the wrong
// X-Wavefront-Error header.
type responseTracker struct {
	http.ResponseWriter
	wrote bool
}

func (t *responseTracker) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *responseTracker) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

// Recover wraps an http.Handler so that any panic inside it is converted to
// a typed wavefront wire-error response — HTTP 500 with the
// `X-Wavefront-Error: internal_error` header and a `wavefront.v0.Error`
// protobuf body — instead of crashing the process or returning a
// connection reset to the client.
//
// The recovered panic value is recorded on the proxy's `errors` metric
// (code=internal_error, contract_version=unknown, transform_outcome="")
// and logged at Error level with the captured stack so the operator has
// the full forensic trail. The client only ever sees the fixed envelope —
// the panic value and the stack trace never reach the wire.
//
// Two cases are deliberately excluded from envelope emission:
//
//   - http.ErrAbortHandler — the stdlib's sentinel for "abort the connection
//     without writing a response or logging." Used by http.MaxBytesReader,
//     timeout handlers, etc. The contract requires re-panicking so the
//     http.Server's own handling takes over; we must not log or write.
//   - the handler already wrote a status or body before panicking — emitting
//     a wire-error envelope on top would corrupt the response. In that case
//     we still log and count, but skip the envelope.
//
// This middleware is wired onto the data-plane handler only; ops/metrics
// (handled by OpsHandler) keep Go's default behavior so a fault in pprof
// or /metrics surfaces normally.
func (s *Server) Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracker := &responseTracker{ResponseWriter: w}
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// stdlib contract: ErrAbortHandler means "abort the connection
			// silently." Re-panic so net/http's own machinery handles it —
			// don't log, don't count, don't write.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			stack := debug.Stack()
			s.logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered in proxy handler",
				slog.Any("panic", rec),
				slog.String("stack", string(stack)),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Bool("response_already_written", tracker.wrote),
			)
			// Count the panic on the standard errors counter so operators see
			// it alongside other wavefront-originated failures.
			werr := wireerror.InternalError("")
			s.metrics.errors.WithLabelValues(werr.Code(), versionUnknown, "").Inc()
			// If the handler already started writing, the response is
			// committed — emitting an envelope on top would corrupt it.
			// Log the situation and leave the wire alone.
			if tracker.wrote {
				s.logger.LogAttrs(r.Context(), slog.LevelError,
					"panic after response started; cannot emit wavefront.v0.Error envelope",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)
				return
			}
			wireerror.Write(tracker, werr, versionUnknown)
		}()
		next.ServeHTTP(tracker, r)
	})
}
