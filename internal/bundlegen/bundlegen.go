// Package bundlegen is the OpenAPI → bundle generator. It is deliberately a
// constrained, fail-loud subset: request/response must be $ref'd component
// object schemas, scalars + arrays + $ref + nullable→proto3-optional are
// supported, and any unsupported construct
// (allOf/oneOf/anyOf/additionalProperties/untyped/inline) is a hard error
// rather than a lossy bundle. Output is byte-reproducible (sorted,
// deterministic marshal) for the consumer's buf-breaking story.
//
// Add emits one immutable layer per call. It walks every
// paths.<path>.<method> operation in the input OpenAPI document, sorted by
// (path, method) ascending for byte-reproducibility, and emits one
// contract per operation sharing the same contract_version. The
// descriptor set is the union of every referenced request/response
// schema, deduped. A document with a single operation is the degenerate
// case — one operation, one contract.
package bundlegen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"

	"github.com/alternet-dev/wavefront/internal/bundle"
)

type schema struct {
	Ref                  string             `json:"$ref"`
	Type                 string             `json:"type"`
	Format               string             `json:"format"`
	Nullable             bool               `json:"nullable"`
	Properties           map[string]*schema `json:"properties"`
	Items                *schema            `json:"items"`
	AllOf                []*schema          `json:"allOf"`
	OneOf                []*schema          `json:"oneOf"`
	AnyOf                []*schema          `json:"anyOf"`
	AdditionalProperties json.RawMessage    `json:"additionalProperties"`
}

type mediaType struct {
	Schema *schema `json:"schema"`
}

type operation struct {
	OperationID string `json:"operationId"`
	RequestBody *struct {
		Content map[string]mediaType `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Content map[string]mediaType `json:"content"`
	} `json:"responses"`
}

type openAPI struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]*schema `json:"schemas"`
	} `json:"components"`
}

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true,
	"delete": true, "head": true, "options": true,
}

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9_]`)

const (
	openapiFetchTimeout = 30 * time.Second
	maxOpenAPIBytes     = 32 << 20 // defensive cap on a fetched/loaded OpenAPI doc
)

// readOpenAPI loads the OpenAPI document from a local file path, or fetches
// it when src is an http(s):// URL (GET, bounded timeout, 2xx required). A
// URL fetch is still frozen at build time: Add passes these bytes through into
// the committed bundle, so the point-in-time guarantee holds.
func readOpenAPI(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		client := &http.Client{Timeout: openapiFetchTimeout}
		resp, err := client.Get(src)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", src, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("fetch %s: status %d", src, resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, maxOpenAPIBytes))
	}
	return os.ReadFile(src)
}

// Add reads the OpenAPI doc from openapiSrc — a local file path or an
// http(s):// URL (fetched at build time; the bytes are passed through into
// the committed bundle, so a URL fetch stays point-in-time) — and emits a
// new immutable layer into bundleDir/<version>/, where <version> is the
// doc's info.version. Every paths.<path>.<method> operation becomes one
// contract; all contracts share the same contract_version. The descriptor
// set is the union of every referenced request/response schema across all
// operations (deduped). The fail-loud doctrine applies: any unsupported
// construct in any walked operation rejects the whole bundle, not just
// that operation.
//
// By default, Add refuses to overwrite an existing layer: a frozen version
// is never rewritten. When force is true, an existing same-version layer
// dir is replaced wholesale — the pre-commit-iteration escape hatch for a
// caller still refining the OpenAPI. Once a layer is committed and
// consumers depend on it, the default (force=false) guards against
// accidental clobbering.
func Add(openapiSrc, bundleDir string, force bool) error {
	raw, doc, version, err := loadAndPrevalidate(openapiSrc)
	if err != nil {
		return err
	}

	ops, err := allOperations(doc)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return fmt.Errorf("openapi has no operations; a bundle layer must contain at least one contract")
	}

	// Collect (request, response) schema names per operation; build the
	// roots slice in (path, method) order so collectSchemas walks
	// deterministically.
	pkg := protoPackage(version)
	roots := make([]string, 0, 2*len(ops))
	entries := make([]contractEntry, 0, len(ops))
	for _, o := range ops {
		reqName, rerr := refSchemaName(o.op, true)
		if rerr != nil {
			return fmt.Errorf("%s %s: %w", strings.ToUpper(o.method), o.route, rerr)
		}
		respName, rerr := refSchemaName(o.op, false)
		if rerr != nil {
			return fmt.Errorf("%s %s: %w", strings.ToUpper(o.method), o.route, rerr)
		}
		roots = append(roots, reqName, respName)
		entries = append(entries, contractEntry{
			Route:           o.route,
			Method:          strings.ToUpper(o.method),
			RequestMessage:  pkg + "." + reqName,
			ResponseMessage: pkg + "." + respName,
		})
	}

	descBytes, err := buildDescriptors(doc, roots, pkg, version)
	if err != nil {
		return err
	}

	versions := renderVersionsYAML(version, entries)
	return writeLayer(bundleDir, version, raw, descBytes, versions, force)
}

// loadAndPrevalidate fetches/reads the OpenAPI source, parses it, and
// validates info.version is present and usable as a layer directory name.
// It returns the raw bytes (passed through verbatim into the committed
// bundle), the parsed doc, and the trimmed contract version.
func loadAndPrevalidate(openapiSrc string) (raw []byte, doc openAPI, version string, err error) {
	raw, err = readOpenAPI(openapiSrc)
	if err != nil {
		return nil, openAPI{}, "", fmt.Errorf("read openapi: %w", err)
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		return nil, openAPI{}, "", fmt.Errorf("parse openapi: %w", err)
	}
	version = strings.TrimSpace(doc.Info.Version)
	if version == "" {
		return nil, openAPI{}, "", fmt.Errorf("openapi info.version is required (it is the contract version)")
	}
	if !safeLayerName(version) {
		return nil, openAPI{}, "", fmt.Errorf("contract version %q is not a usable directory name", version)
	}
	return raw, doc, version, nil
}

// protoPackage is the proto package convention: one package per contract
// version, so the bundle's package-collision check across layers is
// honoured.
func protoPackage(version string) string {
	return "wavefront.gen.v" + sanitize(version)
}

// buildDescriptors walks every schema reachable from roots, validates each
// touched schema against the fail-loud doctrine, and returns the
// deterministic wire bytes of a FileDescriptorSet that holds one
// FileDescriptorProto for the package.
func buildDescriptors(doc openAPI, roots []string, pkg, version string) ([]byte, error) {
	// The synthetic Empty message has no OpenAPI component, so split it out
	// before walking the document and inject it directly below.
	realRoots, needEmpty := partitionEmpty(roots)
	needed, err := collectSchemas(doc, realRoots)
	if err != nil {
		return nil, err
	}

	// Sort schema names so descriptor message order is stable: the
	// byte-reproducibility invariant the consumer's buf-breaking gate
	// relies on. The synthetic Empty is sorted in alongside the real ones
	// (skipped if a real component already claims the name).
	names := make([]string, 0, len(needed)+1)
	for n := range needed {
		names = append(names, n)
	}
	if _, claimed := needed[emptyMessageName]; needEmpty && !claimed {
		names = append(names, emptyMessageName)
	}
	sort.Strings(names)

	msgs := make([]*descriptorpb.DescriptorProto, 0, len(names))
	for _, n := range names {
		if needed[n] == nil {
			// Synthetic zero-field Empty message.
			msgs = append(msgs, &descriptorpb.DescriptorProto{Name: proto.String(n)})
			continue
		}
		dp, derr := buildMessage(n, needed[n], pkg)
		if derr != nil {
			return nil, derr
		}
		msgs = append(msgs, dp)
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("wavefront/gen/" + sanitize(version) + ".proto"),
		Package:     proto.String(pkg),
		Syntax:      proto.String("proto3"),
		MessageType: msgs,
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	if _, err := protodesc.NewFiles(fds); err != nil {
		return nil, fmt.Errorf("generated descriptors are invalid: %w", err)
	}
	descBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(fds)
	if err != nil {
		return nil, fmt.Errorf("marshal descriptors: %w", err)
	}
	return descBytes, nil
}

// contractEntry is one row in versions.yaml.contracts. Fields are written
// in struct-declaration order so the marshalled output is stable.
type contractEntry struct {
	Route           string
	Method          string
	RequestMessage  string
	ResponseMessage string
}

// renderVersionsYAML emits the versions.yaml body for one layer. The
// printf-style template (rather than yaml.Marshal) keeps key order under
// the generator's control — yaml.Marshal of a struct preserves struct field
// order but yaml.Marshal of a map does not, and this same shape has to be
// byte-reproducible run-to-run.
func renderVersionsYAML(version string, entries []contractEntry) string {
	var b strings.Builder
	b.WriteString("version: 1\ncontracts:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "  - contract_version: %q\n", version)
		fmt.Fprintf(&b, "    route: %s\n", e.Route)
		fmt.Fprintf(&b, "    method: %s\n", e.Method)
		fmt.Fprintf(&b, "    request_message: %s\n", e.RequestMessage)
		fmt.Fprintf(&b, "    response_message: %s\n", e.ResponseMessage)
	}
	return b.String()
}

// writeLayer emits one layer's three files into bundleDir/<version>/. By
// default it refuses to overwrite a pre-existing layer: a frozen version
// is never rewritten. When force is true, an existing same-version layer
// dir is removed before the new one is written — the pre-commit-iteration
// escape hatch.
//
// The forced removal goes through a defensive sanity check: the deletion
// target must be exactly one path segment under bundleDir and must be a
// directory. The check is belt-and-suspenders against a bug or future
// caller that passes an unexpected version string; safeLayerName upstream
// already rejects path separators and "." / "..", but os.RemoveAll is
// destructive enough that one extra guard is worth the line.
func writeLayer(bundleDir, version string, openapiRaw, descBytes []byte, versions string, force bool) error {
	layerDir := filepath.Join(bundleDir, version)
	info, err := os.Stat(layerDir)
	if err == nil {
		if !force {
			return fmt.Errorf("version %q already exists in the bundle; frozen versions are immutable", version)
		}
		if !info.IsDir() {
			return fmt.Errorf("refusing to overwrite %s: not a directory", layerDir)
		}
		if filepath.Dir(layerDir) != filepath.Clean(bundleDir) {
			return fmt.Errorf("refusing to overwrite %s: not a direct child of bundle dir %s", layerDir, bundleDir)
		}
		if err := os.RemoveAll(layerDir); err != nil {
			return fmt.Errorf("remove existing layer %s: %w", layerDir, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", layerDir, err)
	}
	if err := os.MkdirAll(layerDir, 0o755); err != nil {
		return fmt.Errorf("mkdir layer: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layerDir, "descriptors.binpb"), descBytes, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(layerDir, "openapi.json"), openapiRaw, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(layerDir, "versions.yaml"), []byte(versions), 0o644)
}

// Remove hard-deletes a version's layer from bundleDir. It is oldest-only:
// version must be the lexically-oldest layer present, and at least one other
// layer must remain — the bundle is never emptied and the current version is
// never removed. Any other request is a hard error.
func Remove(bundleDir, version string) error {
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		return fmt.Errorf("read bundle dir: %w", err)
	}
	var layers []string
	for _, e := range entries {
		if e.IsDir() {
			layers = append(layers, e.Name())
		}
	}
	// os.ReadDir returns entries sorted by name, so layers is already in
	// lexical order: layers[0] is the oldest.
	if len(layers) < 2 {
		return fmt.Errorf("refusing to remove %q: a bundle must keep at least one layer", version)
	}
	if version != layers[0] {
		return fmt.Errorf("refusing to remove %q: only the oldest layer (%q) may be removed", version, layers[0])
	}
	return os.RemoveAll(filepath.Join(bundleDir, version))
}

// safeLayerName reports whether name is usable as a single path segment: a
// non-empty string, not "." or "..", with no path separator and no control
// bytes.
func safeLayerName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7F {
			return false
		}
	}
	return true
}

// foundOperation is one walked operation: its path, lowercased method, and
// parsed operation body.
type foundOperation struct {
	route, method string
	op            operation
}

// allOperations walks every paths.<path>.<method> entry in doc, parses each
// operation body, and returns the result sorted by (route, method)
// ascending. The sort is the byte-reproducibility invariant: the
// FileDescriptorSet's message order and versions.yaml's contract order
// depend on a stable input order.
func allOperations(doc openAPI) ([]foundOperation, error) {
	var ops []foundOperation
	for p, item := range doc.Paths {
		for m, rawOp := range item {
			if !httpMethods[strings.ToLower(m)] {
				continue
			}
			var o operation
			if e := json.Unmarshal(rawOp, &o); e != nil {
				return nil, fmt.Errorf("parse operation %s %s: %w", m, p, e)
			}
			ops = append(ops, foundOperation{p, strings.ToLower(m), o})
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].route != ops[j].route {
			return ops[i].route < ops[j].route
		}
		return ops[i].method < ops[j].method
	})
	return ops, nil
}

// emptyMessageName is the synthetic message bound by a bodyless side of an
// operation — a GET/path-only POST with no requestBody, or a response with
// no 200 body (e.g. an HTTP 204). One zero-field `Empty` message is emitted
// per layer into the version's package and shared by every bodyless side.
const emptyMessageName = "Empty"

// refSchemaName resolves the component-schema name a side of an operation
// binds to. A bodyless side — no requestBody on the request side, or no 200
// response on the response side — binds the synthetic Empty message; the
// fail-loud rejection of inline (non-$ref) schemas still applies to bodied
// sides.
func refSchemaName(op operation, request bool) (string, error) {
	var mt map[string]mediaType
	if request {
		if op.RequestBody == nil {
			return emptyMessageName, nil
		}
		mt = op.RequestBody.Content
	} else {
		resp, ok := op.Responses["200"]
		if !ok {
			return emptyMessageName, nil
		}
		mt = resp.Content
	}
	m, ok := mt["application/json"]
	if !ok || m.Schema == nil {
		return "", fmt.Errorf("operation has no application/json schema")
	}
	if m.Schema.Ref == "" {
		return "", fmt.Errorf("request/response schema must be a $ref to #/components/schemas (inline schemas are unsupported)")
	}
	return refName(m.Schema.Ref), nil
}

// partitionEmpty splits roots into the real component-schema names (which
// collectSchemas resolves from the OpenAPI document) and a flag reporting
// whether the synthetic Empty message is referenced. Empty has no OpenAPI
// component backing it, so it must be excluded from the collectSchemas walk
// and injected at descriptor-build time instead.
func partitionEmpty(roots []string) (kept []string, needEmpty bool) {
	kept = make([]string, 0, len(roots))
	for _, r := range roots {
		if r == emptyMessageName {
			needEmpty = true
			continue
		}
		kept = append(kept, r)
	}
	return kept, needEmpty
}

func refName(ref string) string {
	return ref[strings.LastIndex(ref, "/")+1:]
}

// collectSchemas walks from the roots, validating each touched schema, and
// returns every component object schema that must become a message.
func collectSchemas(doc openAPI, roots []string) (map[string]*schema, error) {
	out := map[string]*schema{}
	var visit func(name string) error
	visit = func(name string) error {
		if _, done := out[name]; done {
			return nil
		}
		sc := doc.Components.Schemas[name]
		if sc == nil {
			return fmt.Errorf("schema %q referenced but not defined", name)
		}
		if err := rejectUnsupported(sc); err != nil {
			return fmt.Errorf("schema %q: %w", name, err)
		}
		if sc.Type != "object" || len(sc.Properties) == 0 {
			return fmt.Errorf("schema %q must be an object with properties", name)
		}
		out[name] = sc
		for _, prop := range sc.Properties {
			if err := rejectUnsupported(prop); err != nil {
				return fmt.Errorf("schema %q property: %w", name, err)
			}
			switch {
			case prop.Ref != "":
				if err := visit(refName(prop.Ref)); err != nil {
					return err
				}
			case prop.Type == "array" && prop.Items != nil && prop.Items.Ref != "":
				if err := visit(refName(prop.Items.Ref)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, r := range roots {
		if err := visit(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func rejectUnsupported(sc *schema) error {
	switch {
	case len(sc.AllOf) > 0:
		return fmt.Errorf("allOf is unsupported")
	case len(sc.OneOf) > 0:
		return fmt.Errorf("oneOf is unsupported")
	case len(sc.AnyOf) > 0:
		return fmt.Errorf("anyOf is unsupported")
	case sc.AdditionalProperties != nil && !additionalPropertiesFalse(sc.AdditionalProperties):
		return fmt.Errorf("additionalProperties is unsupported (only the no-op additionalProperties: false is accepted; true and typed-dict {schema} forms are not)")
	}
	return nil
}

// additionalPropertiesFalse reports whether the additionalProperties value
// is the literal boolean false — "no properties beyond those declared",
// which is exactly how a closed proto message already behaves. Such a
// constraint carries no information and is treated as absent. The loose
// form (true) and the typed-dict form ({<schema>}, i.e. Dict[str, X]) are
// genuine semantic differences and stay rejected.
func additionalPropertiesFalse(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("false"))
}

func buildMessage(name string, sc *schema, pkg string) (*descriptorpb.DescriptorProto, error) {
	props := make([]string, 0, len(sc.Properties))
	for p := range sc.Properties {
		props = append(props, p)
	}
	sort.Strings(props)

	dp := &descriptorpb.DescriptorProto{Name: proto.String(name)}
	num := int32(1)
	for _, pname := range props {
		f, optional, err := buildField(pname, num, sc.Properties[pname], pkg)
		if err != nil {
			return nil, fmt.Errorf("message %q field %q: %w", name, pname, err)
		}
		if optional {
			idx := int32(len(dp.OneofDecl))
			dp.OneofDecl = append(dp.OneofDecl, &descriptorpb.OneofDescriptorProto{
				Name: proto.String("_" + pname),
			})
			f.OneofIndex = proto.Int32(idx)
			f.Proto3Optional = proto.Bool(true)
		}
		dp.Field = append(dp.Field, f)
		num++
	}
	return dp, nil
}

func buildField(pname string, num int32, sc *schema, pkg string) (*descriptorpb.FieldDescriptorProto, bool, error) {
	f := &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(pname),
		Number:   proto.Int32(num),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		JsonName: proto.String(pname),
	}

	if sc.Ref != "" {
		f.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
		f.TypeName = proto.String("." + pkg + "." + refName(sc.Ref))
		return f, false, nil
	}

	switch sc.Type {
	case "string":
		f.Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	case "boolean":
		f.Type = descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()
	case "integer":
		if sc.Format == "int64" {
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
		} else {
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()
		}
	case "number":
		if sc.Format == "float" {
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_FLOAT.Enum()
		} else {
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_DOUBLE.Enum()
		}
	case "array":
		if sc.Items == nil {
			return nil, false, fmt.Errorf("array without items")
		}
		f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		if sc.Items.Ref != "" {
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
			f.TypeName = proto.String("." + pkg + "." + refName(sc.Items.Ref))
			return f, false, nil
		}
		switch sc.Items.Type {
		case "string":
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
		case "boolean":
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()
		case "integer":
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
		case "number":
			f.Type = descriptorpb.FieldDescriptorProto_TYPE_DOUBLE.Enum()
		default:
			return nil, false, fmt.Errorf("array item type %q is unsupported", sc.Items.Type)
		}
		return f, false, nil
	case "object":
		return nil, false, fmt.Errorf("inline object properties are unsupported (use a $ref)")
	case "":
		return nil, false, fmt.Errorf("property has no type and no $ref")
	default:
		return nil, false, fmt.Errorf("type %q is unsupported", sc.Type)
	}

	return f, sc.Nullable, nil
}

func sanitize(version string) string {
	s := nonIdent.ReplaceAllString(version, "_")
	if s == "" {
		s = "x"
	}
	if s[0] >= '0' && s[0] <= '9' {
		return s // already prefixed by the caller's "v"
	}
	return s
}

// --- resolution.yaml helpers for Retire ---
//
// These structs mirror the on-disk shape just enough to detect existing
// overrides and append a new transform-shim stanza. The request/response
// stanza arrays are held as raw yaml.Node so that existing verb lists are
// round-tripped without losing their values. Note: operator comments in
// resolution.yaml are NOT preserved across a retire rewrite.

type resolutionFile struct {
	Version   int               `yaml:"version"`
	Overrides []resolutionEntry `yaml:"overrides"`
}

type resolutionEntry struct {
	ContractVersion string               `yaml:"contract_version"`
	Transform       *resolutionTransform `yaml:"transform,omitempty"`
	Route           *resolutionRoute     `yaml:"route,omitempty"`
}

type resolutionTransform struct {
	Target   string    `yaml:"target"`
	Request  yaml.Node `yaml:"request"`
	Response yaml.Node `yaml:"response"`
}

type resolutionRoute struct {
	Target string `yaml:"target"`
}

// emptySeqNode returns a YAML sequence node with no items (marshals as []).
func emptySeqNode() yaml.Node {
	return yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
}

// Retire scaffolds a transform-shim override for version in bundleDir. It
// converts version from the default (direct-route) behaviour to a transform
// that chains to its immediate newer neighbour, leaving the request and
// response stanza arrays empty for a human to fill.
//
// Hard errors:
//   - version is not a layer in bundleDir
//   - version is the newest layer (the current version is never retired)
//   - version already has an override in resolution.yaml
//
// Any existing overrides for other versions are preserved verbatim.
// Operator comments in resolution.yaml are not preserved across the rewrite.
func Retire(bundleDir, version string) error {
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		return fmt.Errorf("read bundle dir: %w", err)
	}
	var layers []string
	for _, e := range entries {
		if e.IsDir() {
			layers = append(layers, e.Name())
		}
	}
	// os.ReadDir returns entries sorted by name.
	found := false
	idx := -1
	for i, l := range layers {
		if l == version {
			found = true
			idx = i
			break
		}
	}
	if !found {
		return fmt.Errorf("version %q is not a layer in the bundle", version)
	}
	if idx == len(layers)-1 {
		return fmt.Errorf("refusing to retire the current version %q", version)
	}
	neighbour := layers[idx+1]

	// Read existing resolution.yaml if present.
	resPath := filepath.Join(bundleDir, "resolution.yaml")
	var rf resolutionFile
	raw, err := os.ReadFile(resPath)
	if err != nil && !isNotExist(err) {
		return fmt.Errorf("read resolution.yaml: %w", err)
	}
	if err == nil {
		// File exists — parse it.
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		if derr := dec.Decode(&rf); derr != nil {
			return fmt.Errorf("parse resolution.yaml: %w", derr)
		}
	} else {
		// File absent — start with schema version 1 and no overrides.
		rf.Version = 1
	}

	// Check for a pre-existing override for version.
	for _, ov := range rf.Overrides {
		if ov.ContractVersion == version {
			return fmt.Errorf("version %q already has an override in resolution.yaml; retire only scaffolds a fresh override", version)
		}
	}

	// Append the transform-shim override.
	rf.Overrides = append(rf.Overrides, resolutionEntry{
		ContractVersion: version,
		Transform: &resolutionTransform{
			Target:   neighbour,
			Request:  emptySeqNode(),
			Response: emptySeqNode(),
		},
	})

	out, merr := yaml.Marshal(&rf)
	if merr != nil {
		return fmt.Errorf("marshal resolution.yaml: %w", merr)
	}
	return os.WriteFile(resPath, out, 0o644)
}

func isNotExist(err error) bool {
	return err != nil && (os.IsNotExist(err) || errors.Is(err, fs.ErrNotExist))
}

// Verify loads the bundle at bundleDir via bundle.Load and returns the typed
// error on any inconsistency, or nil on success. It is the thin command-facing
// wrapper that gives a CI consumer a single pass/fail over layers,
// resolution.yaml, and the transform chains. It also refuses a
// resolution.yaml that still carries an unresolved drift-shim candidates
// block — a draft pasted in unedited is a no-op override at runtime, but
// shipping one signals the operator never picked an OPTION A / B and is
// almost always a mistake.
func Verify(bundleDir string) error {
	if _, err := bundle.Load(bundleDir); err != nil {
		return err
	}
	return verifyNoUnresolvedCandidates(bundleDir)
}

// candidatesBlockMarker is the header `ShimProposal.RenderYAML` emits at
// the top of every ambiguous candidates block. Its presence in a
// committed resolution.yaml means a draft was pasted in but never
// resolved — the operator left both OPTION A and OPTION B sitting in
// the comment lane.
const candidatesBlockMarker = "# AMBIGUOUS pair"

func verifyNoUnresolvedCandidates(bundleDir string) error {
	path := filepath.Join(bundleDir, "resolution.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read resolution.yaml: %w", err)
	}
	if bytes.Contains(raw, []byte(candidatesBlockMarker)) {
		return fmt.Errorf("resolution.yaml contains an unresolved drift-shim candidates block (look for %q); pick one option, edit the stanzas, and remove the comment block before committing", candidatesBlockMarker)
	}
	return nil
}
