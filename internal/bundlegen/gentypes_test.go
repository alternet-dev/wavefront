package bundlegen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

type fakeProtocExecutor struct {
	path        string
	lookPathErr error
	run         func(name string, args []string) (string, error)
	lookedUp    string
	runName     string
	runArgs     []string
}

func (f *fakeProtocExecutor) LookPath(name string) (string, error) {
	f.lookedUp = name
	if f.lookPathErr != nil {
		return "", f.lookPathErr
	}
	if f.path == "" {
		return "/test/bin/protoc", nil
	}
	return f.path, nil
}

func (f *fakeProtocExecutor) Run(name string, args []string) (string, error) {
	f.runName = name
	f.runArgs = append([]string(nil), args...)
	if f.run == nil {
		return "", nil
	}
	return f.run(name, args)
}

func addGenTypesLayer(t *testing.T, bundleDir, version string, withStruct bool) {
	t.Helper()
	property := `"text":{"type":"string"}`
	if withStruct {
		property = `"payload":{"type":"object","additionalProperties":true}`
	}
	doc := fmt.Sprintf(`{"openapi":"3.0.0","info":{"title":"t","version":%q},
"paths":{"/v3/echo":{"post":{
"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}},
"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Req"}}}}}}}},
"components":{"schemas":{"Req":{"type":"object","properties":{%s}}}}}`, version, property)
	source := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(source, []byte(doc), 0o600); err != nil {
		t.Fatalf("write OpenAPI: %v", err)
	}
	if err := Add(source, bundleDir, false); err != nil {
		t.Fatalf("add bundle layer %q: %v", version, err)
	}
}

func outputDirFromArgs(t *testing.T, plugin string, args []string) string {
	t.Helper()
	prefix := "--" + plugin + "_out="
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	t.Fatalf("protoc args missing %s: %v", prefix, args)
	return ""
}

func TestGenTypesDefaultsToLatestAndInvokesRequestedPlugin(t *testing.T) {
	bundleDir := t.TempDir()
	addGenTypesLayer(t, bundleDir, "2026-05-17", false)
	addGenTypesLayer(t, bundleDir, "2027-01", false)
	outDir := filepath.Join(t.TempDir(), "generated")
	fake := &fakeProtocExecutor{path: "/tools/protoc"}
	fake.run = func(_ string, args []string) (string, error) {
		out := outputDirFromArgs(t, "es", args)
		if err := os.MkdirAll(filepath.Join(out, "z"), 0o755); err != nil {
			t.Fatalf("mkdir generated: %v", err)
		}
		if err := os.WriteFile(filepath.Join(out, "z", "types.ts"), []byte("generated\n"), 0o600); err != nil {
			t.Fatalf("write generated file: %v", err)
		}
		if err := os.WriteFile(filepath.Join(out, "a.ts"), []byte("generated\n"), 0o600); err != nil {
			t.Fatalf("write generated file: %v", err)
		}
		return "", nil
	}

	result, err := genTypes(GenTypesOptions{
		BundleDir:     bundleDir,
		OutDir:        outDir,
		Plugin:        "es",
		PluginPath:    "./node_modules/.bin/protoc-gen-es",
		PluginOptions: []string{"target=ts", "import_extension=none"},
	}, fake)
	if err != nil {
		t.Fatalf("genTypes: %v", err)
	}
	if result.Version != "2027-01" {
		t.Fatalf("resolved version = %q, want latest 2027-01", result.Version)
	}
	wantSources := []string{"wavefront/gen/2027_01.proto"}
	if !reflect.DeepEqual(result.SourceFiles, wantSources) {
		t.Errorf("source files = %v, want %v", result.SourceFiles, wantSources)
	}
	wantGenerated := []string{"a.ts", "z/types.ts"}
	if !reflect.DeepEqual(result.GeneratedFiles, wantGenerated) {
		t.Errorf("generated files = %v, want %v", result.GeneratedFiles, wantGenerated)
	}
	if fake.lookedUp != "protoc" || fake.runName != "/tools/protoc" {
		t.Errorf("executor calls = LookPath(%q), Run(%q), want protoc and /tools/protoc", fake.lookedUp, fake.runName)
	}
	wantArgs := []string{
		"--descriptor_set_in=" + filepath.Join(bundleDir, "2027-01", "descriptors.binpb"),
		"--plugin=protoc-gen-es=./node_modules/.bin/protoc-gen-es",
		"--es_out=" + outDir,
		"--es_opt=target=ts",
		"--es_opt=import_extension=none",
		"wavefront/gen/2027_01.proto",
	}
	if !reflect.DeepEqual(fake.runArgs, wantArgs) {
		t.Errorf("protoc args:\n got %q\nwant %q", fake.runArgs, wantArgs)
	}
}

func TestGenTypesPinsVersionAndExcludesEmbeddedWellKnownTypes(t *testing.T) {
	bundleDir := t.TempDir()
	addGenTypesLayer(t, bundleDir, "2026-05-17", true)
	addGenTypesLayer(t, bundleDir, "2027-01", false)
	outDir := filepath.Join(t.TempDir(), "generated")
	fake := &fakeProtocExecutor{}
	fake.run = func(_ string, args []string) (string, error) {
		out := outputDirFromArgs(t, "python", args)
		return "", os.WriteFile(filepath.Join(out, "types.py"), []byte("generated\n"), 0o600)
	}

	result, err := genTypes(GenTypesOptions{
		BundleDir: bundleDir,
		OutDir:    outDir,
		Version:   "2026-05-17",
		Plugin:    "python",
	}, fake)
	if err != nil {
		t.Fatalf("genTypes: %v", err)
	}
	want := []string{"wavefront/gen/2026_05_17.proto"}
	if !reflect.DeepEqual(result.SourceFiles, want) {
		t.Fatalf("source files = %v, want %v", result.SourceFiles, want)
	}
	for _, arg := range fake.runArgs {
		if strings.Contains(arg, "google/protobuf/struct.proto") {
			t.Fatalf("embedded well-known type was passed as a generator source: %q", arg)
		}
	}
}

func TestExpandSourceOptions(t *testing.T) {
	got := expandSourceOptions(
		[]string{"paths=source_relative", "M{source}=example.com/client/gen"},
		[]string{"acme/a.proto", "acme/b.proto"},
	)
	want := []string{
		"paths=source_relative",
		"Macme/a.proto=example.com/client/gen",
		"Macme/b.proto=example.com/client/gen",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded options = %v, want %v", got, want)
	}
}

func TestGenTypesReportsMissingProtoc(t *testing.T) {
	bundleDir := t.TempDir()
	addGenTypesLayer(t, bundleDir, "2026-05-17", false)
	outDir := filepath.Join(t.TempDir(), "generated")
	fake := &fakeProtocExecutor{lookPathErr: errors.New("not found")}

	_, err := genTypes(GenTypesOptions{
		BundleDir: bundleDir,
		OutDir:    outDir,
		Plugin:    "go",
	}, fake)
	if err == nil {
		t.Fatal("genTypes without protoc: want error")
	}
	var missing *ProtocMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("error type = %T, want *ProtocMissingError", err)
	}
	if !strings.Contains(err.Error(), "protoc not found") {
		t.Errorf("error should identify missing protoc: %v", err)
	}
	if _, statErr := os.Stat(outDir); !os.IsNotExist(statErr) {
		t.Errorf("output directory should not be created before protoc is resolved: %v", statErr)
	}
}

func TestGenTypesReportsPluginFailure(t *testing.T) {
	bundleDir := t.TempDir()
	addGenTypesLayer(t, bundleDir, "2026-05-17", false)
	fake := &fakeProtocExecutor{run: func(string, []string) (string, error) {
		return "protoc-gen-prost: program not found", errors.New("exit status 1")
	}}

	_, err := genTypes(GenTypesOptions{
		BundleDir: bundleDir,
		OutDir:    filepath.Join(t.TempDir(), "generated"),
		Plugin:    "prost",
	}, fake)
	if err == nil {
		t.Fatal("genTypes after plugin failure: want error")
	}
	for _, want := range []string{"plugin \"prost\"", "exit status 1", "program not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestGenTypesRejectsNonEmptyOutput(t *testing.T) {
	bundleDir := t.TempDir()
	addGenTypesLayer(t, bundleDir, "2026-05-17", false)
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "stale.go"), []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	fake := &fakeProtocExecutor{}

	_, err := genTypes(GenTypesOptions{BundleDir: bundleDir, OutDir: outDir, Plugin: "go"}, fake)
	if err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("genTypes non-empty output error = %v, want empty-directory error", err)
	}
	if fake.runName != "" {
		t.Fatalf("protoc ran despite non-empty output: %q", fake.runName)
	}
}

func TestGenTypesRejectsInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		opts GenTypesOptions
		want string
	}{
		{name: "bundle", opts: GenTypesOptions{OutDir: "out", Plugin: "go"}, want: "--bundle is required"},
		{name: "output", opts: GenTypesOptions{BundleDir: "bundle", Plugin: "go"}, want: "--out is required"},
		{name: "plugin", opts: GenTypesOptions{BundleDir: "bundle", OutDir: "out"}, want: "--plugin is required"},
		{name: "plugin name", opts: GenTypesOptions{BundleDir: "bundle", OutDir: "out", Plugin: "--go_out"}, want: "invalid --plugin"},
		{name: "non-ASCII plugin name", opts: GenTypesOptions{BundleDir: "bundle", OutDir: "out", Plugin: "gø"}, want: "invalid --plugin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := genTypes(test.opts, &fakeProtocExecutor{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestDescriptorSourceFilesRejectsFlagLikeNames(t *testing.T) {
	descriptors := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("--go_out=outside.proto"),
	}}}
	raw, err := proto.Marshal(descriptors)
	if err != nil {
		t.Fatalf("marshal descriptors: %v", err)
	}
	path := filepath.Join(t.TempDir(), "descriptors.binpb")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write descriptors: %v", err)
	}
	_, err = descriptorSourceFiles(path)
	if err == nil || !strings.Contains(err.Error(), "must not start with a hyphen") {
		t.Fatalf("descriptorSourceFiles error = %v, want unsafe-name error", err)
	}
}

func TestGenTypesRejectsUnknownVersion(t *testing.T) {
	bundleDir := t.TempDir()
	addGenTypesLayer(t, bundleDir, "2026-05-17", false)
	_, err := genTypes(GenTypesOptions{
		BundleDir: bundleDir,
		OutDir:    filepath.Join(t.TempDir(), "generated"),
		Version:   "missing",
		Plugin:    "go",
	}, &fakeProtocExecutor{})
	if err == nil || !strings.Contains(err.Error(), `version "missing" not in bundle`) {
		t.Fatalf("unknown version error = %v", err)
	}
}

func TestGenTypesRejectsAPluginThatProducesNoFiles(t *testing.T) {
	bundleDir := t.TempDir()
	addGenTypesLayer(t, bundleDir, "2026-05-17", false)
	_, err := genTypes(GenTypesOptions{
		BundleDir: bundleDir,
		OutDir:    filepath.Join(t.TempDir(), "generated"),
		Plugin:    "go",
	}, &fakeProtocExecutor{})
	if err == nil || !strings.Contains(err.Error(), "produced no files") {
		t.Fatalf("no-output error = %v", err)
	}
}
