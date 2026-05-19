package server

import (
	"bytes"
	"io"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func protoStr(s string) protoreflect.Value { return protoreflect.ValueOfString(s) }
func bytesReader(b []byte) io.Reader       { return bytes.NewReader(b) }
