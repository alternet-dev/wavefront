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

	"github.com/alternet-dev/wavefront/internal/bundle"
)

const sourceOptionPlaceholder = "{source}"

// GenTypesOptions describes one standard protobuf code-generator invocation
// against a bundle layer. Plugin is the protoc suffix (for example "es",
// "go", "python", or "prost"), not the executable name.
type GenTypesOptions struct {
	BundleDir     string
	OutDir        string
	Version       string
	Plugin        string
	PluginPath    string
	PluginOptions []string
}

// GenTypesResult records the resolved layer and files emitted by GenTypes.
// GeneratedFiles contains paths relative to OutDir, sorted lexically.
type GenTypesResult struct {
	BundleDir      string
	OutDir         string
	Version        string
	Plugin         string
	SourceFiles    []string
	GeneratedFiles []string
}

// ProtocMissingError reports that the standard protobuf compiler is not on
// PATH. The selected generator may be built into protoc or may be a separate
// protoc-gen-X executable, so protoc is the one common prerequisite.
type ProtocMissingError struct {
	Cause error
}

func (e *ProtocMissingError) Error() string {
	return "gen-types: protoc not found on PATH\n" +
		"install the Protocol Buffers compiler and ensure protoc is on PATH"
}

func (e *ProtocMissingError) Unwrap() error { return e.Cause }

type protocExecutor interface {
	LookPath(string) (string, error)
	Run(string, []string) (string, error)
}

type systemProtocExecutor struct{}

func (systemProtocExecutor) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (systemProtocExecutor) Run(name string, args []string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stderr.String()), err
}

// GenTypes invokes a standard protoc generator against one bundle layer.
// An empty Version selects the lexically latest layer. The output directory
// is created when absent and must otherwise be empty, preventing stale files
// from an earlier generation from surviving unnoticed.
//
// A plugin option containing "{source}" is expanded once per bundle source
// file. This supports generators such as protoc-gen-go, which need an M-file
// import mapping while the language-neutral descriptors intentionally omit a
// go_package option.
func GenTypes(opts GenTypesOptions) (*GenTypesResult, error) {
	return genTypes(opts, systemProtocExecutor{})
}

func genTypes(opts GenTypesOptions, executor protocExecutor) (*GenTypesResult, error) {
	opts.BundleDir = strings.TrimSpace(opts.BundleDir)
	opts.OutDir = strings.TrimSpace(opts.OutDir)
	opts.Version = strings.TrimSpace(opts.Version)
	opts.Plugin = strings.TrimSpace(opts.Plugin)
	opts.PluginPath = strings.TrimSpace(opts.PluginPath)

	if opts.BundleDir == "" {
		return nil, errors.New("gen-types: --bundle is required")
	}
	if opts.OutDir == "" {
		return nil, errors.New("gen-types: --out is required")
	}
	if opts.Plugin == "" {
		return nil, errors.New("gen-types: --plugin is required")
	}
	if !validPluginName(opts.Plugin) {
		return nil, fmt.Errorf("gen-types: invalid --plugin %q (use letters, digits, underscores, or hyphens)", opts.Plugin)
	}

	b, err := bundle.Load(opts.BundleDir)
	if err != nil {
		return nil, fmt.Errorf("gen-types: load bundle: %w", err)
	}
	versions := b.Versions()
	if len(versions) == 0 {
		return nil, errors.New("gen-types: bundle has no contract versions")
	}
	resolvedVersion := opts.Version
	if resolvedVersion == "" {
		resolvedVersion = versions[len(versions)-1]
	} else if _, ok := b.Contract(resolvedVersion); !ok {
		return nil, fmt.Errorf(
			"gen-types: version %q not in bundle (available: %s)",
			resolvedVersion, strings.Join(versions, ", "),
		)
	}

	descriptorPath := filepath.Join(opts.BundleDir, resolvedVersion, "descriptors.binpb")
	sourceFiles, err := descriptorSourceFiles(descriptorPath)
	if err != nil {
		return nil, err
	}

	protocPath, err := executor.LookPath("protoc")
	if err != nil {
		return nil, &ProtocMissingError{Cause: err}
	}
	if err := ensureEmptyGenTypesOutDir(opts.OutDir); err != nil {
		return nil, err
	}

	args := make([]string, 0, 3+len(opts.PluginOptions)+len(sourceFiles))
	args = append(args, "--descriptor_set_in="+descriptorPath)
	if opts.PluginPath != "" {
		args = append(args, "--plugin=protoc-gen-"+opts.Plugin+"="+opts.PluginPath)
	}
	args = append(args, "--"+opts.Plugin+"_out="+opts.OutDir)
	for _, option := range expandSourceOptions(opts.PluginOptions, sourceFiles) {
		args = append(args, "--"+opts.Plugin+"_opt="+option)
	}
	args = append(args, sourceFiles...)

	stderr, runErr := executor.Run(protocPath, args)
	if runErr != nil {
		if stderr == "" {
			return nil, fmt.Errorf("gen-types: run protoc for plugin %q: %w", opts.Plugin, runErr)
		}
		return nil, fmt.Errorf("gen-types: run protoc for plugin %q: %w (stderr: %s)", opts.Plugin, runErr, stderr)
	}

	generatedFiles, err := listGeneratedFiles(opts.OutDir)
	if err != nil {
		return nil, err
	}
	if len(generatedFiles) == 0 {
		return nil, fmt.Errorf("gen-types: protoc plugin %q produced no files", opts.Plugin)
	}

	return &GenTypesResult{
		BundleDir:      opts.BundleDir,
		OutDir:         opts.OutDir,
		Version:        resolvedVersion,
		Plugin:         opts.Plugin,
		SourceFiles:    sourceFiles,
		GeneratedFiles: generatedFiles,
	}, nil
}

func validPluginName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			if i == 0 && (c == '_' || c == '-') {
				return false
			}
			continue
		}
		return false
	}
	return name != ""
}

// descriptorSourceFiles returns the files the consumer's language generator
// should emit. Well-known types are embedded in some layers to keep runtime
// descriptor loading self-contained, but their generated language types come
// from the protobuf runtime and must not be emitted into the consumer tree.
func descriptorSourceFiles(descriptorPath string) ([]string, error) {
	raw, err := os.ReadFile(descriptorPath)
	if err != nil {
		return nil, fmt.Errorf("gen-types: read %s: %w", descriptorPath, err)
	}
	var descriptors descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &descriptors); err != nil {
		return nil, fmt.Errorf("gen-types: parse %s: %w", descriptorPath, err)
	}

	files := make([]string, 0, len(descriptors.File))
	for _, file := range descriptors.File {
		name := strings.TrimSpace(file.GetName())
		if name == "" || strings.HasPrefix(name, "google/protobuf/") {
			continue
		}
		if err := validateDescriptorSourceName(name); err != nil {
			return nil, fmt.Errorf("gen-types: %s: %w", descriptorPath, err)
		}
		files = append(files, name)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("gen-types: %s contains no generator source files", descriptorPath)
	}
	sort.Strings(files)
	return files, nil
}

func validateDescriptorSourceName(name string) error {
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return fmt.Errorf("descriptor source name %q must be relative and must not start with a hyphen", name)
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("descriptor source name %q must use / as the separator", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("descriptor source name %q contains invalid path segment %q", name, segment)
		}
	}
	return nil
}

func expandSourceOptions(options, sourceFiles []string) []string {
	var expanded []string
	for _, option := range options {
		if strings.Contains(option, sourceOptionPlaceholder) {
			for _, source := range sourceFiles {
				expanded = append(expanded, strings.ReplaceAll(option, sourceOptionPlaceholder, source))
			}
			continue
		}
		expanded = append(expanded, option)
	}
	return expanded
}

func ensureEmptyGenTypesOutDir(outDir string) error {
	info, err := os.Stat(outDir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fmt.Errorf("gen-types: create --out: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("gen-types: stat --out: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("gen-types: --out %q exists but is not a directory", outDir)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return fmt.Errorf("gen-types: read --out: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("gen-types: --out %q must be empty or missing (found %d existing entries)", filepath.Clean(outDir), len(entries))
	}
	return nil
}

func listGeneratedFiles(outDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(outDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(outDir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gen-types: inspect --out: %w", err)
	}
	sort.Strings(files)
	return files, nil
}
