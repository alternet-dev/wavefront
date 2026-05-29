package bundle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// fdsBytes builds a self-contained FileDescriptorSet (package acme.v1, two
// proto3 string messages) and returns its wire bytes.
func fdsBytes(t *testing.T) []byte {
	t.Helper()
	return fdsBytesPkg(t, "acme/v1/types.proto", "acme.v1", "Ping", "Pong")
}

// fdsBytesPkg builds a self-contained FileDescriptorSet wire bytes with a
// single file under the supplied (file, package) names and two proto3 string
// messages. It is used to construct per-layer disjoint-package fixtures.
func fdsBytesPkg(t *testing.T, fileName, pkg, msgA, msgB string) []byte {
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
			Name:    proto.String(fileName),
			Package: proto.String(pkg),
			Syntax:  proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String(msgA), Field: []*descriptorpb.FieldDescriptorProto{strField("text", 1)}},
				{Name: proto.String(msgB), Field: []*descriptorpb.FieldDescriptorProto{strField("text", 1)}},
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

func TestDuplicateRouteMethodInLayerRejected(t *testing.T) {
	// Two contracts in the same layer with the same (version, route, method)
	// is a true duplicate — the binding is ambiguous. Multi-route layers
	// (different routes at the same contract_version) are legal and covered
	// by TestMultiRouteLayerLoads.
	y := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /a
    method: GET
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
  - contract_version: "2024-11"
    route: /a
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

func TestMultiRouteLayerLoads(t *testing.T) {
	// A multi-route layer — many contracts sharing a contract_version,
	// distinguished by (route, method) — must load cleanly: LookupRoute
	// resolves each binding.
	y := `version: 1
contracts:
  - contract_version: "2024-11"
    route: /a
    method: GET
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
  - contract_version: "2024-11"
    route: /b
    method: POST
    request_message: acme.v1.Ping
    response_message: acme.v1.Pong
`
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, y)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := b.LookupRoute("/a", "GET"); !ok {
		t.Error(`LookupRoute("/a", "GET"): not found in multi-route layer`)
	}
	if _, ok := b.LookupRoute("/b", "POST"); !ok {
		t.Error(`LookupRoute("/b", "POST"): not found in multi-route layer`)
	}
	if vs := b.Versions(); len(vs) != 1 || vs[0] != "2024-11" {
		t.Errorf("Versions() = %v, want [2024-11]", vs)
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

func TestCrossLayerDuplicateRouteMethodRejected(t *testing.T) {
	// Two layers each bind the same (contract_version, route, method) —
	// a true duplicate that makes the binding ambiguous. Load must reject
	// it. Two layers may share a contract_version on different (route,
	// method) bindings, but identical (version, route, method) across
	// layers is invalid.
	mk := func(cv, route string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: " + route + "\n    method: GET\n" +
			"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n"
	}
	dir := t.TempDir()
	for _, l := range []struct{ name, cv, route string }{
		{"2024-11", "2024-11", "/a"},
		{"2026-05", "2024-11", "/a"}, // same (version, route, method) as the first layer
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
	mk := func(cv, pkg, msg string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: /x\n    method: GET\n" +
			"    request_message: " + pkg + "." + msg + "\n    response_message: " + pkg + "." + msg + "\n"
	}
	dir := t.TempDir()
	// Each layer ships a per-version layer-unique file in its own per-version
	// proto package (the wavefront.gen.v<version> convention) plus a
	// bit-identical shared/dep.proto (same bytes). mergeDescriptors collapses
	// the shared file; the package-collision check only fires on multiple
	// layers contributing UNIQUE files in the same package, which is not the
	// case here.
	for _, l := range []struct{ name, cv, file, pkg, msg string }{
		{"2024-11", "2024-11", "gen/v2024_11/one.proto", "wavefront.gen.v2024_11", "One"},
		{"2026-05", "2026-05", "gen/v2026_05/two.proto", "wavefront.gen.v2026_05", "Two"},
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsWithSharedDep(t, l.file, l.pkg, l.msg, "v"))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.cv, l.pkg, l.msg)))
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load with an identical shared dependency across layers: %v", err)
	}
}

func TestLoadRejectsConflictingDescriptors(t *testing.T) {
	mk := func(cv, pkg, msg string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: /x\n    method: GET\n" +
			"    request_message: " + pkg + "." + msg + "\n    response_message: " + pkg + "." + msg + "\n"
	}
	dir := t.TempDir()
	// Both layers carry shared/dep.proto, but with a different field name —
	// the same file name with conflicting bytes. mergeDescriptors must
	// reject it. Layer-unique files use per-version disjoint packages so
	// the package-collision check passes; the conflicting shared dep is
	// the only failure mode under test.
	for _, l := range []struct{ name, cv, file, pkg, msg, depField string }{
		{"2024-11", "2024-11", "gen/v2024_11/one.proto", "wavefront.gen.v2024_11", "One", "v"},
		{"2026-05", "2026-05", "gen/v2026_05/two.proto", "wavefront.gen.v2026_05", "Two", "different"},
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsWithSharedDep(t, l.file, l.pkg, l.msg, l.depField))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.cv, l.pkg, l.msg)))
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

func TestLoadResolutionRouteOverrideSetsTarget(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	mustWrite(t, filepath.Join(dir, "resolution.yaml"), []byte(`version: 1
overrides:
  - contract_version: "2024-11"
    route:
      target: backend-v2
`))
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c, ok := b.Contract("2024-11")
	if !ok {
		t.Fatal(`Contract("2024-11") not found`)
	}
	if c.Target() != "backend-v2" {
		t.Errorf("Target() = %q, want \"backend-v2\"", c.Target())
	}
	if len(c.RequestOps()) != 0 || len(c.ResponseOps()) != 0 {
		t.Errorf("route override must not set transform ops: req=%d resp=%d", len(c.RequestOps()), len(c.ResponseOps()))
	}
}

func TestLoadResolutionBothTransformAndRouteRejected(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	mustWrite(t, filepath.Join(dir, "resolution.yaml"), []byte(`version: 1
overrides:
  - contract_version: "2024-11"
    transform:
      request: []
    route:
      target: svc
`))
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for both transform+route set, got %v", err)
	}
}

func TestLoadResolutionNeitherTransformNorRouteRejected(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	mustWrite(t, filepath.Join(dir, "resolution.yaml"), []byte(`version: 1
overrides:
  - contract_version: "2024-11"
`))
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for neither transform nor route set, got %v", err)
	}
}

func TestLoadResolutionRouteBlankTargetRejected(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	mustWrite(t, filepath.Join(dir, "resolution.yaml"), []byte(`version: 1
overrides:
  - contract_version: "2024-11"
    route:
      target: ""
`))
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError for blank route.target, got %v", err)
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

// TestLookupRoute covers the bundle's route-membership check used by the
// proxy as the first-pass routing gate. Path-method pairs that any contract
// binds must hit (and return a non-nil *Contract); everything else (unknown
// path, known path with wrong method, case-sensitive variants) must miss
// (and return nil). The test deliberately exercises the wrong-method case:
// each contract names exactly one method, so a path bound to GET must miss
// when queried for POST.
func TestLookupRoute(t *testing.T) {
	mk := func(cv, route, method string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: " + route + "\n    method: " + method + "\n" +
			"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n"
	}
	dir := t.TempDir()
	for _, l := range []struct{ name, cv, route, method string }{
		{"2024-11", "2024-11", "/v3/echo", "POST"},
		{"2025-01", "2025-01", "/v3/items", "GET"},
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsBytes(t))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.cv, l.route, l.method)))
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := []struct {
		path, method string
		want         bool
	}{
		{"/v3/echo", "POST", true},
		{"/v3/items", "GET", true},
		// Wrong method on a known path — must miss. v0.5 folds this into
		// unknown_route at the proxy boundary.
		{"/v3/echo", "GET", false},
		{"/v3/items", "POST", false},
		{"/v3/items", "DELETE", false},
		// Entirely unknown path.
		{"/nope", "GET", false},
		{"/", "GET", false},
		// Case sensitivity: methods are normalized at load time to upper-case;
		// the proxy receives them already uppercase from net/http, so a
		// lowercase query must miss (defensive — should never happen at
		// runtime).
		{"/v3/echo", "post", false},
		{"/V3/ECHO", "POST", false},
	}
	for _, c := range cases {
		got, ok := b.LookupRoute(c.path, c.method)
		if ok != c.want {
			t.Errorf("LookupRoute(%q, %q) ok = %v, want %v", c.path, c.method, ok, c.want)
		}
		if c.want && got == nil {
			t.Errorf("LookupRoute(%q, %q): want non-nil contract on hit, got nil", c.path, c.method)
		}
		if !c.want && got != nil {
			t.Errorf("LookupRoute(%q, %q): want nil contract on miss, got %+v", c.path, c.method, got)
		}
	}
}

// TestLookupRouteMultipleContractsSamePath exercises the documented case
// where two contracts in different layers bind the same (route, method)
// under different contract_version values. LookupRoute must return one of
// them; which one is the version-negotiation layer's problem, not this
// layer's, so the test only checks that the returned contract is one of
// the two known matches.
func TestLookupRouteMultipleContractsSamePath(t *testing.T) {
	mk := func(cv string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: /v3/items\n    method: GET\n" +
			"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n"
	}
	dir := t.TempDir()
	for _, cv := range []string{"2024-11", "2025-01"} {
		ld := filepath.Join(dir, cv)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsBytes(t))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(cv)))
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := b.LookupRoute("/v3/items", "GET")
	if !ok || got == nil {
		t.Fatalf("LookupRoute hit expected, got ok=%v contract=%v", ok, got)
	}
	if got.ContractVersion() != "2024-11" && got.ContractVersion() != "2025-01" {
		t.Errorf("LookupRoute returned a contract not among the two known matches: %q", got.ContractVersion())
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

// Versions exposes the loaded contract versions in lexical-ascending order.
// Downstream tooling (gen-ts-client, etc.) needs a stable list to pick the
// latest layer (the lexically-highest version) and to render a "available:
// ..." hint when a caller pins an unknown version.
func TestVersionsReturnsAllContractsLexicalAscending(t *testing.T) {
	mk := func(cv, route string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: " + route + "\n    method: GET\n" +
			"    request_message: acme.v1.Ping\n    response_message: acme.v1.Pong\n"
	}
	dir := t.TempDir()
	// Write layers in non-sorted creation order to confirm Versions() does the
	// sort itself (does not rely on directory-entry order at the call site).
	for _, l := range []struct{ name, cv, route string }{
		{"2026-05", "2026-05", "/b"},
		{"2024-11", "2024-11", "/a"},
		{"2025-03", "2025-03", "/c"},
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
	got := b.Versions()
	want := []string{"2024-11", "2025-03", "2026-05"}
	if len(got) != len(want) {
		t.Fatalf("Versions() len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Versions()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestVersionsSingleLayer(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := b.Versions()
	if len(got) != 1 || got[0] != "2024-11" {
		t.Errorf("Versions() = %v, want [2024-11]", got)
	}
}

// --- proto-package collision: defense-in-depth check ---

// TestLoadDisjointPackagesAcrossLayers is the happy path for the
// package-collision check: two layers, each with its own per-version proto
// package, load successfully. This mirrors the convention the docs spell
// out (wavefront.gen.v<version>).
func TestLoadDisjointPackagesAcrossLayers(t *testing.T) {
	mk := func(cv, pkg string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: /v3/echo\n    method: GET\n" +
			"    request_message: " + pkg + ".Ping\n    response_message: " + pkg + ".Pong\n"
	}
	dir := t.TempDir()
	for _, l := range []struct{ name, cv, file, pkg string }{
		{"2024-11", "2024-11", "gen/v2024_11/types.proto", "wavefront.gen.v2024_11"},
		{"2025-03", "2025-03", "gen/v2025_03/types.proto", "wavefront.gen.v2025_03"},
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsBytesPkg(t, l.file, l.pkg, "Ping", "Pong"))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.cv, l.pkg)))
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load with disjoint per-version proto packages: %v", err)
	}
}

// TestLoadPackageCollisionRejected is the unhappy path: two layers each
// uniquely contribute types into the same proto package. Load must return
// a typed *PackageCollisionError naming that package and both layer names
// in sorted order, with a message that suggests the per-version namespacing
// fix.
func TestLoadPackageCollisionRejected(t *testing.T) {
	mk := func(cv string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: /v3/echo\n    method: GET\n" +
			"    request_message: wavefront.gen.shared.Ping\n" +
			"    response_message: wavefront.gen.shared.Pong\n"
	}
	dir := t.TempDir()
	// Two layers each ship a UNIQUE file (different file names) that lands
	// in the same proto package "wavefront.gen.shared". The bytes differ
	// (different message names) so mergeDescriptors' file-level dedup does
	// not collapse them; the package-collision check must fire.
	for _, l := range []struct{ name, file, msgA, msgB string }{
		{"2024-11", "gen/shared/types_a.proto", "Ping", "Pong"},
		{"2025-03", "gen/shared/types_b.proto", "Ping", "Pong"},
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsBytesPkg(t, l.file, "wavefront.gen.shared", l.msgA, l.msgB))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.name)))
	}
	_, err := Load(dir)
	var pce *PackageCollisionError
	if !errors.As(err, &pce) {
		t.Fatalf("want *PackageCollisionError, got %v", err)
	}
	if pce.Package != "wavefront.gen.shared" {
		t.Errorf("Package = %q, want %q", pce.Package, "wavefront.gen.shared")
	}
	wantLayers := []string{"2024-11", "2025-03"}
	if len(pce.Layers) != len(wantLayers) {
		t.Fatalf("Layers = %v, want %v", pce.Layers, wantLayers)
	}
	for i := range wantLayers {
		if pce.Layers[i] != wantLayers[i] {
			t.Errorf("Layers[%d] = %q, want %q", i, pce.Layers[i], wantLayers[i])
		}
	}
	// The message must name the colliding package and suggest the fix.
	msg := pce.Error()
	for _, want := range []string{"wavefront.gen.shared", "per-version"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}

// TestLoadPackageCollisionAcrossThreeLayers exercises the case where three
// layers all unique-contribute files into the same proto package. The error
// must enumerate all three layers in sorted order (not just the first two
// encountered).
func TestLoadPackageCollisionAcrossThreeLayers(t *testing.T) {
	mk := func(cv string) string {
		return "version: 1\ncontracts:\n  - contract_version: \"" + cv + "\"\n" +
			"    route: /v3/echo\n    method: GET\n" +
			"    request_message: wavefront.gen.shared.Ping\n" +
			"    response_message: wavefront.gen.shared.Pong\n"
	}
	dir := t.TempDir()
	for _, l := range []struct{ name, file string }{
		{"2025-03", "gen/shared/types_b.proto"},
		{"2026-11", "gen/shared/types_c.proto"},
		{"2024-11", "gen/shared/types_a.proto"},
	} {
		ld := filepath.Join(dir, l.name)
		mustMkdir(t, ld)
		mustWrite(t, filepath.Join(ld, fileDescriptors), fdsBytesPkg(t, l.file, "wavefront.gen.shared", "Ping", "Pong"))
		mustWrite(t, filepath.Join(ld, fileOpenAPI), []byte(validOpenAPI))
		mustWrite(t, filepath.Join(ld, fileVersions), []byte(mk(l.name)))
	}
	_, err := Load(dir)
	var pce *PackageCollisionError
	if !errors.As(err, &pce) {
		t.Fatalf("want *PackageCollisionError, got %v", err)
	}
	want := []string{"2024-11", "2025-03", "2026-11"}
	if len(pce.Layers) != len(want) {
		t.Fatalf("Layers = %v, want %v", pce.Layers, want)
	}
	for i := range want {
		if pce.Layers[i] != want[i] {
			t.Errorf("Layers[%d] = %q, want %q", i, pce.Layers[i], want[i])
		}
	}
}

// TestLoadSingleLayerCannotCollide asserts that a single-layer bundle (the
// most common shape) can never trip the package-collision check, no matter
// how many files / packages it contains.
func TestLoadSingleLayerCannotCollide(t *testing.T) {
	dir := writeBundle(t, fdsBytes(t), validOpenAPI, validVersions)
	if _, err := Load(dir); err != nil {
		t.Fatalf("single-layer bundle must not collide: %v", err)
	}
}
