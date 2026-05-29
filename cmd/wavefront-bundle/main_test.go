package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestRunAddRefusesReAddWithoutForce confirms the default behaviour: a
// second add against the same info.version is a hard error.
func TestRunAddRefusesReAddWithoutForce(t *testing.T) {
	in := writeSampleOpenAPI(t)
	bundleDir := t.TempDir()
	if code := run([]string{"add", "--openapi", in, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("first add: exit %d, want 0", code)
	}
	if code := run([]string{"add", "--openapi", in, "--bundle", bundleDir}, io.Discard); code == 0 {
		t.Fatal("second add without --force: want non-zero exit code")
	}
}

// writeSampleOpenAPIExtraRoute writes a same-info.version OpenAPI with an
// extra route so a re-add with --force visibly changes the layer contents.
func writeSampleOpenAPIExtraRoute(t *testing.T) string {
	t.Helper()
	const doc = `{"openapi":"3.0.0","info":{"title":"t","version":"2026-05-17"},
"paths":{
"/v3/echo":{"post":{
"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}},
"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}}}}},
"/v3/ping":{"post":{
"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}},
"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}}}}}
},
"components":{"schemas":{"Req":{"type":"object","properties":{"text":{"type":"string"}}}}}}`
	p := filepath.Join(t.TempDir(), "openapi-extra.json")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatalf("write openapi: %v", err)
	}
	return p
}

// TestRunAddForceOverwritesExistingLayer drives the CLI: emit a layer,
// re-run with --force against a same-info.version OpenAPI that carries a
// new route, and verify the layer dir now reflects the new content.
func TestRunAddForceOverwritesExistingLayer(t *testing.T) {
	in1 := writeSampleOpenAPI(t)
	in2 := writeSampleOpenAPIExtraRoute(t)
	bundleDir := t.TempDir()
	if code := run([]string{"add", "--openapi", in1, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("first add: exit %d, want 0", code)
	}
	versionsPath := filepath.Join(bundleDir, "2026-05-17", "versions.yaml")
	pre, err := os.ReadFile(versionsPath)
	if err != nil {
		t.Fatalf("read versions.yaml after first add: %v", err)
	}
	if strings.Contains(string(pre), "/v3/ping") {
		t.Fatalf("pre-condition violated: first add already contains /v3/ping:\n%s", pre)
	}

	if code := run([]string{"add", "--force", "--openapi", in2, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("add --force: exit %d, want 0", code)
	}
	post, err := os.ReadFile(versionsPath)
	if err != nil {
		t.Fatalf("read versions.yaml after force re-add: %v", err)
	}
	if !strings.Contains(string(post), "/v3/ping") {
		t.Errorf("force re-add did not replace the layer; /v3/ping missing from versions.yaml:\n%s", post)
	}
	if !strings.Contains(string(post), "/v3/echo") {
		t.Errorf("force re-add lost the original route /v3/echo:\n%s", post)
	}
}

// TestRunAddForceOnFreshBundle confirms --force is a no-op when there is
// no existing layer to overwrite: the add succeeds like a regular one.
func TestRunAddForceOnFreshBundle(t *testing.T) {
	in := writeSampleOpenAPI(t)
	bundleDir := t.TempDir()
	code := run([]string{"add", "--force", "--openapi", in, "--bundle", bundleDir}, io.Discard)
	if code != 0 {
		t.Fatalf("add --force on a fresh bundle: exit %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "2026-05-17", "versions.yaml")); err != nil {
		t.Fatalf("force-add on a fresh bundle did not produce a layer: %v", err)
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

// --- retire subcommand ---

func TestRunRetireScaffoldsAnOverride(t *testing.T) {
	in := writeSampleOpenAPI(t)
	bundleDir := t.TempDir()
	if code := run([]string{"add", "--openapi", in, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("add: exit %d", code)
	}
	// add a newer layer so the sample is not the current version
	newer := filepath.Join(bundleDir, "2027-01")
	if err := os.MkdirAll(newer, 0o755); err != nil {
		t.Fatalf("mkdir newer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(newer, "versions.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatalf("write versions.yaml: %v", err)
	}
	code := run([]string{"retire", "--version", "2026-05-17", "--bundle", bundleDir}, io.Discard)
	if code != 0 {
		t.Fatalf("retire: exit code %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(bundleDir, "resolution.yaml")); err != nil {
		t.Fatalf("resolution.yaml not created: %v", err)
	}
}

func TestRunRetireMissingFlags(t *testing.T) {
	// Missing --version: must return exit 2.
	bundleDir := t.TempDir()
	if code := run([]string{"retire", "--bundle", bundleDir}, io.Discard); code != 2 {
		t.Fatalf("retire without --version: exit %d, want 2", code)
	}
	// Missing --bundle: must return exit 2.
	if code := run([]string{"retire", "--version", "2024-01"}, io.Discard); code != 2 {
		t.Fatalf("retire without --bundle: exit %d, want 2", code)
	}
}

func TestRunRetireRefusesCurrentVersion(t *testing.T) {
	bundleDir := t.TempDir()
	// single layer — that version is the current; retire should fail
	layer := filepath.Join(bundleDir, "2026-05-17")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatalf("mkdir layer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layer, "versions.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatalf("write versions.yaml: %v", err)
	}
	if code := run([]string{"retire", "--version", "2026-05-17", "--bundle", bundleDir}, io.Discard); code == 0 {
		t.Fatal("retire of the only/current layer: want non-zero exit code")
	}
}

// --- verify subcommand ---

func TestRunVerifySucceedsOnGoodBundle(t *testing.T) {
	in := writeSampleOpenAPI(t)
	bundleDir := t.TempDir()
	if code := run([]string{"add", "--openapi", in, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("add: exit %d", code)
	}
	if code := run([]string{"verify", "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("verify good bundle: exit %d, want 0", code)
	}
}

func TestRunVerifyFailsOnBrokenBundle(t *testing.T) {
	in := writeSampleOpenAPI(t)
	bundleDir := t.TempDir()
	if code := run([]string{"add", "--openapi", in, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("add: exit %d", code)
	}
	// Write a resolution.yaml that names a nonexistent version.
	res := []byte(`version: 1
overrides:
  - contract_version: "9999-99"
    transform:
      target: "2026-05-17"
      request: []
      response: []
`)
	if err := os.WriteFile(filepath.Join(bundleDir, "resolution.yaml"), res, 0o644); err != nil {
		t.Fatalf("write resolution.yaml: %v", err)
	}
	if code := run([]string{"verify", "--bundle", bundleDir}, io.Discard); code == 0 {
		t.Fatal("verify on a broken bundle: want non-zero exit code")
	}
}

func TestRunVerifyMissingFlag(t *testing.T) {
	if code := run([]string{"verify"}, io.Discard); code != 2 {
		t.Fatalf("verify without --bundle: exit %d, want 2", code)
	}
}

// --- draft-shim subcommand ---

func writeDriftPair(t *testing.T) (bundleDir, fromVer, toVer string) {
	t.Helper()
	bundleDir = t.TempDir()
	fromVer = "2024-01"
	toVer = "2024-06"
	for _, layer := range []struct{ name, body string }{
		{fromVer, `{"openapi":"3.0.0","info":{"title":"t","version":"` + fromVer + `"},
"paths":{"/v/echo":{"post":{
"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}},
"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Resp"}}}}}}}},
"components":{"schemas":{
"Req":{"type":"object","properties":{"userName":{"type":"string"}}},
"Resp":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}}`},
		{toVer, `{"openapi":"3.0.0","info":{"title":"t","version":"` + toVer + `"},
"paths":{"/v/echo":{"post":{
"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}},
"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Resp"}}}}}}}},
"components":{"schemas":{
"Req":{"type":"object","properties":{"user_name":{"type":"string"}}},
"Resp":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}}`},
	} {
		dir := filepath.Join(bundleDir, layer.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "openapi.json"), []byte(layer.body), 0o600); err != nil {
			t.Fatalf("write %s/openapi.json: %v", dir, err)
		}
	}
	return bundleDir, fromVer, toVer
}

func TestRunDraftShimEmitsYAMLToStdout(t *testing.T) {
	bundleDir, fromVer, toVer := writeDriftPair(t)
	var out bytes.Buffer
	code := runDraftShim(
		[]string{"--bundle", bundleDir, "--from", fromVer, "--to", toVer},
		io.Discard, &out)
	if code != 0 {
		t.Fatalf("draft-shim: exit %d, want 0", code)
	}
	if !strings.Contains(out.String(), `- rename: { from: "userName", to: "user_name" }`) {
		t.Errorf("rendered YAML missing the confident case-rename:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `target: "`+toVer+`"`) {
		t.Errorf("rendered YAML missing target=%q:\n%s", toVer, out.String())
	}
}

func TestRunDraftShimWritesOutFile(t *testing.T) {
	bundleDir, fromVer, toVer := writeDriftPair(t)
	outPath := filepath.Join(t.TempDir(), "draft.yaml")
	code := runDraftShim(
		[]string{"--bundle", bundleDir, "--from", fromVer, "--to", toVer, "--out", outPath},
		io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("draft-shim: exit %d, want 0", code)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read %s: %v", outPath, err)
	}
	if !strings.Contains(string(body), "- contract_version: \""+fromVer+"\"") {
		t.Errorf("--out file missing contract_version header:\n%s", body)
	}
}

func TestRunDraftShimStrictSurfacesCandidate(t *testing.T) {
	// userName ↔ user_name is a confident case-rename in default mode.
	// --strict refuses the auto-match and renders the candidates block.
	bundleDir, fromVer, toVer := writeDriftPair(t)
	var out bytes.Buffer
	code := runDraftShim(
		[]string{"--bundle", bundleDir, "--from", fromVer, "--to", toVer, "--strict"},
		io.Discard, &out)
	if code != 0 {
		t.Fatalf("draft-shim --strict: exit %d, want 0", code)
	}
	if !strings.Contains(out.String(), "# AMBIGUOUS pair") {
		t.Errorf("strict mode should emit a candidates block:\n%s", out.String())
	}
	if strings.Contains(out.String(), `      - rename:`) {
		t.Errorf("strict mode should not emit a confident rename outside the comment lane:\n%s", out.String())
	}
}

func TestRunDraftShimMissingFlags(t *testing.T) {
	if code := runDraftShim([]string{"--bundle", t.TempDir()}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("missing --from/--to: exit %d, want 2", code)
	}
}

func TestRunDraftShimMissingLayer(t *testing.T) {
	bundleDir, _, toVer := writeDriftPair(t)
	if code := runDraftShim(
		[]string{"--bundle", bundleDir, "--from", "9999-99", "--to", toVer},
		io.Discard, io.Discard); code != 1 {
		t.Fatalf("missing from-layer: exit %d, want 1", code)
	}
}

// --- verify: candidates-block check ---

func TestRunVerifyRefusesUnresolvedCandidatesBlock(t *testing.T) {
	in := writeSampleOpenAPI(t)
	bundleDir := t.TempDir()
	if code := run([]string{"add", "--openapi", in, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("add: exit %d", code)
	}
	res := []byte(`version: 1
overrides:
  - contract_version: "2026-05-17"
    transform:
      request:
        # AMBIGUOUS pair "text" ↔ "message" (same-type:string).
        # OPTION A — interpret as a rename:
        # - rename: { from: "text", to: "message" }
        # OPTION B — interpret as separate add + remove:
        # - default: { field: "message", value: "" }
`)
	if err := os.WriteFile(filepath.Join(bundleDir, "resolution.yaml"), res, 0o644); err != nil {
		t.Fatalf("write resolution.yaml: %v", err)
	}
	if code := run([]string{"verify", "--bundle", bundleDir}, io.Discard); code == 0 {
		t.Fatal("verify on an unresolved-candidates resolution.yaml: want non-zero exit code")
	}
}
