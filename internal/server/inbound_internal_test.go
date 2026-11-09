package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alternet-dev/wavefront/internal/config"
)

func TestInboundProtocolEmptyDefaultsToHTTP1(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	if got := s.inboundProtocol(); got != config.InboundHTTP1 {
		t.Errorf("inboundProtocol empty = %q, want %q", got, config.InboundHTTP1)
	}
}

func TestInboundProtocolExplicitPassthrough(t *testing.T) {
	for _, mode := range []config.InboundProtocol{
		config.InboundHTTP1,
		config.InboundH2C,
		config.InboundHTTPS,
		config.InboundHTTPSH3,
	} {
		s := &Server{cfg: &config.Config{InboundProtocol: mode}}
		if got := s.inboundProtocol(); got != mode {
			t.Errorf("inboundProtocol(%q) = %q, want %q", mode, got, mode)
		}
	}
}

func TestWithHTTP3AdvertisementSetsAltSvcHeader(t *testing.T) {
	noop := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {})
	h, err := withHTTP3Advertisement("127.0.0.1:8443", noop)
	if err != nil {
		t.Fatalf("withHTTP3Advertisement: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	want := `h3=":8443"; ma=2592000`
	if got := rec.Header().Get("Alt-Svc"); got != want {
		t.Errorf("Alt-Svc = %q, want %q", got, want)
	}
}

func TestWithHTTP3AdvertisementRejectsMalformedAddr(t *testing.T) {
	noop := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {})
	_, err := withHTTP3Advertisement("no-port-here", noop)
	if err == nil || !strings.Contains(err.Error(), "Alt-Svc") {
		t.Fatalf("withHTTP3Advertisement bad addr = %v, want Alt-Svc error", err)
	}
}

func TestProtocolsForInboundMode(t *testing.T) {
	tests := []struct {
		mode             config.InboundProtocol
		http1            bool
		http2            bool
		unencryptedHTTP2 bool
	}{
		{mode: config.InboundHTTP1, http1: true},
		{mode: config.InboundH2C, http1: true, unencryptedHTTP2: true},
		{mode: config.InboundHTTPS, http1: true, http2: true},
		{mode: config.InboundHTTPSH3, http1: true, http2: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			got := protocolsFor(tt.mode)
			if got.HTTP1() != tt.http1 {
				t.Errorf("HTTP1 = %v, want %v", got.HTTP1(), tt.http1)
			}
			if got.HTTP2() != tt.http2 {
				t.Errorf("HTTP2 = %v, want %v", got.HTTP2(), tt.http2)
			}
			if got.UnencryptedHTTP2() != tt.unencryptedHTTP2 {
				t.Errorf("UnencryptedHTTP2 = %v, want %v", got.UnencryptedHTTP2(), tt.unencryptedHTTP2)
			}
		})
	}
}
