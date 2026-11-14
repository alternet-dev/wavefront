package bundlegen

// These values match the loader's internal archive limits in internal/bundle.
// They bound work while the packer constructs an archive; Pack then verifies
// the completed archive with the loader before installing it. The path limit
// also bounds recursive descent: each nested directory adds at least two bytes
// to the child entry path, so writeDir refuses an entry past maxPackPathBytes
// before descending into it.
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
