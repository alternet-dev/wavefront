// Package bundle loads and validates the read-only descriptor bundle
// (descriptors.binpb + openapi.json + versions.yaml) at boot, fail-fast. The
// route → message binding is read verbatim (zero inference); every binding is
// resolved against the FileDescriptorSet here so the runtime never has to.
// Transform stanzas live in the operator-owned resolution.yaml at the bundle
// root; the per-version layer manifests are binding-only. Unknown keys are
// refused by strict decode.
package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	fileResolution  = "resolution.yaml"
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
	target          string
	transformTarget string
	chain           []*Contract
}

func (c *Contract) ContractVersion() string     { return c.contractVersion }
func (c *Contract) Route() string               { return c.route }
func (c *Contract) Method() string              { return c.method }
func (c *Contract) RequestMessage() string      { return c.requestMessage }
func (c *Contract) ResponseMessage() string     { return c.responseMessage }
func (c *Contract) RequestOps() []transform.Op  { return c.requestOps }
func (c *Contract) ResponseOps() []transform.Op { return c.responseOps }
func (c *Contract) Target() string              { return c.target }
func (c *Contract) TransformTarget() string     { return c.transformTarget }
func (c *Contract) Chain() []*Contract          { return c.chain }

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
	ContractVersion string `yaml:"contract_version"`
	Route           string `yaml:"route"`
	Method          string `yaml:"method"`
	RequestMessage  string `yaml:"request_message"`
	ResponseMessage string `yaml:"response_message"`
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

type yamlResolutionFile struct {
	Version   int              `yaml:"version"`
	Overrides []yamlResolution `yaml:"overrides"`
}

type yamlResolution struct {
	ContractVersion string         `yaml:"contract_version"`
	Transform       *yamlTransform `yaml:"transform"`
	Route           *yamlRoute     `yaml:"route"`
}

type yamlTransform struct {
	Request  []yamlOp `yaml:"request"`
	Response []yamlOp `yaml:"response"`
	Target   string   `yaml:"target"`
}

type yamlRoute struct {
	Target string `yaml:"target"`
}

var allowedMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodHead: true,
	http.MethodOptions: true,
}

// loadResolution reads the optional operator-owned resolution.yaml at the
// bundle root. An absent file yields an empty map — no version has an
// override. Strict decode; the schema version must be 1. Each override must
// set exactly one of transform or route.
func loadResolution(dir string) (map[string]yamlResolution, error) {
	raw, err := os.ReadFile(filepath.Join(dir, fileResolution))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]yamlResolution{}, nil
	}
	if err != nil {
		return nil, &ReadError{File: fileResolution, Err: err}
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var yr yamlResolutionFile
	if err := dec.Decode(&yr); err != nil {
		return nil, &ParseError{File: fileResolution, Err: err}
	}
	if yr.Version != 1 {
		return nil, &UnsupportedVersionError{Version: yr.Version}
	}
	out := make(map[string]yamlResolution, len(yr.Overrides))
	for _, ov := range yr.Overrides {
		cv := strings.TrimSpace(ov.ContractVersion)
		if cv == "" {
			return nil, &ValidationError{Field: "contract_version", Reason: "resolution override must name a contract version"}
		}
		if _, dup := out[cv]; dup {
			return nil, &ValidationError{Contract: cv, Field: "contract_version", Reason: "duplicate resolution override"}
		}
		hasTransform := ov.Transform != nil
		hasRoute := ov.Route != nil
		if hasTransform == hasRoute { // both set or neither set
			return nil, &ValidationError{Contract: cv, Field: "override", Reason: "must set exactly one of transform or route"}
		}
		if hasRoute && strings.TrimSpace(ov.Route.Target) == "" {
			return nil, &ValidationError{Contract: cv, Field: "route.target", Reason: "route.target must name a target"}
		}
		out[cv] = ov
	}
	return out, nil
}

// Load reads, parses, validates, and resolves the bundle in dir. The bundle
// is a directory of layer subdirectories, each holding descriptors.binpb,
// openapi.json, and versions.yaml. Any failure is fatal (a typed error); the
// caller refuses to start.
func Load(dir string) (*Bundle, error) {
	layerNames, err := layerDirs(dir)
	if err != nil {
		return nil, err
	}

	resolutionMap, rerr := loadResolution(dir)
	if rerr != nil {
		return nil, rerr
	}

	type layer struct {
		fdps []*descriptorpb.FileDescriptorProto
		yb   *yamlBundle
	}
	loaded := make([]layer, 0, len(layerNames))

	for _, name := range layerNames {
		lp := filepath.Join(dir, name)
		fdps, derr := loadLayerDescriptors(filepath.Join(lp, fileDescriptors))
		if derr != nil {
			return nil, derr
		}
		if verr := validateOpenAPI(filepath.Join(lp, fileOpenAPI)); verr != nil {
			return nil, verr
		}
		yb, lerr := loadVersions(filepath.Join(lp, fileVersions))
		if lerr != nil {
			return nil, lerr
		}
		if yb.Version != 1 {
			return nil, &UnsupportedVersionError{Version: yb.Version}
		}
		loaded = append(loaded, layer{fdps: fdps, yb: yb})
	}

	allFDPs := make([][]*descriptorpb.FileDescriptorProto, len(loaded))
	for i, l := range loaded {
		allFDPs[i] = l.fdps
	}
	files, merr := mergeDescriptors(allFDPs)
	if merr != nil {
		return nil, merr
	}

	contracts := make(map[string]*Contract)
	for _, l := range loaded {
		if len(l.yb.Contracts) == 0 {
			return nil, &ValidationError{Reason: "layer has no contracts"}
		}
		for _, yc := range l.yb.Contracts {
			c, verr := validateContract(yc)
			if verr != nil {
				return nil, verr
			}
			if _, dup := contracts[c.contractVersion]; dup {
				return nil, &ValidationError{Contract: c.contractVersion, Field: "contract_version", Reason: "duplicate across layers"}
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
			if ov, hasOverride := resolutionMap[c.contractVersion]; hasOverride {
				if ov.Transform != nil {
					reqOps, oerr := toOps(c.contractVersion, "request", ov.Transform.Request, reqMsg, respMsg)
					if oerr != nil {
						return nil, oerr
					}
					respOps, oerr := toOps(c.contractVersion, "response", ov.Transform.Response, reqMsg, respMsg)
					if oerr != nil {
						return nil, oerr
					}
					c.requestOps = reqOps
					c.responseOps = respOps
					c.transformTarget = strings.TrimSpace(ov.Transform.Target)
				} else {
					// Route override — named target, no transform ops.
					c.target = strings.TrimSpace(ov.Route.Target)
				}
			}
			contracts[c.contractVersion] = c
		}
	}

	// Validate that every resolution override names a known contract version.
	for cv := range resolutionMap {
		if _, known := contracts[cv]; !known {
			return nil, &ValidationError{Contract: cv, Field: "contract_version", Reason: "resolution override names an unknown contract version"}
		}
	}

	if cerr := resolveChains(contracts); cerr != nil {
		return nil, cerr
	}

	return &Bundle{contracts: contracts, files: files}, nil
}

// resolveChains walks each contract's transform.target links into an ordered
// chain ([the contract, its target, ...] ending at a terminal — a contract
// with no transform.target). A target naming an unknown version, or a cycle,
// is a hard error. The resolved chain is stored on each Contract.
func resolveChains(contracts map[string]*Contract) error {
	for _, c := range contracts {
		var chain []*Contract
		seen := map[string]bool{}
		cur := c
		for {
			if seen[cur.contractVersion] {
				return &ValidationError{Contract: c.contractVersion, Field: "transform.target", Reason: "transform chain cycles"}
			}
			seen[cur.contractVersion] = true
			chain = append(chain, cur)
			if cur.transformTarget == "" {
				break
			}
			next, ok := contracts[cur.transformTarget]
			if !ok {
				return &ValidationError{Contract: cur.contractVersion, Field: "transform.target", Reason: "transform target names an unknown version"}
			}
			cur = next
		}
		c.chain = chain
	}
	return nil
}

// layerDirs returns the sorted names of the immediate subdirectories of dir,
// each a bundle layer. An unreadable directory, or one with no subdirectory,
// is a hard error.
func layerDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, &ReadError{File: dir, Err: err}
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, &ValidationError{Reason: "bundle directory contains no layers"}
	}
	return names, nil
}

// loadLayerDescriptors reads one layer's descriptors.binpb and returns its
// FileDescriptorProtos. They are merged across layers (and only then built
// into a registry) by mergeDescriptors.
func loadLayerDescriptors(path string) ([]*descriptorpb.FileDescriptorProto, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &ReadError{File: fileDescriptors, Err: err}
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &fds); err != nil {
		return nil, &ParseError{File: fileDescriptors, Err: err}
	}
	return fds.File, nil
}

// mergeDescriptors combines every layer's FileDescriptorProtos into one
// registry. A file name that appears in more than one layer is registered
// once; if two layers define the same file name with differing bytes that
// is a hard error (the bundle is internally inconsistent).
func mergeDescriptors(perLayer [][]*descriptorpb.FileDescriptorProto) (*protoregistry.Files, error) {
	seen := map[string][]byte{}
	merged := &descriptorpb.FileDescriptorSet{}
	for _, fdps := range perLayer {
		for _, f := range fdps {
			name := f.GetName()
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(f)
			if err != nil {
				return nil, &ParseError{File: fileDescriptors, Err: err}
			}
			if prev, dup := seen[name]; dup {
				if !bytes.Equal(prev, b) {
					return nil, &ParseError{File: fileDescriptors, Err: fmt.Errorf("conflicting definitions of %q across layers", name)}
				}
				continue
			}
			seen[name] = b
			merged.File = append(merged.File, f)
		}
	}
	files, err := protodesc.NewFiles(merged)
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
