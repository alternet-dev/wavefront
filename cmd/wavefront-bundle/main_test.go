package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeSampleOpenAPI(t *testing.T) string {
	t.Helper()
	const doc = `{"openapi":"3.0.0","info":{"title":"t","version":"2026-05-17"},
"paths":{"/v3/echo":{"post":{
"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}},
"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}}}}}},
"components":{"schemas":{"Req":{"type":"object","properties":{"text":{"type":"string"}}}}}}`
	p := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	return p
}

func TestRunAddCreatesALayer(t *testing.T) {
	in := writeSampleOpenAPI(t)
	bundleDir := t.TempDir()
	code := run([]string{"add", "--openapi", in, "--bundle", bundleDir}, io.Discard)
	if code != 0 {
		t.Fatalf("run add: exit code %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "2026-05-17", "versions.yaml")); err != nil {
		t.Fatalf("layer not created: %v", err)
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	if code := run([]string{"frobnicate"}, io.Discard); code == 0 {
		t.Fatal("unknown subcommand: want non-zero exit code")
	}
}

func TestRunNoSubcommand(t *testing.T) {
	if code := run(nil, io.Discard); code == 0 {
		t.Fatal("no subcommand: want non-zero exit code")
	}
}

func TestRunRemoveDeletesTheOldestLayer(t *testing.T) {
	in := writeSampleOpenAPI(t)
	bundleDir := t.TempDir()
	if code := run([]string{"add", "--openapi", in, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("add: exit %d", code)
	}
	// a second, newer layer so the sample version is no longer the only one
	newer := filepath.Join(bundleDir, "2027-01")
	if err := os.MkdirAll(newer, 0o755); err != nil {
		t.Fatalf("mkdir newer layer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(newer, "versions.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatalf("write newer versions.yaml: %v", err)
	}
	code := run([]string{"remove", "--version", "2026-05-17", "--bundle", bundleDir}, io.Discard)
	if code != 0 {
		t.Fatalf("run remove: exit code %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "2026-05-17")); !os.IsNotExist(err) {
		t.Fatal("layer 2026-05-17 still present after remove")
	}
}

func TestRunRemoveRefusesANonOldestLayer(t *testing.T) {
	in := writeSampleOpenAPI(t)
	bundleDir := t.TempDir()
	if code := run([]string{"add", "--openapi", in, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("add: exit %d", code)
	}
	// an OLDER layer, so the sample version is not the oldest
	older := filepath.Join(bundleDir, "2020-01")
	if err := os.MkdirAll(older, 0o755); err != nil {
		t.Fatalf("mkdir older layer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(older, "versions.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatalf("write older versions.yaml: %v", err)
	}
	if code := run([]string{"remove", "--version", "2026-05-17", "--bundle", bundleDir}, io.Discard); code == 0 {
		t.Fatal("remove of a non-oldest layer: want non-zero exit code")
	}
}
