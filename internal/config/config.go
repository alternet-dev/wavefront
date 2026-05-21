// Package config parses the WAVEFRONT_* environment once at startup into an
// immutable Config. Required vars missing ⇒ MissingError; malformed values ⇒
// InvalidError (both name the offending var). Load uses the functional-options
// pattern: production calls config.Load(); tests inject a fixed environment
// with config.Load(config.WithLookup(fake)).
//
// Recognised environment variables:
//
//	WAVEFRONT_BUNDLE_PATH             — path to the bundle directory (required)
//	WAVEFRONT_UPSTREAM_BASE_URL       — default backend base URL (required)
//	WAVEFRONT_TARGETS                 — named backend targets, comma-separated name=url pairs
//	WAVEFRONT_LISTEN_ADDR             — data-plane listen address (default 0.0.0.0:8080)
//	WAVEFRONT_METRICS_ADDR            — ops/metrics listen address (default 0.0.0.0:9090)
//	WAVEFRONT_CONTRACT_VERSION_HEADER — header carrying the contract version (default X-Api-Contract-Version)
//	WAVEFRONT_REQUEST_TIMEOUT_MS      — per-request upstream timeout in ms (default 15000)
//	WAVEFRONT_MAX_BODY_BYTES          — maximum request body size in bytes (default 1048576)
//	WAVEFRONT_LOG_LEVEL               — log level: debug|info|warn|error (default info)
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	BundlePath            string
	UpstreamBaseURL       string
	Targets               map[string]string
	ListenAddr            string
	MetricsAddr           string
	ContractVersionHeader string
	RequestTimeout        time.Duration
	MaxBodyBytes          int64
	LogLevel              slog.Level
}

type MissingError struct{ Var string }

func (e *MissingError) Error() string { return "missing required env var: " + e.Var }

type InvalidError struct {
	Var   string
	Value string
	Err   error
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("invalid %s %q: %v", e.Var, e.Value, e.Err)
}

func (e *InvalidError) Unwrap() error { return e.Err }

const (
	defListenAddr  = "0.0.0.0:8080"
	defMetricsAddr = "0.0.0.0:9090"
	defCVHeader    = "X-Api-Contract-Version"
	defTimeoutMS   = 15000
	defMaxBody     = 1 << 20
)

var errPositive = errors.New("must be greater than zero")

// Lookuper resolves an environment variable: its value and whether it is set.
// It matches the signature of os.LookupEnv.
type Lookuper func(key string) (string, bool)

// Option configures Load.
type Option func(*loader)

// WithLookup overrides the environment source (default os.LookupEnv). Tests
// use it to supply a fixed, in-memory environment.
func WithLookup(fn Lookuper) Option {
	return func(l *loader) { l.lookup = fn }
}

type loader struct {
	lookup Lookuper
}

// Load parses the WAVEFRONT_* environment once into an immutable Config.
// Production: config.Load(). Tests: config.Load(config.WithLookup(fake)).
func Load(opts ...Option) (*Config, error) {
	l := &loader{lookup: os.LookupEnv}
	for _, o := range opts {
		o(l)
	}
	get := l.lookup

	nonBlank := func(k string) (string, bool) {
		v, ok := get(k)
		if !ok {
			return "", false
		}
		v = strings.TrimSpace(v)
		return v, v != ""
	}
	required := func(k string) (string, error) {
		if v, ok := nonBlank(k); ok {
			return v, nil
		}
		return "", &MissingError{Var: k}
	}
	withDefault := func(k, def string) string {
		if v, ok := nonBlank(k); ok {
			return v
		}
		return def
	}

	cfg := &Config{}
	var err error

	if cfg.BundlePath, err = required("WAVEFRONT_BUNDLE_PATH"); err != nil {
		return nil, err
	}
	if cfg.UpstreamBaseURL, err = required("WAVEFRONT_UPSTREAM_BASE_URL"); err != nil {
		return nil, err
	}
	if e := validateBaseURL(cfg.UpstreamBaseURL); e != nil {
		return nil, &InvalidError{Var: "WAVEFRONT_UPSTREAM_BASE_URL", Value: cfg.UpstreamBaseURL, Err: e}
	}

	cfg.Targets = map[string]string{}
	if raw, ok := nonBlank("WAVEFRONT_TARGETS"); ok {
		for _, pair := range strings.Split(raw, ",") {
			name, urlStr, found := strings.Cut(strings.TrimSpace(pair), "=")
			name, urlStr = strings.TrimSpace(name), strings.TrimSpace(urlStr)
			if !found || name == "" || urlStr == "" {
				return nil, &InvalidError{Var: "WAVEFRONT_TARGETS", Value: raw, Err: errors.New("each target must be name=url")}
			}
			if _, dup := cfg.Targets[name]; dup {
				return nil, &InvalidError{Var: "WAVEFRONT_TARGETS", Value: raw, Err: fmt.Errorf("duplicate target %q", name)}
			}
			if e := validateBaseURL(urlStr); e != nil {
				return nil, &InvalidError{Var: "WAVEFRONT_TARGETS", Value: urlStr, Err: e}
			}
			cfg.Targets[name] = urlStr
		}
	}

	cfg.ListenAddr = withDefault("WAVEFRONT_LISTEN_ADDR", defListenAddr)
	if e := validateHostPort(cfg.ListenAddr); e != nil {
		return nil, &InvalidError{Var: "WAVEFRONT_LISTEN_ADDR", Value: cfg.ListenAddr, Err: e}
	}
	cfg.MetricsAddr = withDefault("WAVEFRONT_METRICS_ADDR", defMetricsAddr)
	if e := validateHostPort(cfg.MetricsAddr); e != nil {
		return nil, &InvalidError{Var: "WAVEFRONT_METRICS_ADDR", Value: cfg.MetricsAddr, Err: e}
	}

	cfg.ContractVersionHeader = withDefault("WAVEFRONT_CONTRACT_VERSION_HEADER", defCVHeader)

	timeoutMS, e := parseIntVar(nonBlank, "WAVEFRONT_REQUEST_TIMEOUT_MS", defTimeoutMS)
	if e != nil {
		return nil, e
	}
	if timeoutMS <= 0 {
		return nil, &InvalidError{Var: "WAVEFRONT_REQUEST_TIMEOUT_MS", Value: strconv.Itoa(timeoutMS), Err: errPositive}
	}
	cfg.RequestTimeout = time.Duration(timeoutMS) * time.Millisecond

	maxBody, e := parseInt64Var(nonBlank, "WAVEFRONT_MAX_BODY_BYTES", defMaxBody)
	if e != nil {
		return nil, e
	}
	if maxBody <= 0 {
		return nil, &InvalidError{Var: "WAVEFRONT_MAX_BODY_BYTES", Value: strconv.FormatInt(maxBody, 10), Err: errPositive}
	}
	cfg.MaxBodyBytes = maxBody

	lvl, e := parseLevel(withDefault("WAVEFRONT_LOG_LEVEL", "info"))
	if e != nil {
		raw, _ := nonBlank("WAVEFRONT_LOG_LEVEL")
		return nil, &InvalidError{Var: "WAVEFRONT_LOG_LEVEL", Value: raw, Err: e}
	}
	cfg.LogLevel = lvl

	return cfg, nil
}

// TargetURL returns the base URL for a named backend target. The empty name
// resolves to the default target (UpstreamBaseURL). An unknown name returns
// ("", false).
func (c *Config) TargetURL(name string) (string, bool) {
	if name == "" {
		return c.UpstreamBaseURL, true
	}
	u, ok := c.Targets[name]
	return u, ok
}

func parseIntVar(nonBlank func(string) (string, bool), key string, def int) (int, error) {
	v, ok := nonBlank(key)
	if !ok {
		return def, nil
	}
	n, perr := strconv.Atoi(v)
	if perr != nil {
		return 0, &InvalidError{Var: key, Value: v, Err: perr}
	}
	return n, nil
}

func parseInt64Var(nonBlank func(string) (string, bool), key string, def int64) (int64, error) {
	v, ok := nonBlank(key)
	if !ok {
		return def, nil
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return 0, &InvalidError{Var: key, Value: v, Err: perr}
	}
	return n, nil
}

func validateBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

func validateHostPort(s string) error {
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return err
	}
	pn, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("port %q not numeric", port)
	}
	if pn < 1 || pn > 65535 {
		return fmt.Errorf("port %d out of range", pn)
	}
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}
