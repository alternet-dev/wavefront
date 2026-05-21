package bundle

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// fdsBytes builds a self-contained FileDescriptorSet (package acme.v1, two
// proto3 string messages) and returns its wire bytes.
func fdsBytes(t *testing.T) []byte {
	t.Helper()
	strField := func(name string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(name),
			Number:   proto.Int32(num),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			JsonName: proto.String(name),
		}
	}
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{{
			Name:    proto.String("acme/v1/types.proto"),
			Package: proto.String("acme.v1"),
			Syntax:  proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("Ping"), Field: []*descriptorpb.FieldDescriptorProto{strField("text", 1)}},
				{Name: proto.String("Pong"), Field: []*descriptorpb.FieldDescriptorProto{strField("text", 1)}},
			},
		}},
	}
	b, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal fds: %v", err)
	}
	return b
}

const validOpenAPI = `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{}}`

const validVersions = `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/me/session
    method: GET
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// writeBundle writes a single-layer bundle and returns the bundle path. The
// lone layer goes under the subdirectory "layer"; nil/empty inputs are
// skipped so the missing/malformed-file tests still exercise their cases.
func writeBundle(t *testing.T, descriptors []byte, openapi, versions string) string {
	t.Helper()
	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	mustMkdir(t, layer)
	if descriptors != nil {
		mustWrite(t, filepath.Join(layer, fileDescriptors), descriptors)
	}
	if openapi != "" {
		mustWrite(t, filepath.Join(layer, fileOpenAPI), []byte(openapi))
	}
	if versions != "" {
		mustWrite(t, filepath.Join(layer, fileVersions), []byte(versions))
	}
	return dir
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadValidBundle(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c, ok := b.Contract("2024-11")
	if !ok {
		t.Fatal(`Contract("2024-11") not found`)
	}
	if c.Route() != "/v3/me/session" || c.Method() != "GET" {
		t.Errorf("route/method = %q %q", c.Route(), c.Method())
	}
	if c.RequestMessage() != "acme.v1.Ping" || c.ResponseMessage() != "acme.v1.Pong" {
		t.Errorf("messages = %q %q", c.RequestMessage(), c.ResponseMessage())
	}
	if _, err := b.Message("acme.v1.Ping"); err != nil {
		t.Errorf("resolve acme.v1.Ping: %v", err)
	}
}

func TestMissingFilesAreReadErrors(t *testing.T) {
	cases := []struct {
		name              string
		desc              []byte
		openapi, versions string
		wantFile          string
	}{
		{"no descriptors", nil, validOpenAPI, validVersions, fileDescriptors},
		{"no openapi", fdsBytes(t), "", validVersions, fileOpenAPI},
		{"no versions", fdsBytes(t), validOpenAPI, "", fileVersions},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeBundle(t, c.desc, c.openapi, c.versions)
			_, err := Load(dir)
			var re *ReadError
			if !errors.As(err, &re) || re.File != c.wantFile {
				t.Fatalf("want ReadError(%s), got %v", c.wantFile, err)
			}
		})
	}
}

func TestMalformedFilesAreParseErrors(t *testing.T) {
	cases := []struct {
		name              string
		desc              []byte
		openapi, versions string
		wantFile          string
	}{
		{"bad descriptors", []byte("\xff\xff not protobuf"), validOpenAPI, validVersions, fileDescriptors},
		{"bad openapi", fdsBytes(t), "this is not json", validVersions, fileOpenAPI},
		{"bad versions", fdsBytes(t), validOpenAPI, "version: [: :", fileVersions},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeBundle(t, c.desc, c.openapi, c.versions)
			_, err := Load(dir)
			var pe *ParseError
			if !errors.As(err, &pe) || pe.File != c.wantFile {
				t.Fatalf("want ParseError(%s), got %v", c.wantFile, err)
			}
		})
	}
}

func TestUnsupportedVersion(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, "version: 2\ncontracts: []\n")
	_, err := Load(dir)
	var ue *UnsupportedVersionError
	if !errors.As(err, &ue) || ue.Version != 2 {
		t.Fatalf("want UnsupportedVersionError(2), got %v", err)
	}
}

func TestEmptyContractsRejected(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, "version: 1\ncontracts: []\n")
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
}

func TestMissingRequiredFieldRejected(t *testing.T) {
	y := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/me/session
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, y)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "method" {
		t.Fatalf("want ValidationError(field=method), got %v", err)
	}
}

func TestBadMethodRejected(t *testing.T) {
	y := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/me/session
    method: TELEPORT
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, y)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "method" {
		t.Fatalf("want ValidationError(field=method), got %v", err)
	}
}

func TestDuplicateContractVersionRejected(t *testing.T) {
	y := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /a
    method: GET
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
  - contract_version: "2024-11"
    route: /b
    method: GET
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, y)
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError(duplicate), got %v", err)
	}
}

func TestBindingMessageNotInDescriptorsRejected(t *testing.T) {
	y := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /v3/me/session
    method: GET
    request_message: acme.v1.Nope
    response_message: acme.v1.Pong
`
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, y)
	_, err := Load(dir)
	var me *MessageNotFoundError
	if !errors.As(err, &me) || me.Message != "acme.v1.Nope" {
		t.Fatalf("want MessageNotFoundError(acme.v1.Nope), got %v", err)
	}
}

func TestCrossLayerDuplicateContractVersionRejected(t *testing.T) {
	// Both layers declare the same contract_version; Load must reject it.
	mk := func(cv, route string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: " + route + "\n    method: GET\n" +
			"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n"
	}
	dir := t.TempDir()
	for _, l := range []struct{ name, cv, route string }{
		{"2024-11", "2024-11", "/a"},
		{"2026-05", "2024-11", "/b"}, // same contract_version as the first layer
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsBytes(t))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.cv, l.route)))
	}
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError(duplicate across layers), got %v", err)
	}
}

// fdsWithSharedDep builds a FileDescriptorSet wire bytes containing a shared
// file "shared/dep.proto" (package shared, message Dep with a single string
// field named depField) plus one layer-specific file. Varying depField makes
// the shared file's bytes differ between layers.
func fdsWithSharedDep(t *testing.T, layerFile, layerPkg, msg, depField string) []byte {
	t.Helper()
	strField := func(name string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(num),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			JsonName: proto.String(name),
		}
	}
	shared := &descriptorpb.FileDescriptorProto{
		Name: proto.String("shared/dep.proto"), Package: proto.String("shared"),
		Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Dep"), Field: []*descriptorpb.FieldDescriptorProto{strField(depField, 1)}},
		},
	}
	layer := &descriptorpb.FileDescriptorProto{
		Name: proto.String(layerFile), Package: proto.String(layerPkg),
		Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String(msg), Field: []*descriptorpb.FieldDescriptorProto{strField("text", 1)}},
		},
	}
	b, err := proto.Marshal(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{shared, layer},
	})
	if err != nil {
		t.Fatalf("marshal fds: %v", err)
	}
	return b
}

func TestLoadDeduplicatesSharedDependency(t *testing.T) {
	mk := func(cv, msg string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: /x\n    method: GET\n" +
			"    request_message: a." + msg + "\n    response_message: a." + msg + "\n"
	}
	dir := t.TempDir()
	for _, l := range []struct{ name, cv, file, msg string }{
		{"2024-11", "2024-11", "a/one.proto", "One"},
		{"2026-05", "2026-05", "a/two.proto", "Two"},
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsWithSharedDep(t, l.file, "a", l.msg, "v"))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.cv, l.msg)))
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load with an identical shared dependency across layers: %v", err)
	}
}

func TestLoadRejectsConflictingDescriptors(t *testing.T) {
	mk := func(cv, msg string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: /x\n    method: GET\n" +
			"    request_message: a." + msg + "\n    response_message: a." + msg + "\n"
	}
	dir := t.TempDir()
	// Both layers carry shared/dep.proto, but with a different field name —
	// the same file name with conflicting bytes. Load must reject it.
	for _, l := range []struct{ name, cv, file, msg, depField string }{
		{"2024-11", "2024-11", "a/one.proto", "One", "v"},
		{"2026-05", "2026-05", "a/two.proto", "Two", "different"},
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsWithSharedDep(t, l.file, "a", l.msg, l.depField))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.cv, l.msg)))
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load with conflicting definitions of shared/dep.proto: expected an error, got nil")
	}
}

func TestLoadEmptyBundleDirRejected(t *testing.T) {
	_, err := Load(t.TempDir())
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for an empty bundle directory, got %v", err)
	}
}

func TestLoadMissingBundleDirIsReadError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist"))
	var re *ReadError
	if !errors.As(err, &re) {
		t.Fatalf("want ReadError for a missing bundle directory, got %v", err)
	}
}

func TestLoadResolutionTransformOverride(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	resolution := `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request:
        - rename: { from: text, to: text2 }
`
	mustWrite(t, filepath.Join(dir, "resolution.yaml"), []byte(resolution))
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c, ok := b.Contract("2024-11")
	if !ok {
		t.Fatal(`Contract("2024-11") not found`)
	}
	if len(c.RequestOps()) != 1 {
		t.Fatalf("want 1 request op from resolution.yaml, got %d", len(c.RequestOps()))
	}
}

func TestLoadResolutionUnknownVersionRejected(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	mustWrite(t, filepath.Join(dir, "resolution.yaml"), []byte(`version: 1
overrides:
  - contract_version: "nope"
    transform:
      request: []
`))
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for an override on an unknown version, got %v", err)
	}
}

func TestLoadLayerMissingFileIsReadError(t *testing.T) {
	dir := t.TempDir()
	ld := filepath.Join(dir, "2024-11")
	mustMkdir(t, ld)
	// descriptors.binpb deliberately omitted.
	mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
	mustWrite(t, filepath.Join(ld, fileVersions), []byte(validVersions))
	_, err := Load(dir)
	var re *ReadError
	if !errors.As(err, &re) || re.File != fileDescriptors {
		t.Fatalf("want ReadError(%s), got %v", fileDescriptors, err)
	}
}

func TestLoadResolutionUnsupportedVersion(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	mustWrite(t, filepath.Join(dir, "resolution.yaml"), []byte("version: 2\noverrides: []\n"))
	_, err := Load(dir)
	var ue *UnsupportedVersionError
	if !errors.As(err, &ue) {
		t.Fatalf("want UnsupportedVersionError for resolution.yaml version 2, got %v", err)
	}
}

func TestLoadResolutionDuplicateOverrideRejected(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	resolution := `version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request: []
  - contract_version: "2024-11"
    transform:
      response: []
`
	mustWrite(t, filepath.Join(dir, "resolution.yaml"), []byte(resolution))
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for a duplicate resolution override, got %v", err)
	}
}

func TestLoadMultipleLayers(t *testing.T) {
	mk := func(cv, route string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: " + route + "\n    method: GET\n" +
			"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n"
	}
	dir := t.TempDir()
	for _, l := range []struct{ name, cv, route string }{
		{"2024-11", "2024-11", "/a"},
		{"2026-05", "2026-05", "/b"},
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsBytes(t))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.cv, l.route)))
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, cv := range []string{"2024-11", "2026-05"} {
		if _, ok := b.Contract(cv); !ok {
			t.Errorf("Contract(%q) not found in merged bundle", cv)
		}
	}
	if _, err := b.Message("acme.v1.Ping"); err != nil {
		t.Errorf("resolve acme.v1.Ping against merged registry: %v", err)
	}
}
