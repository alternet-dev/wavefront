package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"

	"github.com/alternet-dev/wavefront/internal/config"
)

const http3AltSvcMaxAge = 30 * 24 * 60 * 60

// inboundProtocol preserves HTTP/1.1 for embedders that construct Config
// directly and therefore leave the newer InboundProtocol field at zero.
func (s *Server) inboundProtocol() config.InboundProtocol {
	if s.cfg.InboundProtocol == "" {
		return config.InboundHTTP1
	}
	return s.cfg.InboundProtocol
}

func protocolsFor(mode config.InboundProtocol) *http.Protocols {
	p := new(http.Protocols)
	switch mode {
	case config.InboundH2C:
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
	case config.InboundHTTPS, config.InboundHTTPSH3:
		p.SetHTTP1(true)
		p.SetHTTP2(true)
	default:
		p.SetHTTP1(true)
	}
	return p
}

func (s *Server) newDataHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		IdleTimeout:       srvIdleTimeout,
		Protocols:         protocolsFor(s.inboundProtocol()),
		// WriteTimeout intentionally unset — see the timeout consts in server.go.
	}
}

func (s *Server) loadTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load inbound TLS certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// withHTTP3Advertisement tells HTTPS clients that HTTP/3 is available on the
// UDP port matching the configured TCP data-plane port. The operator must
// publish both protocols at that port.
func withHTTP3Advertisement(addr string, next http.Handler) (http.Handler, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("build HTTP/3 Alt-Svc header: %w", err)
	}
	value := fmt.Sprintf(`h3=":%s"; ma=%d`, port, http3AltSvcMaxAge)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", value)
		next.ServeHTTP(w, r)
	}), nil
}
