package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/alternet-dev/wavefront/internal/negotiate"
	"github.com/alternet-dev/wavefront/internal/wireerror"
)

// Wavefront response/extension header names — centralized so the success and
// error paths can't drift or typo them.
const (
	headerContentType     = "Content-Type"
	headerContractVersion = "X-Wavefront-Contract-Version"
	headerWavefrontError  = "X-Wavefront-Error"
)

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
	s.metrics.requests.Inc()
	requested := r.Header.Get(s.cfg.ContractVersionHeader)

	b := s.bundle.Load()
	if b == nil {
		s.writeError(w, wireerror.UpstreamError("no bundle loaded"), requested)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeError(w, wireerror.RequestBodyTooLarge(""), requested)
			return
		}
		s.writeError(w, wireerror.DecodeFailed("could not read request body"), requested)
		return
	}

	c, werr := negotiate.Resolve(b, requested)
	if werr != nil {
		s.writeError(w, werr, requested)
		return
	}

	call, werr := s.adapter.DecodeRequest(c, body)
	if werr != nil {
		s.writeError(w, werr, c.ContractVersion())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	url := strings.TrimRight(s.cfg.UpstreamBaseURL, "/") + call.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery // verbatim query passthrough
	}
	ureq, err := http.NewRequestWithContext(ctx, call.Method, url, bytes.NewReader(call.Body))
	if err != nil {
		s.writeError(w, wireerror.UpstreamError("could not build upstream request"), c.ContractVersion())
		return
	}
	forwardClientHeaders(ureq.Header, r.Header)
	ureq.Header.Set(headerContentType, call.ContentType)

	uresp, err := s.client.Do(ureq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			s.writeError(w, wireerror.UpstreamTimeout(""), c.ContractVersion())
			return
		}
		s.writeError(w, wireerror.UpstreamError("upstream unreachable"), c.ContractVersion())
		return
	}
	defer uresp.Body.Close()

	upBody, err := io.ReadAll(uresp.Body)
	if err != nil {
		s.writeError(w, wireerror.UpstreamError("could not read upstream response"), c.ContractVersion())
		return
	}
	if uresp.StatusCode < 200 || uresp.StatusCode >= 300 {
		s.writeError(w, wireerror.UpstreamError("upstream returned status "+strconv.Itoa(uresp.StatusCode)), c.ContractVersion())
		return
	}

	out, ct, werr := s.adapter.EncodeResponse(c, upBody)
	if werr != nil {
		s.writeError(w, werr, c.ContractVersion())
		return
	}
	w.Header().Set(headerContentType, ct)
	w.Header().Set(headerContractVersion, c.ContractVersion())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *Server) writeError(w http.ResponseWriter, werr *wireerror.Error, contractVersion string) {
	s.metrics.errors.WithLabelValues(werr.Code()).Inc()
	for k, vs := range werr.Headers() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set(headerWavefrontError, werr.Code())
	cv := strings.TrimSpace(contractVersion)
	if cv == "" {
		cv = "unknown"
	}
	w.Header().Set(headerContractVersion, cv)
	w.WriteHeader(werr.HTTPStatus())
	_, _ = w.Write(werr.ProtoBody())
}
