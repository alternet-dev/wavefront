package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

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
// This middleware is wired onto the data-plane handler only; ops/metrics
// (handled by OpsHandler) keep Go's default behavior so a fault in pprof
// or /metrics surfaces normally.
func (s *Server) Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			stack := debug.Stack()
			s.logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered in proxy handler",
				slog.Any("panic", rec),
				slog.String("stack", string(stack)),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			// Count the panic on the standard errors counter so operators see
			// it alongside other wavefront-originated failures.
			werr := wireerror.InternalError("")
			s.metrics.errors.WithLabelValues(werr.Code(), versionUnknown, "").Inc()
			wireerror.Write(w, werr, versionUnknown)
		}()
		next.ServeHTTP(w, r)
	})
}
