// Package bundle loads and validates the read-only descriptor bundle
// (descriptors.binpb + openapi.json + versions.yaml) at boot, fail-fast. The
// route → message binding is read verbatim (zero inference); every binding is
// resolved against the FileDescriptorSet here so the runtime never has to.
// Bundles may carry optional request/response transform stanzas (additive
// fields parsed into per-contract `[]transform.Op`). Unknown keys are
// refused by strict decode.
package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"

	"github.com/alternet-dev/wavefront/internal/transform"
)

const (
	fileDescriptors = "descriptors.binpb"
	fileOpenAPI     = "openapi.json"
	fileVersions    = "versions.yaml"
)

// --- typed errors (one per failure step; mirrors the sibling taxonomy) ---

type ReadError struct {
	File string
	Err  error
}

func (e *ReadError) Error() string { return fmt.Sprintf("read %s: %v", e.File, e.Err) }
func (e *ReadError) Unwrap() error { return e.Err }

type ParseError struct {
	File string
	Err  error
}

func (e *ParseError) Error() string { return fmt.Sprintf("parse %s: %v", e.File, e.Err) }
func (e *ParseError) Unwrap() error { return e.Err }

type UnsupportedVersionError struct{ Version int }

func (e *UnsupportedVersionError) Error() string {
	return fmt.Sprintf("unsupported bundle schema version %d (only 1 is supported)", e.Version)
}

type ValidationError struct {
	Contract string
	Field    string
	Reason   string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid versions.yaml (contract=%q field=%q): %s", e.Contract, e.Field, e.Reason)
}

type MessageNotFoundError struct {
	Contract string
	Message  string
}

func (e *MessageNotFoundError) Error() string {
	return fmt.Sprintf("contract %q binding message %q not found in descriptors", e.Contract, e.Message)
}

// --- model ---

// Contract is one resolved binding. Fields are unexported with accessors
// so it satisfies an adapter Binding interface (RequestMessage/ResponseMessage)
// structurally, with no import cycle.
type Contract struct {
	contractVersion string
	route           string
	method          string
	requestMessage  string
	responseMessage string
	requestOps      []transform.Op
	responseOps     []transform.Op
}

func (c *Contract) ContractVersion() string     { return c.contractVersion }
func (c *Contract) Route() string               { return c.route }
func (c *Contract) Method() string              { return c.method }
func (c *Contract) RequestMessage() string      { return c.requestMessage }
func (c *Contract) ResponseMessage() string     { return c.responseMessage }
func (c *Contract) RequestOps() []transform.Op  { return c.requestOps }
func (c *Contract) ResponseOps() []transform.Op { return c.responseOps }

type Bundle struct {
	contracts map[string]*Contract
	files     *protoregistry.Files
}

func (b *Bundle) Contract(version string) (*Contract, bool) {
	c, ok := b.contracts[version]
	return c, ok
}

// Message resolves a fully-qualified proto message name against the bundle's
// FileDescriptorSet.
func (b *Bundle) Message(fullName string) (protoreflect.MessageDescriptor, error) {
	d, err := b.files.FindDescriptorByName(protoreflect.FullName(fullName))
	if err != nil {
		return nil, &MessageNotFoundError{Message: fullName}
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, &MessageNotFoundError{Message: fullName}
	}
	return md, nil
}

// --- on-disk shape (strict-decoded) ---

type yamlContract struct {
	ContractVersion string   `yaml:"contract_version"`
	Route           string   `yaml:"route"`
	Method          string   `yaml:"method"`
	RequestMessage  string   `yaml:"request_message"`
	ResponseMessage string   `yaml:"response_message"`
	Request         []yamlOp `yaml:"request"`
	Response        []yamlOp `yaml:"response"`
}

type yamlRename struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}
type yamlDefault struct {
	Field string `yaml:"field"`
	Value any    `yaml:"value"`
}
type yamlField struct {
	Field string `yaml:"field"`
}
type yamlCoerce struct {
	Field string `yaml:"field"`
	To    string `yaml:"to"`
}
type yamlOp struct {
	Rename      *yamlRename  `yaml:"rename"`
	Default     *yamlDefault `yaml:"default"`
	Optionalize *yamlField   `yaml:"optionalize"`
	Coerce      *yamlCoerce  `yaml:"coerce"`
}

type yamlBundle struct {
	Version   int            `yaml:"version"`
	Contracts []yamlContract `yaml:"contracts"`
}

var allowedMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodHead: true,
	http.MethodOptions: true,
}

// Load reads, parses, validates, and resolves the bundle in dir. Any failure
// is fatal (returned as a typed error); the caller refuses to start.
func Load(dir string) (*Bundle, error) {
	files, err := loadDescriptors(filepath.Join(dir, fileDescriptors))
	if err != nil {
		return nil, err
	}
	if err := validateOpenAPI(filepath.Join(dir, fileOpenAPI)); err != nil {
		return nil, err
	}
	yb, err := loadVersions(filepath.Join(dir, fileVersions))
	if err != nil {
		return nil, err
	}

	if yb.Version != 1 {
		return nil, &UnsupportedVersionError{Version: yb.Version}
	}
	if len(yb.Contracts) == 0 {
		return nil, &ValidationError{Reason: "no contracts"}
	}

	contracts := make(map[string]*Contract, len(yb.Contracts))
	for _, yc := range yb.Contracts {
		c, verr := validateContract(yc)
		if verr != nil {
			return nil, verr
		}
		if _, dup := contracts[c.contractVersion]; dup {
			return nil, &ValidationError{Contract: c.contractVersion, Field: "contract_version", Reason: "duplicate"}
		}
		var reqMsg, respMsg protoreflect.MessageDescriptor
		for _, pair := range []struct {
			name string
			out  *protoreflect.MessageDescriptor
		}{{c.requestMessage, &reqMsg}, {c.responseMessage, &respMsg}} {
			d, ferr := files.FindDescriptorByName(protoreflect.FullName(pair.name))
			if ferr != nil {
				return nil, &MessageNotFoundError{Contract: c.contractVersion, Message: pair.name}
			}
			md, ok := d.(protoreflect.MessageDescriptor)
			if !ok {
				return nil, &MessageNotFoundError{Contract: c.contractVersion, Message: pair.name}
			}
			*pair.out = md
		}
		reqOps, oerr := toOps(c.contractVersion, "request", yc.Request, reqMsg, respMsg)
		if oerr != nil {
			return nil, oerr
		}
		respOps, oerr := toOps(c.contractVersion, "response", yc.Response, reqMsg, respMsg)
		if oerr != nil {
			return nil, oerr
		}
		c.requestOps = reqOps
		c.responseOps = respOps
		contracts[c.contractVersion] = c
	}

	return &Bundle{contracts: contracts, files: files}, nil
}

func loadDescriptors(path string) (*protoregistry.Files, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &ReadError{File: fileDescriptors, Err: err}
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		return nil, &ParseError{File: fileDescriptors, Err: err}
	}
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		return nil, &ParseError{File: fileDescriptors, Err: err}
	}
	return files, nil
}

func validateOpenAPI(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return &ReadError{File: fileOpenAPI, Err: err}
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return &ParseError{File: fileOpenAPI, Err: err}
	}
	return nil
}

func loadVersions(path string) (*yamlBundle, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &ReadError{File: fileVersions, Err: err}
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // refuse any unknown key
	var yb yamlBundle
	if err := dec.Decode(&yb); err != nil {
		return nil, &ParseError{File: fileVersions, Err: err}
	}
	return &yb, nil
}

// sameParentDifferentLeaf reports whether p and q are the same length, agree
// segment-for-segment on every non-final segment (same Name AND same Array
// flag — the structural parent path must be identical), and differ at the
// final leaf's Name. It is the load-time invariant for rename: apply()
// writes the new leaf into the from-leaf's parent map, so any cross-parent
// rename would silently land at the wrong location.
func sameParentDifferentLeaf(p, q transform.Path) bool {
	if len(p) != len(q) || len(p) == 0 {
		return false
	}
	for i := 0; i < len(p)-1; i++ {
		if p[i] != q[i] {
			return false
		}
	}
	last := len(p) - 1
	return p[last].Name != q[last].Name
}

// toOps parses raw verb stanzas into a slice of transform.Op, statically
// validating grammar, leaf-only-rename, and (for external-targeting paths)
// the proto descriptor cross-check.
//
// reqMsg / respMsg are the request/response message descriptors for this
// contract (resolved by Load against the FileDescriptorSet). They are used
// for the descriptor cross-check on external-targeting paths per the
// spec's validation table; internal-targeting paths get only the grammar
// + structural checks (the live upstream is the gate).
func toOps(cv, dir string, raw []yamlOp, reqMsg, respMsg protoreflect.MessageDescriptor) ([]transform.Op, error) {
	if dir != "request" && dir != "response" {
		return nil, fmt.Errorf("toOps: unknown direction %q (must be request or response)", dir)
	}
	// external returns the descriptor a given stanza-role's path targets,
	// or nil if it targets the internal (upstream) shape.
	external := func(role string) protoreflect.MessageDescriptor {
		switch dir {
		case "request":
			switch role {
			case "rename.from", "coerce.field", "optionalize.field":
				return reqMsg
			}
		case "response":
			switch role {
			case "rename.to", "default.field":
				return respMsg
			}
		}
		return nil
	}

	ops := make([]transform.Op, 0, len(raw))
	for _, o := range raw {
		set := 0
		var op transform.Op
		if o.Rename != nil {
			set++
			if o.Rename.From == "" || o.Rename.To == "" || o.Rename.From == o.Rename.To {
				return nil, &ValidationError{Contract: cv, Field: dir + ".rename", Reason: "from/to must be non-empty and distinct"}
			}
			fromPath, ferr := transform.ParsePath(o.Rename.From)
			if ferr != nil {
				return nil, &ValidationError{Contract: cv, Field: dir + ".rename.from", Reason: ferr.Error()}
			}
			toPath, perr := transform.ParsePath(o.Rename.To)
			if perr != nil {
				return nil, &ValidationError{Contract: cv, Field: dir + ".rename.to", Reason: perr.Error()}
			}
			// Rename is leaf-only: from and to must share every non-final segment
			// (same parent) AND differ at the final leaf. The runtime apply()
			// ASSUMES this invariant — it writes the new leaf into the from-leaf's
			// parent map. Cross-parent rename would silently corrupt data.
			if !sameParentDifferentLeaf(fromPath, toPath) {
				return nil, &ValidationError{Contract: cv, Field: dir + ".rename", Reason: "from/to must share every non-final segment AND differ at the leaf (leaf-only rename)"}
			}
			if md := external("rename.from"); md != nil {
				if err := validatePathAgainstMessage(fromPath, md, dir+".rename.from"); err != nil {
					return nil, &ValidationError{Contract: cv, Field: dir + ".rename.from", Reason: err.Error()}
				}
			}
			if md := external("rename.to"); md != nil {
				if err := validatePathAgainstMessage(toPath, md, dir+".rename.to"); err != nil {
					return nil, &ValidationError{Contract: cv, Field: dir + ".rename.to", Reason: err.Error()}
				}
			}
			op = transform.Op{Kind: transform.KindRename, From: fromPath, To: toPath}
		}
		if o.Default != nil {
			set++
			if o.Default.Field == "" {
				return nil, &ValidationError{Contract: cv, Field: dir + ".default", Reason: "field must be non-empty"}
			}
			fieldPath, ferr := transform.ParsePath(o.Default.Field)
			if ferr != nil {
				return nil, &ValidationError{Contract: cv, Field: dir + ".default.field", Reason: ferr.Error()}
			}
			dv, derr := transform.NewDefaultValue(o.Default.Value)
			if derr != nil {
				return nil, &ValidationError{Contract: cv, Field: dir + ".default.value", Reason: derr.Error()}
			}
			if md := external("default.field"); md != nil {
				if err := validatePathAgainstMessage(fieldPath, md, dir+".default.field"); err != nil {
					return nil, &ValidationError{Contract: cv, Field: dir + ".default.field", Reason: err.Error()}
				}
			}
			op = transform.Op{Kind: transform.KindDefault, Field: fieldPath, Value: dv}
		}
		if o.Optionalize != nil {
			set++
			if o.Optionalize.Field == "" {
				return nil, &ValidationError{Contract: cv, Field: dir + ".optionalize", Reason: "field must be non-empty"}
			}
			fieldPath, ferr := transform.ParsePath(o.Optionalize.Field)
			if ferr != nil {
				return nil, &ValidationError{Contract: cv, Field: dir + ".optionalize.field", Reason: ferr.Error()}
			}
			if md := external("optionalize.field"); md != nil {
				if err := validatePathAgainstMessage(fieldPath, md, dir+".optionalize.field"); err != nil {
					return nil, &ValidationError{Contract: cv, Field: dir + ".optionalize.field", Reason: err.Error()}
				}
			}
			op = transform.Op{Kind: transform.KindOptionalize, Field: fieldPath}
		}
		if o.Coerce != nil {
			set++
			if o.Coerce.Field == "" {
				return nil, &ValidationError{Contract: cv, Field: dir + ".coerce", Reason: "field must be non-empty"}
			}
			if o.Coerce.To != "string" && o.Coerce.To != "number" && o.Coerce.To != "bool" {
				return nil, &ValidationError{Contract: cv, Field: dir + ".coerce.to", Reason: "must be string|number|bool"}
			}
			fieldPath, ferr := transform.ParsePath(o.Coerce.Field)
			if ferr != nil {
				return nil, &ValidationError{Contract: cv, Field: dir + ".coerce.field", Reason: ferr.Error()}
			}
			if md := external("coerce.field"); md != nil {
				if err := validatePathAgainstMessage(fieldPath, md, dir+".coerce.field"); err != nil {
					return nil, &ValidationError{Contract: cv, Field: dir + ".coerce.field", Reason: err.Error()}
				}
			}
			op = transform.Op{Kind: transform.KindCoerce, Field: fieldPath, CoerceTo: o.Coerce.To}
		}
		if set != 1 {
			return nil, &ValidationError{Contract: cv, Field: dir, Reason: "each transform op must set exactly one verb"}
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func validateContract(yc yamlContract) (*Contract, error) {
	cv := strings.TrimSpace(yc.ContractVersion)
	c := &Contract{
		contractVersion: cv,
		route:           strings.TrimSpace(yc.Route),
		method:          strings.ToUpper(strings.TrimSpace(yc.Method)),
		requestMessage:  strings.TrimSpace(yc.RequestMessage),
		responseMessage: strings.TrimSpace(yc.ResponseMessage),
	}
	if c.contractVersion == "" {
		return nil, &ValidationError{Field: "contract_version", Reason: "must not be empty"}
	}
	if c.route == "" || !strings.HasPrefix(c.route, "/") {
		return nil, &ValidationError{Contract: cv, Field: "route", Reason: "must be a path beginning with /"}
	}
	if c.method == "" {
		return nil, &ValidationError{Contract: cv, Field: "method", Reason: "must not be empty"}
	}
	if !allowedMethods[c.method] {
		return nil, &ValidationError{Contract: cv, Field: "method", Reason: "not a valid HTTP method"}
	}
	if c.requestMessage == "" {
		return nil, &ValidationError{Contract: cv, Field: "request_message", Reason: "must not be empty"}
	}
	if c.responseMessage == "" {
		return nil, &ValidationError{Contract: cv, Field: "response_message", Reason: "must not be empty"}
	}
	return c, nil
}
