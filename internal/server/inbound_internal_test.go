package server

import (
	"testing"

	"github.com/alternet-dev/wavefront/internal/config"
)

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
