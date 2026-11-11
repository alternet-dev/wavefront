package bundlegen

// These limits match the loader's internal constants in internal/bundle.
// An archive that exceeds them will be rejected at load time, so the packer
// enforces the same constraints before writing to disk.
const (
	maxPackBytes     int64 = 1 << 30
	maxPackEntries         = 100_000
	maxPackPathBytes       = 4 << 10
)

// packState accumulates counters threaded through recursive writeDir calls.
type packState struct {
	totalBytes int64
	entries    int
}
