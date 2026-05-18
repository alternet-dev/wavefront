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

// forwardHeaders is the explicit allowlist passed to the upstream untouched:
// the bearer (wavefront is auth-transparent) and common tracing propagation.
// The client speaks protobuf to us; we do not relay arbitrary client headers.
var forwardHeaders = []string{
	"Authorization",
	"traceparent", "tracestate", "baggage",
	"X-Request-Id",
	"X-B3-TraceId", "X-B3-SpanId", "X-B3-ParentSpanId", "X-B3-Sampled", "X-B3-Flags",
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
	ureq.Header.Set("Content-Type", call.ContentType)
	for _, h := range forwardHeaders {
		for _, v := range r.Header.Values(h) {
			ureq.Header.Add(h, v)
		}
	}

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
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Wavefront-Contract-Version", c.ContractVersion())
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
	w.Header().Set("X-Wavefront-Error", werr.Code())
	cv := strings.TrimSpace(contractVersion)
	if cv == "" {
		cv = "unknown"
	}
	w.Header().Set("X-Wavefront-Contract-Version", cv)
	w.WriteHeader(werr.HTTPStatus())
	_, _ = w.Write(werr.ProtoBody())
}
