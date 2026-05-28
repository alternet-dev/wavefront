package bundlegen

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	"github.com/alternet-dev/wavefront/internal/bundle"
)

// GenTSClientResult is the resolved input + outputs of the TypeScript-client
// codegen for one contract version. The CLI layer renders a status line from
// it; downstream chunks (routes.ts, client.ts) extend the struct with their
// own output lists.
type GenTSClientResult struct {
	BundleDir    string
	OutDir       string
	Version      string
	MessageFiles []string // paths relative to OutDir of emitted .ts message classes
}

// ProtocGenESMissingError is returned by GenTSClient when protoc-gen-es is
// not on PATH. It carries an install hint so the CLI layer can print an
// actionable message without inventing one. errors.As lets the CLI choose
// to special-case the message (for exit-code routing or a different
// rendering) without parsing the string.
type ProtocGenESMissingError struct {
	Cause error // the underlying exec.LookPath error
}

func (e *ProtocGenESMissingError) Error() string {
	return "gen-ts-client: protoc-gen-es not found on PATH\n" +
		"install: npm install -g @bufbuild/protoc-gen-es\n" +
		"        (or include @bufbuild/protoc-gen-es as a dev dependency and run via npx)"
}

func (e *ProtocGenESMissingError) Unwrap() error { return e.Cause }

// protocGenESParams is the parameter string passed to protoc-gen-es via the
// CodeGeneratorRequest. Documented here so the rationale survives review.
//
//   - target=ts: emit TypeScript source, not transpiled JavaScript. The
//     generated client is a TypeScript artifact; consumers compile it with
//     their own tsc/bundler.
//   - import_extension=none: emit import statements without a file
//     extension on the specifier (e.g. `import { Foo } from "./foo_pb"`
//     rather than `"./foo_pb.js"`). This works with tsc's
//     `moduleResolution: bundler` mode and every modern bundler (Vite,
//     esbuild, Webpack 5, Rollup). The alternative — `import_extension=.js`
//     — is the canonical ESM form but requires the consumer to be set up
//     for Node's resolver, which is a stricter contract than we want to
//     impose on a generated client. If a consumer needs `.js` later we can
//     surface this as a flag without breaking the default.
const protocGenESParams = "target=ts,import_extension=none"

// GenTSClient resolves the inputs for the gen-ts-client subcommand, then
// emits TypeScript message classes for the target layer's
// FileDescriptorSet by shelling out to protoc-gen-es on PATH. version=""
// means "use the latest contract version present in the bundle"; a pinned
// version that is not present in the bundle is a hard error whose message
// lists the available versions.
//
// Layout: outDir is created if missing; an existing outDir must be empty.
// The emission step writes .ts files under outDir at the relative paths
// protoc-gen-es chooses (typically <package>/<name>_pb.ts mirroring the
// proto file layout).
//
// If protoc-gen-es is not on PATH this returns a *ProtocGenESMissingError
// (errors.As) carrying an actionable install hint. The lookup happens late
// in this function — after bundle load and version resolution — so the
// caller sees the "real" problem first when the bundle itself is broken.
func GenTSClient(bundleDir, outDir, version string) (*GenTSClientResult, error) {
	if bundleDir == "" {
		return nil, errors.New("gen-ts-client: --bundle is required")
	}
	if outDir == "" {
		return nil, errors.New("gen-ts-client: --out is required")
	}

	b, err := bundle.Load(bundleDir)
	if err != nil {
		return nil, fmt.Errorf("gen-ts-client: load bundle: %w", err)
	}
	versions := b.Versions()
	if len(versions) == 0 {
		// bundle.Load enforces at least one layer, so this is defence in
		// depth — if it ever flips, the message should still be helpful.
		return nil, errors.New("gen-ts-client: bundle has no contract versions")
	}

	resolved := strings.TrimSpace(version)
	if resolved == "" {
		// Latest = the lexically-highest contract version. Layer/contract
		// names are date-stamped (e.g. "2026-05-17"), so a string sort
		// is the same as a chronological sort.
		resolved = versions[len(versions)-1]
	} else {
		if _, ok := b.Contract(resolved); !ok {
			return nil, fmt.Errorf(
				"gen-ts-client: version %q not in bundle (available: %s)",
				resolved, strings.Join(versions, ", "),
			)
		}
	}

	if err := ensureEmptyOutDir(outDir); err != nil {
		return nil, err
	}

	messageFiles, err := emitMessageClasses(bundleDir, outDir, resolved)
	if err != nil {
		return nil, err
	}

	return &GenTSClientResult{
		BundleDir:    bundleDir,
		OutDir:       outDir,
		Version:      resolved,
		MessageFiles: messageFiles,
	}, nil
}

// ensureEmptyOutDir validates the --out path policy: a missing path is
// created (parent must exist or be creatable), an existing path must be an
// empty directory, anything else is rejected. The policy is conservative on
// purpose — re-emitting into a non-empty directory would leave stale files
// from a previous emission silently in place; force-overwrite is a later
// opt-in if it becomes necessary.
func ensureEmptyOutDir(outDir string) error {
	info, err := os.Stat(outDir)
	if errors.Is(err, fs.ErrNotExist) {
		if mkErr := os.MkdirAll(outDir, 0o755); mkErr != nil {
			return fmt.Errorf("gen-ts-client: create --out: %w", mkErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("gen-ts-client: stat --out: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("gen-ts-client: --out %q exists but is not a directory", outDir)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return fmt.Errorf("gen-ts-client: read --out: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("gen-ts-client: --out %q must be empty or missing (found %d existing entries)", filepath.Clean(outDir), len(entries))
	}
	return nil
}

// emitMessageClasses invokes protoc-gen-es on the given layer's
// descriptors.binpb and writes the resulting .ts files under outDir. It
// returns the relative paths of every file written, in sorted order so the
// status line and tests are deterministic across runs.
//
// Wire shape: protoc-gen-es is a stock protoc plugin — it reads a wire-
// encoded CodeGeneratorRequest on stdin and writes a wire-encoded
// CodeGeneratorResponse on stdout. We build the request from the layer's
// FileDescriptorSet, marshal it, pipe it in, then unmarshal stdout and
// write each response.File to disk at the path protoc-gen-es chose
// (typically <package-path>/<basename>_pb.ts).
func emitMessageClasses(bundleDir, outDir, version string) ([]string, error) {
	binPath, err := exec.LookPath("protoc-gen-es")
	if err != nil {
		return nil, &ProtocGenESMissingError{Cause: err}
	}

	descPath := filepath.Join(bundleDir, version, "descriptors.binpb")
	descBytes, err := os.ReadFile(descPath)
	if err != nil {
		return nil, fmt.Errorf("gen-ts-client: read %s: %w", descPath, err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(descBytes, &fds); err != nil {
		return nil, fmt.Errorf("gen-ts-client: parse %s: %w", descPath, err)
	}
	if len(fds.File) == 0 {
		return nil, fmt.Errorf("gen-ts-client: %s contains no files", descPath)
	}

	// file_to_generate lists the .proto files the plugin should emit code
	// for. We list every file in the descriptor set — the v0.1 bundle is
	// self-contained per layer, so every file is a "first-party" file the
	// caller wants typed.
	fileToGenerate := make([]string, 0, len(fds.File))
	for _, f := range fds.File {
		fileToGenerate = append(fileToGenerate, f.GetName())
	}

	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: fileToGenerate,
		Parameter:      proto.String(protocGenESParams),
		ProtoFile:      fds.File,
	}
	reqBytes, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("gen-ts-client: marshal CodeGeneratorRequest: %w", err)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binPath)
	cmd.Stdin = bytes.NewReader(reqBytes)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gen-ts-client: run protoc-gen-es: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	var resp pluginpb.CodeGeneratorResponse
	if err := proto.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("gen-ts-client: parse CodeGeneratorResponse: %w", err)
	}
	if e := resp.GetError(); e != "" {
		return nil, fmt.Errorf("gen-ts-client: protoc-gen-es: %s", e)
	}

	written, err := writeGeneratedFiles(outDir, resp.File)
	if err != nil {
		return nil, err
	}
	return written, nil
}

// writeGeneratedFiles materialises each CodeGeneratorResponse_File under
// outDir. The plugin protocol allows a file with an omitted name to be
// "appended to the previous file" (used by some plugins to stream large
// outputs in chunks); we honour that by buffering into the most recent
// named entry and flushing on completion. Returned paths are relative to
// outDir, sorted ascending so a re-run sees the same ordering.
//
// Path safety: response file names are relative; the plugin protocol
// forbids ".." or absolute paths. We re-check that invariant here so a
// misbehaving plugin can't write outside outDir.
func writeGeneratedFiles(outDir string, files []*pluginpb.CodeGeneratorResponse_File) ([]string, error) {
	if len(files) == 0 {
		return nil, errors.New("gen-ts-client: protoc-gen-es produced no files")
	}
	// Accumulate per-file content in declaration order; a nameless entry
	// belongs to the most recent named one.
	type entry struct {
		name    string
		content strings.Builder
	}
	var ordered []*entry
	var current *entry
	for _, f := range files {
		name := f.GetName()
		if name == "" {
			if current == nil {
				return nil, errors.New("gen-ts-client: protoc-gen-es emitted a content-only file with no prior name")
			}
			current.content.WriteString(f.GetContent())
			continue
		}
		if err := validatePluginPath(name); err != nil {
			return nil, err
		}
		current = &entry{name: name}
		current.content.WriteString(f.GetContent())
		ordered = append(ordered, current)
	}

	written := make([]string, 0, len(ordered))
	for _, e := range ordered {
		abs := filepath.Join(outDir, filepath.FromSlash(e.name))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fmt.Errorf("gen-ts-client: mkdir for %s: %w", e.name, err)
		}
		if err := os.WriteFile(abs, []byte(e.content.String()), 0o644); err != nil {
			return nil, fmt.Errorf("gen-ts-client: write %s: %w", e.name, err)
		}
		written = append(written, e.name)
	}
	sort.Strings(written)
	return written, nil
}

// validatePluginPath enforces the CodeGeneratorResponse_File.name rules:
// uses "/" as the separator, is relative, and never contains "." or ".."
// segments. The plugin protocol already specifies this; we re-validate
// because a buggy or malicious plugin could otherwise climb out of outDir.
func validatePluginPath(name string) error {
	if name == "" {
		return errors.New("gen-ts-client: response file name is empty")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("gen-ts-client: response file name %q must be relative", name)
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("gen-ts-client: response file name %q must use / as the separator", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("gen-ts-client: response file name %q contains a forbidden %q segment", name, seg)
		}
	}
	return nil
}
