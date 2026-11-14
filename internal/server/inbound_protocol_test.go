package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/alternet-dev/wavefront/internal/config"
	"github.com/alternet-dev/wavefront/internal/server"
)

func TestInboundProtocolModesReachProxyPipeline(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)

	tests := []struct {
		name      string
		mode      config.InboundProtocol
		wantMajor int
		client    func(*x509.CertPool) (*http.Client, func())
	}{
		{
			name:      "http1 default",
			mode:      "",
			wantMajor: 1,
			client: func(_ *x509.CertPool) (*http.Client, func()) {
				tr := &http.Transport{}
				return &http.Client{Transport: tr}, tr.CloseIdleConnections
			},
		},
		{
			name:      "h2c",
			mode:      config.InboundH2C,
			wantMajor: 2,
			client: func(_ *x509.CertPool) (*http.Client, func()) {
				protocols := new(http.Protocols)
				protocols.SetUnencryptedHTTP2(true)
				tr := &http.Transport{Protocols: protocols}
				return &http.Client{Transport: tr}, tr.CloseIdleConnections
			},
		},
		{
			name:      "HTTP/2 over TLS",
			mode:      config.InboundHTTPS,
			wantMajor: 2,
			client: func(roots *x509.CertPool) (*http.Client, func()) {
				protocols := new(http.Protocols)
				protocols.SetHTTP2(true)
				tr := &http.Transport{
					Protocols: protocols,
					TLSClientConfig: &tls.Config{
						MinVersion: tls.VersionTLS12,
						RootCAs:    roots,
					},
				}
				return &http.Client{Transport: tr}, tr.CloseIdleConnections
			},
		},
		{
			name:      "HTTP/3",
			mode:      config.InboundHTTPSH3,
			wantMajor: 3,
			client: func(roots *x509.CertPool) (*http.Client, func()) {
				tr := &http3.Transport{TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS13,
					RootCAs:    roots,
				}}
				return &http.Client{Transport: tr}, func() { _ = tr.Close() }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := loadBundle(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"text":"pong"}`)
			}))
			defer upstream.Close()

			cfg := baseCfg(upstream.URL)
			cfg.ListenAddr = unusedTCPAddr(t)
			cfg.MetricsAddr = unusedTCPAddr(t)
			cfg.InboundProtocol = tt.mode
			if tt.mode.UsesTLS() {
				cfg.TLSCertFile = certFile
				cfg.TLSKeyFile = keyFile
			}

			s := server.New(cfg)
			s.SetBundle(b)
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- s.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-errCh:
					if err != nil {
						t.Errorf("server shutdown: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("server did not shut down")
				}
			})

			client, closeClient := tt.client(roots)
			defer closeClient()
			client.Timeout = 2 * time.Second
			scheme := "http"
			if tt.mode.UsesTLS() {
				scheme = "https"
			}
			url := scheme + "://" + cfg.ListenAddr + "/v3/echo"

			var resp *http.Response
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(pingBytes(t, b, "hi", 7)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/protobuf")
				req.Header.Set("X-Api-Contract-Version", "2024-11")
				resp, err = client.Do(req)
				if err == nil {
					break
				}
				select {
				case runErr := <-errCh:
					t.Fatalf("server exited before request: %v", runErr)
				default:
				}
				time.Sleep(20 * time.Millisecond)
			}
			if resp == nil {
				t.Fatal("protocol request did not connect before deadline")
			}
			defer resp.Body.Close()
			if resp.ProtoMajor != tt.wantMajor {
				t.Errorf("response protocol = %s, want major version %d", resp.Proto, tt.wantMajor)
			}
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200; body=%q", resp.StatusCode, body)
			}
			if got := resp.Header.Get("X-Wavefront-Contract-Version"); got != "2024-11" {
				t.Errorf("X-Wavefront-Contract-Version = %q, want 2024-11", got)
			}
			if tt.mode == config.InboundHTTPSH3 {
				_, port, _ := net.SplitHostPort(cfg.ListenAddr)
				want := fmt.Sprintf(`h3=":%s"; ma=2592000`, port)
				if got := resp.Header.Get("Alt-Svc"); got != want {
					t.Errorf("Alt-Svc = %q, want %q", got, want)
				}
			}
		})
	}
}

func TestTLSModeRejectsInvalidCertificateBeforeListening(t *testing.T) {
	cfg := baseCfg("http://unused")
	cfg.ListenAddr = unusedTCPAddr(t)
	cfg.MetricsAddr = unusedTCPAddr(t)
	cfg.InboundProtocol = config.InboundHTTPS
	cfg.TLSCertFile = filepath.Join(t.TempDir(), "missing.crt")
	cfg.TLSKeyFile = filepath.Join(t.TempDir(), "missing.key")

	err := server.New(cfg).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "load inbound TLS certificate") {
		t.Fatalf("Run error = %v, want TLS certificate load failure", err)
	}
}

func TestRunRejectsInvalidInboundProtocol(t *testing.T) {
	cfg := baseCfg("http://unused")
	cfg.InboundProtocol = "spdy"
	err := server.New(cfg).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unsupported inbound protocol") {
		t.Fatalf("Run error = %v, want unsupported inbound protocol", err)
	}
}

func unusedTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release TCP address: %v", err)
	}
	return addr
}

func writeTestCertificate(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(cert)
	return certFile, keyFile, roots
}
