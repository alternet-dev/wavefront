package bundle

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/alternet-dev/wavefront/internal/transform"
)

// validatePathAgainstMessage walks p against the FileDescriptor for the
// given message. Each non-final non-array segment's field must exist AND be
// TYPE_MESSAGE. Each array segment's field must exist AND be repeated
// (LABEL_REPEATED). The final segment's field must exist (any type — leaf
// type rules belong to runtime).
//
// segLabel: a label for error messages identifying the path's role
// (e.g. "request.rename.from").
func validatePathAgainstMessage(p transform.Path, md protoreflect.MessageDescriptor, segLabel string) error {
	cur := md
	for i, seg := range p {
		fd := cur.Fields().ByName(protoreflect.Name(seg.Name))
		if fd == nil {
			// protojson also accepts json_name; try that as a fallback so
			// stanza authors can address fields by their protojson form.
			fd = cur.Fields().ByJSONName(seg.Name)
		}
		if fd == nil {
			return fmt.Errorf("%s: %s has no field %q at segment %d", segLabel, cur.FullName(), seg.Name, i+1)
		}
		repeated := fd.Cardinality() == protoreflect.Repeated && !fd.IsMap()
		if seg.Array && !repeated {
			return fmt.Errorf("%s: field %q at segment %d is not repeated; path uses [] but the descriptor isn't an array", segLabel, seg.Name, i+1)
		}
		if !seg.Array && repeated && i < len(p)-1 {
			return fmt.Errorf("%s: field %q at segment %d is repeated; non-final segment must use [] to iterate", segLabel, seg.Name, i+1)
		}
		if i < len(p)-1 {
			if fd.Kind() != protoreflect.MessageKind {
				return fmt.Errorf("%s: field %q at segment %d is scalar; path expects an object intermediate", segLabel, seg.Name, i+1)
			}
			cur = fd.Message()
		}
	}
	return nil
}
