package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/negotiate"
	"github.com/alternet-dev/wavefront/internal/tracing"
	"github.com/alternet-dev/wavefront/internal/transform"
	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// Wavefront response header names used by the success path — re-exported
// from wireerror so the success and error paths cannot drift. The error
// path goes through wireerror.Write and never references these locally.
const (
	headerContentType     = wireerror.HeaderContentType
	headerContractVersion = wireerror.HeaderContractVersion
)

// versionUnknown is the contract_version label value (and response-header
// value) used when negotiation has not yet resolved a real version: a
// pre-negotiate error, or a client header that does not name a known
// contract. It pins metric cardinality and keeps the response header
// well-defined.
const versionUnknown = wireerror.VersionUnknown

// outcomeOK is the structured-log `outcome` value for a request that
// completed without a wavefront-originated failure.
const outcomeOK = "ok"

// hopByHop are the RFC 7230 §6.1 connection-scoped headers plus the
// framing/content headers net/http owns for the freshly-built upstream
// request. wavefront forwards every OTHER client header through untouched —
// it is an auth-transparent edge proxy, not a policy/authz engine; header
// policy, if a deployment wants it, belongs to the ingress, not here. These
// are withheld purely for HTTP correctness, never as policy: hop-by-hop must
// not cross a proxy boundary; Host/Content-Length/Transfer-Encoding are
// framing net/http manages; Content-Type is set to the codec's output type.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Host":                true,
	"Content-Length":      true,
	"Content-Type":        true,
}

// forwardClientHeaders copies every client header to the upstream request
// except hopByHop and any header the client itself named in Connection
// (RFC 7230 §6.1 — connection-scoped by the sender's declaration).
func forwardClientHeaders(dst, src http.Header) {
	skip := map[string]bool{}
	for _, c := range src["Connection"] {
		for _, tok := range strings.Split(c, ",") {
			if name := http.CanonicalHeaderKey(strings.TrimSpace(tok)); name != "" {
				skip[name] = true
			}
		}
	}
	for k, vs := range src {
		if hopByHop[k] || skip[k] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// hasRequestBody reports whether the inbound request presents bytes to the
// proxy. A declared positive Content-Length, or a chunked Transfer-Encoding,
// counts; an explicit zero-length body does not. This is what gates the 415
// envelope check: a request with no body has not declared any envelope, so
// the codec contract has nothing to enforce. net/http sets ContentLength to
// the parsed value when the client supplied a Content-Length header, to -1
// when the framing is chunked/unknown, and to 0 when the body is empty.
func hasRequestBody(r *http.Request) bool {
	if r.ContentLength > 0 {
		return true
	}
	if r.ContentLength < 0 {
		// Chunked or otherwise framed without a length — bytes are coming.
		return true
	}
	for _, te := range r.TransferEncoding {
		if te != "identity" && te != "" {
			return true
		}
	}
	return false
}

// isProtobufContentType reports whether v names the codec's protobuf media
// type. Media-type comparison is case-insensitive (RFC 9110 §8.3.1) and
// parameter-tolerant: `application/protobuf; charset=binary` is still the
// protobuf media type. An unparseable or absent value is rejected.
func isProtobufContentType(v string) bool {
	if v == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(v)
	if err != nil {
		return false
	}
	return strings.EqualFold(mt, wireerror.MediaTypeProtobuf)
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// One wavefront-attributed span per request, continuing the inbound W3C
	// traceparent when present. nil (tracing disabled / unsampled) is a safe
	// no-op for every Span method, so the request path is unchanged.
	span := s.tracer.StartSpan("wavefront.proxy", r.Header.Get("traceparent"))
	// version is the bundle-known contract version (or versionUnknown until
	// negotiate succeeds). It labels the metrics and the structured log line —
	// staying inside that bounded set keeps Prometheus cardinality finite.
	//
	// responseVersion is what we echo in X-Wavefront-Contract-Version. On the
	// success path it's the negotiated version; on a pre-negotiate error path
	// it's the raw client-supplied header value (or versionUnknown when the
	// client sent none) so the client can correlate the error with the value
	// it sent. Only the response header echoes the raw value — the metric and
	// log keep the bounded version to protect cardinality.
	version := versionUnknown
	responseVersion := versionUnknown
	if v := r.Header.Get(s.cfg.ContractVersionHeader); v != "" {
		responseVersion = v
	}
	var route, target string
	resolutionKind := ""
	outcome := outcomeOK
	upstreamStatus := 0
	transformOutcome := ""

	fail := func(werr *wireerror.Error, tout string) {
		outcome = werr.Code()
		transformOutcome = tout
		s.writeError(w, werr, version, responseVersion, tout)
	}

	defer func() {
		s.metrics.requests.WithLabelValues(version).Inc()
		level := slog.LevelInfo
		switch outcome {
		case outcomeOK:
			// already info
		case "transform_failed":
			level = slog.LevelError
		default:
			level = slog.LevelWarn
		}
		s.logger.LogAttrs(r.Context(), level, "proxy",
			slog.String("contract_version", version),
			slog.String("route", route),
			slog.String("target", target),
			slog.String("resolution_kind", resolutionKind),
			slog.Int("upstream_status", upstreamStatus),
			slog.String("outcome", outcome),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
		)

		span.SetAttr("contract_version", version)
		span.SetAttr("target", target)
		span.SetAttr("transform_outcome", transformOutcome)
		if outcome == outcomeOK {
			span.SetStatus(tracing.StatusOK)
		} else {
			span.SetStatus(tracing.StatusError)
		}
		span.End()
	}()

	b := s.bundle.Load()
	if b == nil {
		// The data listener accepted the request before the bundle finished
		// loading (the boot-time race), so there is no upstream to call yet
		// — this is a readiness condition, not an upstream failure. Issue
		// #39 extends the `unavailable` code (originally `/ready`-only) to
		// the proxy path so the data plane matches what the ops readiness
		// probe is already saying. Retry-After: 1 second is a short hint
		// suitable for the boot race; clients honouring it back off briefly
		// and retry.
		//
		// Graceful drain is handled implicitly by http.Server.Shutdown
		// (Run() in server.go): it stops accepting new connections and
		// waits for in-flight to complete within shutdownTimeout. No
		// explicit drain flag is wired here because the bundle pointer is
		// never cleared on shutdown, so accepted-but-pre-Load requests in
		// the drain window still find a live bundle — they finish
		// normally on the way out.
		fail(wireerror.Unavailable("bundle not loaded").WithRetryAfter("1"), "")
		return
	}

	// First-pass routing gate: the inbound (path, method) must match a
	// contract registered in the bundle at SOME version. The matched
	// contract is intentionally discarded — a multi-route bundle may bind
	// the same (path, method) at multiple contract_versions, and the
	// version-aware dispatch happens below in negotiate.Resolve once the
	// client's contract-version header is in scope. The gate's only job is
	// to fail-fast on an unbound (path, method) before any body read or
	// negotiation work; the metric label and structured log's
	// `contract_version` stay `versionUnknown` (bounded cardinality), while
	// the response header echoes the raw client value via `responseVersion`
	// so a caller can correlate the failure with what it sent. Wrong-method
	// folds into the same 404 (no 405, no `Allow` header) because each
	// contract names exactly one method and the bundle is the only routing
	// source of truth.
	if _, ok := b.LookupRoute(r.URL.Path, r.Method); !ok {
		fail(wireerror.UnknownRoute("no contract binds "+r.Method+" "+r.URL.Path), "")
		return
	}

	// Pre-decode envelope check: a request that carries a body must declare
	// the codec's media type. The v0.1 codec is `application/protobuf`
	// (wireerror.MediaTypeProtobuf); a wrong declaration is rejected here
	// with the 415 unsupported_media_type envelope rather than being folded
	// into the downstream 400 decode_failed. This keeps the two failure modes
	// distinct on the wire: 415 = wrong envelope, 400 = valid envelope whose
	// bytes don't parse.
	//
	// Ordering: AFTER the route gate (so unknown_route still wins on an
	// unmatched path) and BEFORE the body read/decode (so decode_failed stays
	// reserved for malformed protobuf inside a valid envelope).
	//
	// No-body rule: if there is no body, no envelope has been declared and
	// the check is a no-op. The simplest, safest rule is to only require a
	// Content-Type when the request actually presents bytes — a no-body
	// request that lacks Content-Type is accepted. A request that DOES carry
	// a body but lacks Content-Type is rejected (an empty string is not the
	// protobuf media type either, so the same comparison handles it). When a
	// future multi-codec selector lands (issue #44) this comparison becomes
	// table-driven against the negotiated codec.
	if hasRequestBody(r) {
		if !isProtobufContentType(r.Header.Get(headerContentType)) {
			fail(wireerror.UnsupportedMediaType("request Content-Type is not "+wireerror.MediaTypeProtobuf), "")
			return
		}
	}

	// Early Content-Length size check: if the client declared a length that
	// already exceeds MaxBodyBytes, reject with 413 BEFORE touching the body.
	// This matters for Expect: 100-continue (RFC 7231 §5.1.1): Go's
	// http.Server auto-emits "100 Continue" on the first body Read, so any
	// rejection that happens via MaxBytesReader (which trips inside Read)
	// would arrive after the client had already received the go-ahead.
	// Reading r.ContentLength here keeps us out of Read until the size
	// budget is known to be satisfiable. Chunked-encoded requests carry no
	// Content-Length (r.ContentLength == -1) and are still caught by the
	// MaxBytesReader mid-read — acceptable, because a client combining
	// chunked + Expect-100 has no ground to complain about wasted bandwidth
	// (it sent the body in pieces by its own choice).
	if r.ContentLength > 0 && r.ContentLength > s.cfg.MaxBodyBytes {
		fail(wireerror.RequestBodyTooLarge(""), "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(wireerror.RequestBodyTooLarge(""), "")
			return
		}
		fail(wireerror.DecodeFailed("could not read request body"), "")
		return
	}

	// Full (path, method, version) negotiation. The route gate above has
	// confirmed (path, method) matches SOME contract in the bundle; this
	// call adds the version filter, picking the one contract whose
	// (route, method, contract_version) matches all three. For a
	// multi-route bundle this is the only correct dispatch — Contract(v)
	// alone would return the first contract for that version in load
	// order, hiding the per-route binding. For a single-route bundle the
	// degenerate case (one contract per version) reduces to the same
	// behaviour with no special-casing.
	c, werr := negotiate.Resolve(b, r.URL.Path, r.Method, r.Header.Get(s.cfg.ContractVersionHeader))
	if werr != nil {
		fail(werr, "")
		return
	}
	// Negotiation resolved a real version: from here on, both the metric
	// label and the response header carry the same bundle-known value.
	version = c.ContractVersion()
	responseVersion = version
	route = c.Route()
	resolutionKind = describeResolution(c)

	call, werr := s.adapter.DecodeRequest(c, body)
	if werr != nil {
		fail(werr, "")
		return
	}

	chain := c.Chain()
	for _, link := range chain {
		call.Body, werr = transform.ApplyRequest(link.RequestOps(), call.Body)
		if werr != nil {
			fail(werr, "request")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	terminal := chain[len(chain)-1]
	target = terminal.Target()
	base, ok := s.cfg.TargetURL(target)
	if !ok {
		fail(wireerror.UpstreamError("unknown backend target "+target), "")
		return
	}
	url := strings.TrimRight(base, "/") + call.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery // verbatim query passthrough
	}
	ureq, err := http.NewRequestWithContext(ctx, call.Method, url, bytes.NewReader(call.Body))
	if err != nil {
		fail(wireerror.UpstreamError("could not build upstream request"), "")
		return
	}
	forwardClientHeaders(ureq.Header, r.Header)
	// A bodyless upstream call (synthetic Empty request_message) declares no
	// envelope: leave Content-Type unset so the upstream GET/path-only POST is
	// clean. Content-Type is hop-by-hop here, so no client value leaks through.
	if call.ContentType != "" {
		ureq.Header.Set(headerContentType, call.ContentType)
	}

	uresp, err := s.client.Do(ureq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			fail(wireerror.UpstreamTimeout(""), "")
			return
		}
		fail(wireerror.UpstreamError("upstream unreachable"), "")
		return
	}
	defer uresp.Body.Close()
	upstreamStatus = uresp.StatusCode

	upBody, err := io.ReadAll(uresp.Body)
	if err != nil {
		fail(wireerror.UpstreamError("could not read upstream response"), "")
		return
	}

	// HTTP status dispatch (issue #39 and #53): the upstream's status
	// determines how wavefront shapes the response.
	//
	//   200–203         → success: status preserved, body = encoded
	//                     response_message, success transforms run.
	//   204/205         → status preserved, NO body (wavefront must not
	//                     encode an empty response_message; the protocol
	//                     contract is that 204/205 carry no body, period).
	//   capability ceiling (206/207/208/226 + any 3xx)
	//                   → hard 502 upstream_error at every strictness,
	//                     regardless of any declared binding.
	//   declared (route, status) in error_messages
	//                   → first-class typed response: upstream status
	//                     preserved, body = bound message encoded from the
	//                     upstream JSON, NO X-Wavefront-Error, contract-
	//                     version header set. Declared-error bodies do not
	//                     run the 2xx-scoped ResponseOps.
	//   undeclared + strict:true
	//                   → hard 502 upstream_error.
	//   undeclared + non-strict (aid envelope)
	//                   → upstream status preserved,
	//                     X-Wavefront-Error: upstream_status, upstream body
	//                     relayed verbatim (UTF-8-coerced) in message. For
	//                     429 the upstream Retry-After is relayed.
	//
	// 404 and 422 are dual-origin: a 404 from wavefront's own route gate is
	// `unknown_route` (see the route lookup above); a 404 from the upstream
	// is `upstream_status` here (unless declared, in which case it is typed).
	// Likewise 422 is either `transform_failed` (if a request transform
	// stanza failed earlier) or typed/upstream_status (here). Clients
	// disambiguate via X-Wavefront-Error.
	switch {
	case uresp.StatusCode >= 200 && uresp.StatusCode <= 203:
		// Success — fall through to transform + encode below.
	case uresp.StatusCode == http.StatusNoContent || uresp.StatusCode == http.StatusResetContent:
		// 204/205 carry no body; encoding an empty response_message would
		// violate the protocol contract.
		w.Header().Set(headerContractVersion, version)
		w.WriteHeader(uresp.StatusCode)
		return
	case wireerror.IsCapabilityCeiling(uresp.StatusCode):
		// 206/207/208/226 + any 3xx: a wire feature wavefront does not model.
		// Hard 502 at every strictness, regardless of any declared binding.
		fail(wireerror.UpstreamError("upstream returned unsupported status "+strconv.Itoa(uresp.StatusCode)), "")
		return
	default:
		// Non-success, non-ceiling. A declared (route, status) is a first-class
		// typed contract response — status preserved, typed body, NO
		// X-Wavefront-Error, exactly like a 2xx. Declared-error bodies do not
		// run the 2xx-scoped ResponseOps (status-scoped error transforms are a
		// separate concern). An undeclared status falls to the aid envelope,
		// unless the contract is strict (hard 502).
		if _, declared := c.ErrorMessage(uresp.StatusCode); declared {
			out, ct, eerr := s.adapter.EncodeError(c, uresp.StatusCode, upBody)
			if eerr != nil {
				fail(eerr, "")
				return
			}
			if ct == "" {
				ct = wireerror.MediaTypeProtobuf
			}
			w.Header().Set(headerContentType, ct)
			w.Header().Set(headerContractVersion, version)
			w.WriteHeader(uresp.StatusCode)
			_, _ = w.Write(out)
			return
		}
		if c.Strict() {
			fail(wireerror.UpstreamError("upstream returned undeclared status "+strconv.Itoa(uresp.StatusCode)+" (strict)"), "")
			return
		}
		// Aid envelope: relay the upstream body verbatim as the message (the
		// envelope coerces it to valid UTF-8). Empty body falls back to the
		// default "upstream returned X" message via msgOr.
		werr := wireerror.UpstreamStatus(uresp.StatusCode, string(upBody))
		// For 429, relay the upstream's Retry-After verbatim if present.
		// WithRetryAfter("") is a no-op so we don't need to branch on
		// presence — and we never fabricate a Retry-After ourselves.
		if uresp.StatusCode == http.StatusTooManyRequests {
			werr = werr.WithRetryAfter(uresp.Header.Get("Retry-After"))
		}
		fail(werr, "")
		return
	}

	for i := len(chain) - 1; i >= 0; i-- {
		upBody, werr = transform.ApplyResponse(chain[i].ResponseOps(), upBody)
		if werr != nil {
			fail(werr, "response")
			return
		}
	}

	out, ct, werr := s.adapter.EncodeResponse(c, upBody)
	if werr != nil {
		fail(werr, "")
		return
	}
	w.Header().Set(headerContentType, ct)
	w.Header().Set(headerContractVersion, version)
	w.WriteHeader(uresp.StatusCode)
	_, _ = w.Write(out)
}

// describeResolution classifies a contract's resolution for the structured
// log line: "transform" if a `resolution.yaml` override has installed any
// transform stanzas or pointed at another version; otherwise "route".
func describeResolution(c *bundle.Contract) string {
	if len(c.RequestOps()) > 0 || len(c.ResponseOps()) > 0 || c.TransformTarget() != "" {
		return "transform"
	}
	return "route"
}

// writeError emits a wavefront-originated failure and increments the error
// counter. `metricVersion` is the bundle-known version (or versionUnknown)
// that labels the wavefront_errors_total counter — keeping it bounded
// protects Prometheus cardinality. `respVersion` is the value echoed in the
// X-Wavefront-Contract-Version response header: on pre-negotiate errors it
// carries the raw client-sent value (or versionUnknown when the client sent
// none) so a caller can correlate the failure with what it sent.
func (s *Server) writeError(w http.ResponseWriter, werr *wireerror.Error, metricVersion, respVersion, transformOutcome string) {
	s.metrics.errors.WithLabelValues(werr.Code(), metricVersion, transformOutcome).Inc()
	wireerror.Write(w, werr, respVersion)
}
