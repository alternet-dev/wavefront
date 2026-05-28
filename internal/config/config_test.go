package config

import (
	"errors"
	"log/slog"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := pairs[k]
		return v, ok
	}
}

func minimal() map[string]string {
	return map[string]string{
		"WAVEFRONT_BUNDLE_PATH":       "./bundle",
		"WAVEFRONT_UPSTREAM_BASE_URL": "http://localhost:9000",
	}
}

// load wires an in-memory env through the functional-options seam.
func load(pairs map[string]string) (*Config, error) {
	return Load(WithLookup(env(pairs)))
}

func TestLoadsWithMinimalRequiredEnv(t *testing.T) {
	cfg, err := load(minimal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BundlePath != "./bundle" {
		t.Errorf("BundlePath = %q", cfg.BundlePath)
	}
	if cfg.UpstreamBaseURL != "http://localhost:9000" {
		t.Errorf("UpstreamBaseURL = %q", cfg.UpstreamBaseURL)
	}
	if cfg.ListenAddr != "0.0.0.0:8080" {
		t.Errorf("ListenAddr default = %q", cfg.ListenAddr)
	}
	if cfg.MetricsAddr != "0.0.0.0:9090" {
		t.Errorf("MetricsAddr default = %q", cfg.MetricsAddr)
	}
	if cfg.ContractVersionHeader != "X-Api-Contract-Version" {
		t.Errorf("ContractVersionHeader default = %q", cfg.ContractVersionHeader)
	}
	if cfg.RequestTimeout != 15000*time.Millisecond {
		t.Errorf("RequestTimeout default = %v", cfg.RequestTimeout)
	}
	if cfg.ReadHeaderTimeout != 10000*time.Millisecond {
		t.Errorf("ReadHeaderTimeout default = %v", cfg.ReadHeaderTimeout)
	}
	if cfg.ReadTimeout != 30000*time.Millisecond {
		t.Errorf("ReadTimeout default = %v", cfg.ReadTimeout)
	}
	if cfg.MaxBodyBytes != 1048576 {
		t.Errorf("MaxBodyBytes default = %d", cfg.MaxBodyBytes)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel default = %v", cfg.LogLevel)
	}
}

func TestOverridesParse(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_LISTEN_ADDR"] = "127.0.0.1:1234"
	p["WAVEFRONT_METRICS_ADDR"] = "127.0.0.1:5678"
	p["WAVEFRONT_CONTRACT_VERSION_HEADER"] = "X-Contract"
	p["WAVEFRONT_REQUEST_TIMEOUT_MS"] = "3000"
	p["WAVEFRONT_READ_HEADER_TIMEOUT_MS"] = "4000"
	p["WAVEFRONT_READ_TIMEOUT_MS"] = "5000"
	p["WAVEFRONT_MAX_BODY_BYTES"] = "2048"
	p["WAVEFRONT_LOG_LEVEL"] = "debug"
	cfg, err := load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:1234" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.MetricsAddr != "127.0.0.1:5678" {
		t.Errorf("MetricsAddr = %q", cfg.MetricsAddr)
	}
	if cfg.ContractVersionHeader != "X-Contract" {
		t.Errorf("ContractVersionHeader = %q", cfg.ContractVersionHeader)
	}
	if cfg.RequestTimeout != 3*time.Second {
		t.Errorf("RequestTimeout = %v", cfg.RequestTimeout)
	}
	if cfg.ReadHeaderTimeout != 4*time.Second {
		t.Errorf("ReadHeaderTimeout = %v", cfg.ReadHeaderTimeout)
	}
	if cfg.ReadTimeout != 5*time.Second {
		t.Errorf("ReadTimeout = %v", cfg.ReadTimeout)
	}
	if cfg.MaxBodyBytes != 2048 {
		t.Errorf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
}

func TestMissingBundlePathIsError(t *testing.T) {
	p := minimal()
	delete(p, "WAVEFRONT_BUNDLE_PATH")
	_, err := load(p)
	var me *MissingError
	if !errors.As(err, &me) || me.Var != "WAVEFRONT_BUNDLE_PATH" {
		t.Fatalf("want MissingError(WAVEFRONT_BUNDLE_PATH), got %v", err)
	}
}

func TestMissingUpstreamBaseURLIsError(t *testing.T) {
	p := minimal()
	delete(p, "WAVEFRONT_UPSTREAM_BASE_URL")
	_, err := load(p)
	var me *MissingError
	if !errors.As(err, &me) || me.Var != "WAVEFRONT_UPSTREAM_BASE_URL" {
		t.Fatalf("want MissingError(WAVEFRONT_UPSTREAM_BASE_URL), got %v", err)
	}
}

func TestBlankRequiredIsMissing(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_BUNDLE_PATH"] = "   "
	_, err := load(p)
	var me *MissingError
	if !errors.As(err, &me) || me.Var != "WAVEFRONT_BUNDLE_PATH" {
		t.Fatalf("blank required var must be MissingError, got %v", err)
	}
}

func TestInvalidUpstreamBaseURLIsError(t *testing.T) {
	for _, bad := range []string{"not a url", "ftp://x", "/no-scheme", "http://"} {
		p := minimal()
		p["WAVEFRONT_UPSTREAM_BASE_URL"] = bad
		_, err := load(p)
		var ie *InvalidError
		if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_UPSTREAM_BASE_URL" {
			t.Errorf("%q: want InvalidError(WAVEFRONT_UPSTREAM_BASE_URL), got %v", bad, err)
		}
	}
}

func TestInvalidListenAddrIsError(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_LISTEN_ADDR"] = "no-port"
	_, err := load(p)
	var ie *InvalidError
	if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_LISTEN_ADDR" {
		t.Fatalf("want InvalidError(WAVEFRONT_LISTEN_ADDR), got %v", err)
	}
}

func TestInvalidMetricsAddrIsError(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_METRICS_ADDR"] = "bad:port"
	_, err := load(p)
	var ie *InvalidError
	if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_METRICS_ADDR" {
		t.Fatalf("want InvalidError(WAVEFRONT_METRICS_ADDR), got %v", err)
	}
}

func TestInvalidRequestTimeoutIsError(t *testing.T) {
	for _, bad := range []string{"soon", "0", "-5"} {
		p := minimal()
		p["WAVEFRONT_REQUEST_TIMEOUT_MS"] = bad
		_, err := load(p)
		var ie *InvalidError
		if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_REQUEST_TIMEOUT_MS" {
			t.Errorf("%q: want InvalidError(WAVEFRONT_REQUEST_TIMEOUT_MS), got %v", bad, err)
		}
	}
}

func TestInvalidReadHeaderTimeoutIsError(t *testing.T) {
	for _, bad := range []string{"soon", "0", "-5"} {
		p := minimal()
		p["WAVEFRONT_READ_HEADER_TIMEOUT_MS"] = bad
		_, err := load(p)
		var ie *InvalidError
		if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_READ_HEADER_TIMEOUT_MS" {
			t.Errorf("%q: want InvalidError(WAVEFRONT_READ_HEADER_TIMEOUT_MS), got %v", bad, err)
		}
	}
}

func TestInvalidReadTimeoutIsError(t *testing.T) {
	for _, bad := range []string{"soon", "0", "-5"} {
		p := minimal()
		p["WAVEFRONT_READ_TIMEOUT_MS"] = bad
		_, err := load(p)
		var ie *InvalidError
		if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_READ_TIMEOUT_MS" {
			t.Errorf("%q: want InvalidError(WAVEFRONT_READ_TIMEOUT_MS), got %v", bad, err)
		}
	}
}

func TestInvalidMaxBodyBytesIsError(t *testing.T) {
	for _, bad := range []string{"big", "0", "-1"} {
		p := minimal()
		p["WAVEFRONT_MAX_BODY_BYTES"] = bad
		_, err := load(p)
		var ie *InvalidError
		if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_MAX_BODY_BYTES" {
			t.Errorf("%q: want InvalidError(WAVEFRONT_MAX_BODY_BYTES), got %v", bad, err)
		}
	}
}

func TestInvalidLogLevelIsError(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_LOG_LEVEL"] = "verbose"
	_, err := load(p)
	var ie *InvalidError
	if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_LOG_LEVEL" {
		t.Fatalf("want InvalidError(WAVEFRONT_LOG_LEVEL), got %v", err)
	}
}

func TestLogLevelValuesMap(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"INFO":  slog.LevelInfo,
		"Warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for in, want := range cases {
		p := minimal()
		p["WAVEFRONT_LOG_LEVEL"] = in
		cfg, err := load(p)
		if err != nil {
			t.Fatalf("%q: unexpected error %v", in, err)
		}
		if cfg.LogLevel != want {
			t.Errorf("%q: LogLevel = %v, want %v", in, cfg.LogLevel, want)
		}
	}
}

func TestTargetsParsesValidPairs(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_TARGETS"] = "alpha=http://alpha:8080,beta=https://beta.example.com"
	cfg, err := load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("Targets len = %d, want 2", len(cfg.Targets))
	}
	if cfg.Targets["alpha"] != "http://alpha:8080" {
		t.Errorf("Targets[alpha] = %q", cfg.Targets["alpha"])
	}
	if cfg.Targets["beta"] != "https://beta.example.com" {
		t.Errorf("Targets[beta] = %q", cfg.Targets["beta"])
	}
}

func TestTargetsMalformedPairIsError(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_TARGETS"] = "noequals"
	_, err := load(p)
	var ie *InvalidError
	if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_TARGETS" {
		t.Fatalf("want InvalidError(WAVEFRONT_TARGETS), got %v", err)
	}
}

func TestTargetsDuplicateNameIsError(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_TARGETS"] = "alpha=http://alpha:8080,alpha=http://alpha2:8080"
	_, err := load(p)
	var ie *InvalidError
	if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_TARGETS" {
		t.Fatalf("want InvalidError(WAVEFRONT_TARGETS) for duplicate name, got %v", err)
	}
}

func TestTargetsBadURLIsError(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_TARGETS"] = "bad=ftp://not-http"
	_, err := load(p)
	var ie *InvalidError
	if !errors.As(err, &ie) || ie.Var != "WAVEFRONT_TARGETS" {
		t.Fatalf("want InvalidError(WAVEFRONT_TARGETS) for bad URL, got %v", err)
	}
}

func TestTargetURLEmptyNameReturnsDefault(t *testing.T) {
	p := minimal()
	cfg, err := load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := cfg.TargetURL("")
	if !ok || got != "http://localhost:9000" {
		t.Errorf("TargetURL(\"\") = (%q, %v), want (\"http://localhost:9000\", true)", got, ok)
	}
}

func TestTargetURLKnownNameReturnsURL(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_TARGETS"] = "svc=http://svc:1234"
	cfg, err := load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := cfg.TargetURL("svc")
	if !ok || got != "http://svc:1234" {
		t.Errorf("TargetURL(\"svc\") = (%q, %v), want (\"http://svc:1234\", true)", got, ok)
	}
}

func TestTargetURLUnknownNameReturnsFalse(t *testing.T) {
	p := minimal()
	cfg, err := load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := cfg.TargetURL("nonexistent")
	if ok || got != "" {
		t.Errorf("TargetURL(\"nonexistent\") = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestBlankOverrideFallsBackToDefault(t *testing.T) {
	p := minimal()
	p["WAVEFRONT_LISTEN_ADDR"] = ""
	p["WAVEFRONT_CONTRACT_VERSION_HEADER"] = "   "
	cfg, err := load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddr != "0.0.0.0:8080" {
		t.Errorf("blank ListenAddr should default, got %q", cfg.ListenAddr)
	}
	if cfg.ContractVersionHeader != "X-Api-Contract-Version" {
		t.Errorf("blank ContractVersionHeader should default, got %q", cfg.ContractVersionHeader)
	}
}
