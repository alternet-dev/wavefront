package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/negotiate"
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

func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
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

	fail := func(werr *wireerror.Error, transformOutcome string) {
		outcome = werr.Code()
		s.writeError(w, werr, version, responseVersion, transformOutcome)
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
	}()

	b := s.bundle.Load()
	if b == nil {
		fail(wireerror.UpstreamError("no bundle loaded"), "")
		return
	}

	// First-pass routing gate: the inbound (path, method) must match a
	// contract registered in the bundle. If it does not — unknown path, or
	// known path with a wrong method — emit the unknown_route 404 envelope
	// and stop. The check runs before negotiation: we have not yet validated
	// the contract-version header, so the metric label and the structured
	// log's `contract_version` stay `versionUnknown` (cardinality stays
	// bounded). The response header, however, echoes the raw client-sent
	// value via `responseVersion` so the caller can correlate the failure
	// with what it sent; absent any client header, it falls back to
	// `unknown`. Wrong-method folds into the same 404 (no 405, no `Allow`
	// header) because each contract names exactly one method and the
	// bundle is the only routing source of truth.
	if _, ok := b.LookupRoute(r.URL.Path, r.Method); !ok {
		fail(wireerror.UnknownRoute("no contract binds "+r.Method+" "+r.URL.Path), "")
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

	c, werr := negotiate.Resolve(b, r.Header.Get(s.cfg.ContractVersionHeader))
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
	ureq.Header.Set(headerContentType, call.ContentType)

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
	if uresp.StatusCode < 200 || uresp.StatusCode >= 300 {
		fail(wireerror.UpstreamError("upstream returned status "+strconv.Itoa(uresp.StatusCode)), "")
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
	w.WriteHeader(http.StatusOK)
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
