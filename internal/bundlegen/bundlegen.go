// Package bundlegen is the OpenAPI → bundle generator. v0.1 is deliberately a
// constrained, fail-loud subset: exactly one operation (single-version
// passthrough), request/response must be $ref'd component object schemas,
// scalars + arrays + $ref + nullable→proto3-optional are supported, and any
// unsupported construct (allOf/oneOf/anyOf/additionalProperties/untyped/
// inline) is a hard error rather than a lossy bundle. Transform-stanza
// emission rolls to v0.2 in lockstep with runtime transform support. Output is
// byte-reproducible (sorted, deterministic marshal) for the consumer's
// buf-breaking story.
package bundlegen

import (
	"encoding/json"
	"fmt"
	"io"
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
// URL fetch is still frozen at build time: Generate passes these bytes
// through into the committed bundle, so the point-in-time guarantee holds.
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

// Generate reads the OpenAPI doc from openapiSrc — a local file path or an
// http(s):// URL (fetched at build time; the bytes are passed through into
// the committed bundle, so a URL fetch stays point-in-time) — and writes
// descriptors.binpb, openapi.json, and versions.yaml into outDir.
func Generate(openapiSrc, outDir string) error {
	raw, err := readOpenAPI(openapiSrc)
	if err != nil {
		return fmt.Errorf("read openapi: %w", err)
	}
	var doc openAPI
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse openapi: %w", err)
	}
	if strings.TrimSpace(doc.Info.Version) == "" {
		return fmt.Errorf("openapi info.version is required (it is the contract version)")
	}

	route, method, op, err := singleOperation(doc)
	if err != nil {
		return err
	}
	reqName, err := refSchemaName(op, true)
	if err != nil {
		return err
	}
	respName, err := refSchemaName(op, false)
	if err != nil {
		return err
	}

	pkg := "wavefront.gen.v" + sanitize(doc.Info.Version)
	needed, err := collectSchemas(doc, []string{reqName, respName})
	if err != nil {
		return err
	}

	names := make([]string, 0, len(needed))
	for n := range needed {
		names = append(names, n)
	}
	sort.Strings(names)

	var msgs []*descriptorpb.DescriptorProto
	for _, n := range names {
		dp, derr := buildMessage(n, needed[n], pkg)
		if derr != nil {
			return derr
		}
		msgs = append(msgs, dp)
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("wavefront/gen/" + sanitize(doc.Info.Version) + ".proto"),
		Package:     proto.String(pkg),
		Syntax:      proto.String("proto3"),
		MessageType: msgs,
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	if _, err := protodesc.NewFiles(fds); err != nil {
		return fmt.Errorf("generated descriptors are invalid: %w", err)
	}
	descBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(fds)
	if err != nil {
		return fmt.Errorf("marshal descriptors: %w", err)
	}

	versions := fmt.Sprintf(`version: 1
contracts:
  - contract_version: "%s"
    route: %s
    method: %s
    request_message: %s.%s
    response_message: %s.%s
`, doc.Info.Version, route, strings.ToUpper(method), pkg, reqName, pkg, respName)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir out: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "descriptors.binpb"), descBytes, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "openapi.json"), raw, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "versions.yaml"), []byte(versions), 0o644)
}

func singleOperation(doc openAPI) (route, method string, op operation, err error) {
	type found struct {
		route, method string
		op            operation
	}
	var ops []found
	for p, item := range doc.Paths {
		for m, rawOp := range item {
			if !httpMethods[strings.ToLower(m)] {
				continue
			}
			var o operation
			if e := json.Unmarshal(rawOp, &o); e != nil {
				return "", "", operation{}, fmt.Errorf("parse operation %s %s: %w", m, p, e)
			}
			ops = append(ops, found{p, m, o})
		}
	}
	if len(ops) != 1 {
		return "", "", operation{}, fmt.Errorf("v0.1 generator supports exactly one operation, found %d (multi-route is v0.2)", len(ops))
	}
	return ops[0].route, ops[0].method, ops[0].op, nil
}

func refSchemaName(op operation, request bool) (string, error) {
	var mt map[string]mediaType
	if request {
		if op.RequestBody == nil {
			return "", fmt.Errorf("v0.1 requires a requestBody schema (bodyless operations are v0.2)")
		}
		mt = op.RequestBody.Content
	} else {
		resp, ok := op.Responses["200"]
		if !ok {
			return "", fmt.Errorf("v0.1 requires a 200 response schema")
		}
		mt = resp.Content
	}
	m, ok := mt["application/json"]
	if !ok || m.Schema == nil {
		return "", fmt.Errorf("v0.1 requires an application/json schema")
	}
	if m.Schema.Ref == "" {
		return "", fmt.Errorf("v0.1 requires the request/response schema to be a $ref to #/components/schemas (inline schemas are unsupported)")
	}
	return refName(m.Schema.Ref), nil
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
		return fmt.Errorf("allOf is unsupported in v0.1")
	case len(sc.OneOf) > 0:
		return fmt.Errorf("oneOf is unsupported in v0.1")
	case len(sc.AnyOf) > 0:
		return fmt.Errorf("anyOf is unsupported in v0.1")
	case sc.AdditionalProperties != nil:
		return fmt.Errorf("additionalProperties is unsupported in v0.1")
	}
	return nil
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
			return nil, false, fmt.Errorf("array item type %q is unsupported in v0.1", sc.Items.Type)
		}
		return f, false, nil
	case "object":
		return nil, false, fmt.Errorf("inline object properties are unsupported in v0.1 (use a $ref)")
	case "":
		return nil, false, fmt.Errorf("property has no type and no $ref")
	default:
		return nil, false, fmt.Errorf("type %q is unsupported in v0.1", sc.Type)
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
