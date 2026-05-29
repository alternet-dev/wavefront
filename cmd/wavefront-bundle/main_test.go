package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireProtocGenES skips the test cleanly when protoc-gen-es is not on
// PATH. CI without Node tooling installed must still go green; the error
// path is exercised independently by TestRunGenTSClientHandlesMissingProtocGenES.
func requireProtocGenES(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("protoc-gen-es"); err != nil {
		t.Skipf("skipping: protoc-gen-es not on PATH (%v); install: npm install -g @bufbuild/protoc-gen-es", err)
	}
}

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

// --- gen-ts-client subcommand ---

// writeTwoLayerBundle writes a fixture bundle with two contract versions:
// 2026-05-17 (from the sample OpenAPI) and 2027-01-09 (a second add). It
// returns the bundle dir; the latest version is "2027-01-09".
func writeTwoLayerBundle(t *testing.T) (bundleDir, latest, older string) {
	t.Helper()
	bundleDir = t.TempDir()
	if code := run([]string{"add", "--openapi", writeSampleOpenAPI(t), "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("seed bundle (add older): exit %d", code)
	}
	// Second layer with a different info.version so it sorts above the first.
	const doc2 = `{"openapi":"3.0.0","info":{"title":"t","version":"2027-01-09"},
"paths":{"/v3/echo":{"post":{
"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}},
"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}}}}}},
"components":{"schemas":{"Req":{"type":"object","properties":{"text":{"type":"string"}}}}}}`
	p := filepath.Join(t.TempDir(), "openapi2.json")
	if err := os.WriteFile(p, []byte(doc2), 0o600); err != nil {
		t.Fatalf("write openapi2: %v", err)
	}
	if code := run([]string{"add", "--openapi", p, "--bundle", bundleDir}, io.Discard); code != 0 {
		t.Fatalf("seed bundle (add newer): exit %d", code)
	}
	return bundleDir, "2027-01-09", "2026-05-17"
}

func TestRunGenTSClientDefaultsToLatestVersion(t *testing.T) {
	requireProtocGenES(t)
	bundleDir, latest, _ := writeTwoLayerBundle(t)
	outDir := filepath.Join(t.TempDir(), "client") // missing — must be created
	var out bytes.Buffer
	code := runGenTSClient(
		[]string{"--bundle", bundleDir, "--out", outDir},
		io.Discard, &out)
	if code != 0 {
		t.Fatalf("gen-ts-client (default version): exit %d, want 0", code)
	}
	if !strings.Contains(out.String(), "version="+latest) {
		t.Errorf("stdout should name the resolved version %q:\n%s", latest, out.String())
	}
	if !strings.Contains(out.String(), "emitted") || !strings.Contains(out.String(), "message class") {
		t.Errorf("stdout should report the emitted message-class count:\n%s", out.String())
	}
	if _, err := os.Stat(outDir); err != nil {
		t.Errorf("--out should have been created: %v", err)
	}
	// At least one .ts file must have been written under outDir.
	var sawTS bool
	if err := filepath.Walk(outDir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(p, ".ts") {
			sawTS = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk outDir: %v", err)
	}
	if !sawTS {
		t.Errorf("gen-ts-client: expected at least one .ts file under %s", outDir)
	}
}

func TestRunGenTSClientPinsVersion(t *testing.T) {
	requireProtocGenES(t)
	bundleDir, _, older := writeTwoLayerBundle(t)
	outDir := filepath.Join(t.TempDir(), "client")
	var out bytes.Buffer
	code := runGenTSClient(
		[]string{"--bundle", bundleDir, "--out", outDir, "--version", older},
		io.Discard, &out)
	if code != 0 {
		t.Fatalf("gen-ts-client --version %s: exit %d, want 0", older, code)
	}
	if !strings.Contains(out.String(), "version="+older) {
		t.Errorf("stdout should reflect the pinned version %q:\n%s", older, out.String())
	}
}

func TestRunGenTSClientRejectsUnknownVersion(t *testing.T) {
	bundleDir, latest, older := writeTwoLayerBundle(t)
	outDir := filepath.Join(t.TempDir(), "client")
	var errOut bytes.Buffer
	code := runGenTSClient(
		[]string{"--bundle", bundleDir, "--out", outDir, "--version", "9999-99"},
		&errOut, io.Discard)
	if code == 0 {
		t.Fatal("gen-ts-client --version <bogus>: want non-zero exit code")
	}
	msg := errOut.String()
	if !strings.Contains(msg, "9999-99") {
		t.Errorf("error should name the missing version:\n%s", msg)
	}
	// The available list must surface both known versions; order is the
	// lexically-ascending list emitted by bundle.Versions().
	for _, v := range []string{older, latest} {
		if !strings.Contains(msg, v) {
			t.Errorf("error should list available version %q:\n%s", v, msg)
		}
	}
}

func TestRunGenTSClientBundleLoadFailureSurfaces(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	outDir := filepath.Join(t.TempDir(), "client")
	var errOut bytes.Buffer
	code := runGenTSClient(
		[]string{"--bundle", missing, "--out", outDir},
		&errOut, io.Discard)
	if code == 0 {
		t.Fatal("gen-ts-client with missing bundle: want non-zero exit code")
	}
	if errOut.Len() == 0 {
		t.Error("gen-ts-client with missing bundle: expected an error message on stderr")
	}
}

func TestRunGenTSClientRefusesNonEmptyOut(t *testing.T) {
	bundleDir, _, _ := writeTwoLayerBundle(t)
	outDir := t.TempDir() // pre-existing, will populate
	if err := os.WriteFile(filepath.Join(outDir, "stale.ts"), []byte("// stale\n"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	var errOut bytes.Buffer
	code := runGenTSClient(
		[]string{"--bundle", bundleDir, "--out", outDir},
		&errOut, io.Discard)
	if code == 0 {
		t.Fatal("gen-ts-client into a non-empty --out: want non-zero exit code")
	}
	if !strings.Contains(errOut.String(), "empty") {
		t.Errorf("error should explain that --out must be empty or missing:\n%s", errOut.String())
	}
}

func TestRunGenTSClientAcceptsExistingEmptyOut(t *testing.T) {
	requireProtocGenES(t)
	bundleDir, _, _ := writeTwoLayerBundle(t)
	outDir := t.TempDir() // pre-existing, empty
	code := runGenTSClient(
		[]string{"--bundle", bundleDir, "--out", outDir},
		io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("gen-ts-client into an existing empty --out: exit %d, want 0", code)
	}
}

// TestRunGenTSClientHandlesMissingProtocGenES exercises the error path when
// protoc-gen-es is not on PATH. It runs unconditionally — independent of
// whether the tool is locally installed — by clearing PATH for the duration
// of the test. The CLI must exit non-zero and surface the install hint on
// stderr so the operator knows what to do next.
func TestRunGenTSClientHandlesMissingProtocGenES(t *testing.T) {
	t.Setenv("PATH", "")
	bundleDir, _, _ := writeTwoLayerBundle(t)
	outDir := filepath.Join(t.TempDir(), "client")
	var errOut bytes.Buffer
	code := runGenTSClient(
		[]string{"--bundle", bundleDir, "--out", outDir},
		&errOut, io.Discard)
	if code == 0 {
		t.Fatal("gen-ts-client with no protoc-gen-es on PATH: want non-zero exit code")
	}
	msg := errOut.String()
	for _, want := range []string{"protoc-gen-es", "not found", "npm install"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr should mention %q:\n%s", want, msg)
		}
	}
}

func TestRunGenTSClientRefusesOutThatIsAFile(t *testing.T) {
	bundleDir, _, _ := writeTwoLayerBundle(t)
	outDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(outDir, []byte("file\n"), 0o600); err != nil {
		t.Fatalf("seed file at --out path: %v", err)
	}
	var errOut bytes.Buffer
	code := runGenTSClient(
		[]string{"--bundle", bundleDir, "--out", outDir},
		&errOut, io.Discard)
	if code == 0 {
		t.Fatal("gen-ts-client with --out pointing at a file: want non-zero exit code")
	}
}

func TestRunGenTSClientMissingFlags(t *testing.T) {
	// No flags at all.
	if code := runGenTSClient(nil, io.Discard, io.Discard); code != 2 {
		t.Fatalf("gen-ts-client without flags: exit %d, want 2", code)
	}
	// Missing --out.
	if code := runGenTSClient([]string{"--bundle", t.TempDir()}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("gen-ts-client without --out: exit %d, want 2", code)
	}
	// Missing --bundle.
	if code := runGenTSClient([]string{"--out", t.TempDir()}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("gen-ts-client without --bundle: exit %d, want 2", code)
	}
}

func TestRunDispatchGenTSClient(t *testing.T) {
	// run() must dispatch the gen-ts-client subcommand. Use a missing bundle
	// so we don't need to seed one — the dispatch path is what's under test.
	missing := filepath.Join(t.TempDir(), "no-bundle")
	code := run(
		[]string{"gen-ts-client", "--bundle", missing, "--out", filepath.Join(t.TempDir(), "client")},
		io.Discard)
	if code == 0 {
		t.Fatal("run gen-ts-client with missing bundle: want non-zero exit code")
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
