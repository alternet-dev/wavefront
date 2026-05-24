package wireerror

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// The canonical wavefront.v0.Error wire shape lives at proto/wavefront/v0/error.proto.
// errorproto.go builds the same shape in code at boot. This test guards the seam
// between those two — a drift on either side fails the test, and `buf breaking`
// in CI guards the .proto file against breaking edits over time.
func TestProtoFileMatchesInCodeDescriptor(t *testing.T) {
	src := readCanonicalErrorProto(t)

	mustContain(t, src, "package wavefront.v0;")
	mustContain(t, src, "message Error")

	fields := errorMD.Fields()
	if fields.Len() == 0 {
		t.Fatal("errorMD has no fields — wireerror init is broken")
	}
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		decl := fmt.Sprintf("%s %s = %d;", protoFieldTypeName(t, f.Kind()), f.Name(), f.Number())
		mustContain(t, src, decl)
	}
}

func readCanonicalErrorProto(t *testing.T) string {
	t.Helper()
	// `go test` runs with cwd == package directory. The canonical .proto lives
	// two levels up from internal/wireerror at proto/wavefront/v0/error.proto.
	path := filepath.Join("..", "..", "proto", "wavefront", "v0", "error.proto")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading canonical error.proto at %s: %v", path, err)
	}
	return string(b)
}

func mustContain(t *testing.T, src, want string) {
	t.Helper()
	if !strings.Contains(src, want) {
		t.Errorf("error.proto missing %q", want)
	}
}

func protoFieldTypeName(t *testing.T, k protoreflect.Kind) string {
	t.Helper()
	switch k {
	case protoreflect.StringKind:
		return "string"
	case protoreflect.Int32Kind:
		return "int32"
	case protoreflect.Int64Kind:
		return "int64"
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.BytesKind:
		return "bytes"
	default:
		t.Fatalf("unsupported field kind %s — extend protoFieldTypeName when adding it to wavefront.v0.Error", k)
		return ""
	}
}
