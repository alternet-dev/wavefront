package e2e

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alternet-dev/wavefront/internal/bundle"
	"github.com/alternet-dev/wavefront/internal/bundletest"
	"github.com/alternet-dev/wavefront/internal/config"
	"github.com/alternet-dev/wavefront/internal/server"
)

func benchmarkRequest(client *http.Client, url string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url+"/v3/echo", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", "2024-11")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func BenchmarkProxyRequestPath(b *testing.B) {
	loaded, err := bundle.Load(bundletest.Dir(b, ""))
	if err != nil {
		b.Fatalf("load bundle: %v", err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"benchmark","at":"2026-10-18T00:00:00Z"}`)
	}))
	b.Cleanup(backend.Close)

	cfg := &config.Config{
		BundlePath:            "benchmark",
		UpstreamBaseURL:       backend.URL,
		ContractVersionHeader: "X-Api-Contract-Version",
		RequestTimeout:        2 * time.Second,
		MaxBodyBytes:          1 << 20,
	}
	proxyServer := server.New(cfg)
	proxyServer.SetBundle(loaded)
	proxyServer.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy := httptest.NewServer(proxyServer.DataHandler())
	b.Cleanup(proxy.Close)

	requestBody := PingBytes(b, loaded, "benchmark", 42)
	client := proxy.Client()

	b.Run("latency", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(requestBody)))
		for b.Loop() {
			if err := benchmarkRequest(client, proxy.URL, requestBody); err != nil {
				b.Fatalf("proxy request: %v", err)
			}
		}
	})

	b.Run("throughput", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(requestBody)))
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if err := benchmarkRequest(client, proxy.URL, requestBody); err != nil {
					b.Errorf("proxy request: %v", err)
					return
				}
			}
		})
	})
}
