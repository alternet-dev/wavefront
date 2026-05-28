package bundlegen

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alternet-dev/wavefront/internal/bundle"
)

// GenTSClientResult is the resolved input for the TypeScript-client codegen.
// GenTSClient returns it on success so the CLI layer can render a status line
// from the same data the upcoming emission steps will consume.
type GenTSClientResult struct {
	BundleDir string
	OutDir    string
	Version   string
}

// GenTSClient resolves the inputs for the gen-ts-client subcommand. It loads
// the bundle at bundleDir, picks the target contract version (the requested
// pin if non-empty, else the lexically-highest version), and validates
// outDir — missing is fine (created here), an existing directory must be
// empty, and an existing non-directory is a hard error. No TypeScript is
// emitted; that lands in the follow-up emission work.
//
// version="" means "use the latest contract version present in the bundle".
// A pinned version that is not present in the bundle is a hard error whose
// message lists the available versions so the caller can correct the pin.
func GenTSClient(bundleDir, outDir, version string) (*GenTSClientResult, error) {
	if bundleDir == "" {
		return nil, errors.New("gen-ts-client: --bundle is required")
	}
	if outDir == "" {
		return nil, errors.New("gen-ts-client: --out is required")
	}

	b, err := bundle.Load(bundleDir)
	if err != nil {
		return nil, err
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

	return &GenTSClientResult{
		BundleDir: bundleDir,
		OutDir:    outDir,
		Version:   resolved,
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
