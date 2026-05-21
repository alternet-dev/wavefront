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
