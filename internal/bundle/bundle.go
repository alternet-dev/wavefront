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
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"

	"github.com/alternet-dev/wavefront/internal/transform"
	"github.com/alternet-dev/wavefront/internal/wireerror"
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

// PackageCollisionError is raised at boot when two or more version layers each
// uniquely contribute proto files into the same proto package. The runtime
// resolves messages by their fully-qualified name against one merged
// FileDescriptorSet, so a package owned by multiple layers would have
// undefined resolution. The disjoint-package guarantee is normally upheld by
// the wavefront.gen.v<version> namespacing convention; this typed error is
// the defense-in-depth check that fails fast at boot if the convention is
// ever violated. Layers is sorted lexically for deterministic messages.
//
// When more than one package collides, Load reports only the
// lexically-first colliding package — the operator fixes one at a time and
// re-runs, surfacing the next collision on the next boot.
type PackageCollisionError struct {
	Package string
	Layers  []string
}

func (e *PackageCollisionError) Error() string {
	return fmt.Sprintf("bundle layers %v share proto package %q; per-version proto packages are required (e.g., wavefront.gen.v<version>)", e.Layers, e.Package)
}

// --- model ---

// Contract is one resolved binding. Fields are unexported with accessors
// so it satisfies an adapter Binding interface (RequestMessage/ResponseMessage)
// structurally, with no import cycle.
type Contract struct {
	contractVersion  string
	route            string
	method           string
	requestMessage   string
	responseMessage  string
	requestOps       []transform.Op
	responseOps      []transform.Op
	target           string
	transformTarget  string
	chain            []*Contract
	errorMessages    map[int]string
	errorResponseOps map[int][]transform.Op
	strict           bool
}

func (c *Contract) ContractVersion() string     { return c.contractVersion }
func (c *Contract) Route() string               { return c.route }
func (c *Contract) Method() string              { return c.method }
func (c *Contract) RequestMessage() string      { return c.requestMessage }
func (c *Contract) ResponseMessage() string     { return c.responseMessage }
func (c *Contract) RequestOps() []transform.Op  { return c.requestOps }
func (c *Contract) ResponseOps() []transform.Op { return c.responseOps }

// ErrorResponseOps returns the response transform ops scoped to a declared
// error status, or nil if none are configured. A nil slice is a byte-identical
// passthrough in transform.ApplyResponse, so callers can apply it unconditionally.
func (c *Contract) ErrorResponseOps(status int) []transform.Op { return c.errorResponseOps[status] }
func (c *Contract) Target() string                             { return c.target }
func (c *Contract) TransformTarget() string                    { return c.transformTarget }
func (c *Contract) Chain() []*Contract                         { return c.chain }

// ErrorMessage returns the proto message name the contract binds for an
// upstream status, if declared. A declared (route, status) is a first-class
// typed contract response; an undeclared one falls to the aid envelope (or a
// hard 502 under strict).
func (c *Contract) ErrorMessage(status int) (string, bool) {
	name, ok := c.errorMessages[status]
	return name, ok
}

// Strict reports whether undeclared upstream statuses collapse to a hard 502
// for this contract rather than the graceful aid envelope.
func (c *Contract) Strict() bool { return c.strict }

// Bundle holds the resolved view of a loaded bundle directory. Internally a
// flat list of contracts: a layer holds one or more contracts sharing a
// contract_version and distinguished by (route, method). The list
// preserves the load order (sorted-layer × layer's versions.yaml order)
// so iteration is deterministic, and the byVersion index gives O(1)
// per-version lookups that LookupRoute, Versions, and resolveChains all
// rely on.
type Bundle struct {
	contracts []*Contract
	byVersion map[string][]*Contract
	files     *protoregistry.Files
}

// Contract returns the first contract in the bundle whose contract_version
// is version. A single-route layer has one contract per version, so this
// is unambiguous. A multi-route layer has many contracts per version (one
// per route); Contract returns the first by load order — callers that
// need a specific (version, route, method) binding use LookupRoute first.
func (b *Bundle) Contract(version string) (*Contract, bool) {
	cs, ok := b.byVersion[version]
	if !ok || len(cs) == 0 {
		return nil, false
	}
	return cs[0], true
}

// LookupRoute returns the first contract in the bundle that binds
// (path, method), if any. It is a (value, ok) lookup — idiomatic in the
// shape of `os.LookupEnv` or `(*sync.Map).Load` — so callers can both gate
// on membership and use the matched contract without a second pass.
//
// The proxy uses this as the first-pass routing gate: a request whose
// (URL.Path, Method) is not in the bundle's route set is refused before any
// other request work (decode, negotiate, upstream call). A wrong-method
// request — same path, different method — is NOT routed: each contract names
// exactly one method, so wrong-method folds into the same "no contract
// binds this" miss that an entirely unknown path produces. The caller emits
// `unknown_route` 404 in both cases.
//
// Multiple contracts may legally bind the same (route, method) across
// different `contract_version` values. LookupRoute returns the first match
// in bundle order; selecting between same-(route, method) contracts is the
// version-negotiation layer's job — see Lookup for the keyed (path, method,
// version) form callers reach for once a version is in hand.
func (b *Bundle) LookupRoute(path, method string) (*Contract, bool) {
	for _, c := range b.contracts {
		if c.route == path && c.method == method {
			return c, true
		}
	}
	return nil, false
}

// Lookup returns the contract that matches (path, method, version) — the
// full resolution key used by the proxy once the inbound contract-version
// header has been read. It is the only correct dispatch for a multi-route
// bundle: Contract(version) would return whichever contract for that
// version happens to be first in load order, hiding the per-route binding.
//
// On miss, ok is false. Misses come in three flavours, all collapsed into
// the same (nil, false) result here; the caller (negotiate.Resolve) maps
// them onto the right wire-error:
//   - version is absent from the bundle,
//   - version is present but does not bind this (path, method),
//   - path/method is unknown to every version (this case is normally
//     handled before negotiate by the proxy's route gate using LookupRoute).
//
// For a single-route bundle every version has exactly one contract, so
// Lookup(c.Route(), c.Method(), c.ContractVersion()) trivially returns c —
// the degenerate case continues to work without special-casing.
func (b *Bundle) Lookup(path, method, version string) (*Contract, bool) {
	cs, ok := b.byVersion[version]
	if !ok {
		return nil, false
	}
	for _, c := range cs {
		if c.route == path && c.method == method {
			return c, true
		}
	}
	return nil, false
}

// Versions returns the contract versions present in the bundle, sorted
// lexically ascending. The last element is the "latest" layer — the
// highest contract version — which is the default target for downstream
// tooling that needs a stable "newest first" ordering.
func (b *Bundle) Versions() []string {
	out := make([]string, 0, len(b.byVersion))
	for cv := range b.byVersion {
		out = append(out, cv)
	}
	sort.Strings(out)
	return out
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
	ContractVersion string            `yaml:"contract_version"`
	Route           string            `yaml:"route"`
	Method          string            `yaml:"method"`
	RequestMessage  string            `yaml:"request_message"`
	ResponseMessage string            `yaml:"response_message"`
	ErrorMessages   map[string]string `yaml:"error_messages"`
	Strict          bool              `yaml:"strict"`
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
	Request        []yamlOp            `yaml:"request"`
	Response       []yamlOp            `yaml:"response"`
	ErrorResponses map[string][]yamlOp `yaml:"error_responses"`
	Target         string              `yaml:"target"`
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
		name string
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
		loaded = append(loaded, layer{name: name, fdps: fdps, yb: yb})
	}

	// Defense-in-depth: no two version layers may uniquely contribute files
	// into the same proto package. The runtime resolves message names
	// against a single merged FileDescriptorSet; a package owned by two
	// layers would have ambiguous resolution. The wavefront.gen.v<version>
	// namespacing convention normally guarantees disjointness; this
	// explicit check fails fast at boot if the convention is ever violated.
	perLayerNames := make([]string, len(loaded))
	allFDPs := make([][]*descriptorpb.FileDescriptorProto, len(loaded))
	for i, l := range loaded {
		perLayerNames[i] = l.name
		allFDPs[i] = l.fdps
	}
	if cerr := checkPackageCollisions(perLayerNames, allFDPs); cerr != nil {
		return nil, cerr
	}

	files, merr := mergeDescriptors(allFDPs)
	if merr != nil {
		return nil, merr
	}

	// contracts is the flat in-order list; byVersion is the per-version
	// index. A single-route layer has one entry per version; a multi-route
	// layer contributes many entries with the same contract_version,
	// distinguished by (route, method).
	var contracts []*Contract
	byVersion := make(map[string][]*Contract)
	// seen detects true duplicates: same version AND same (route, method).
	// A multi-route layer with the same (route, method) listed twice — or
	// two layers each claiming the same (version, route, method) — is
	// invalid.
	type key struct{ version, route, method string }
	seen := make(map[key]bool)
	for _, l := range loaded {
		if len(l.yb.Contracts) == 0 {
			return nil, &ValidationError{Reason: "layer has no contracts"}
		}
		for _, yc := range l.yb.Contracts {
			c, verr := validateContract(yc)
			if verr != nil {
				return nil, verr
			}
			k := key{c.contractVersion, c.route, c.method}
			if seen[k] {
				return nil, &ValidationError{Contract: c.contractVersion, Field: "contract_version", Reason: "duplicate (version, route, method) across layers"}
			}
			seen[k] = true
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
			for _, name := range c.errorMessages {
				d, ferr := files.FindDescriptorByName(protoreflect.FullName(name))
				if ferr != nil {
					return nil, &MessageNotFoundError{Contract: c.contractVersion, Message: name}
				}
				if _, ok := d.(protoreflect.MessageDescriptor); !ok {
					return nil, &MessageNotFoundError{Contract: c.contractVersion, Message: name}
				}
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
					if len(ov.Transform.ErrorResponses) > 0 {
						c.errorResponseOps = make(map[int][]transform.Op, len(ov.Transform.ErrorResponses))
						for code, raw := range ov.Transform.ErrorResponses {
							status, perr := strconv.Atoi(strings.TrimSpace(code))
							if perr != nil {
								return nil, &ValidationError{Contract: c.contractVersion, Field: "transform.error_responses", Reason: "status key " + code + " is not a number"}
							}
							name, declared := c.errorMessages[status]
							if !declared {
								return nil, &ValidationError{Contract: c.contractVersion, Field: "transform.error_responses", Reason: "status " + code + " has no error_messages binding to transform"}
							}
							ed, ferr := files.FindDescriptorByName(protoreflect.FullName(name))
							if ferr != nil {
								return nil, &MessageNotFoundError{Contract: c.contractVersion, Message: name}
							}
							emd, ok := ed.(protoreflect.MessageDescriptor)
							if !ok {
								return nil, &MessageNotFoundError{Contract: c.contractVersion, Message: name}
							}
							// The bound error message is the response cross-check target:
							// rename.to / default.field validate against the error type,
							// exactly as 2xx response ops validate against response_message.
							ops, oerr := toOps(c.contractVersion, "response", raw, reqMsg, emd)
							if oerr != nil {
								return nil, oerr
							}
							c.errorResponseOps[status] = ops
						}
					}
				} else {
					// Route override — named target, no transform ops.
					c.target = strings.TrimSpace(ov.Route.Target)
				}
			}
			contracts = append(contracts, c)
			byVersion[c.contractVersion] = append(byVersion[c.contractVersion], c)
		}
	}

	// Validate that every resolution override names a known contract version.
	for cv := range resolutionMap {
		if _, known := byVersion[cv]; !known {
			return nil, &ValidationError{Contract: cv, Field: "contract_version", Reason: "resolution override names an unknown contract version"}
		}
	}

	if cerr := resolveChains(contracts, byVersion); cerr != nil {
		return nil, cerr
	}

	return &Bundle{contracts: contracts, byVersion: byVersion, files: files}, nil
}

// resolveChains walks each contract's transform.target links into an ordered
// chain ([the contract, its target, ...] ending at a terminal — a contract
// with no transform.target). A target naming an unknown version, or a cycle,
// is a hard error. The resolved chain is stored on each Contract.
//
// Cross-version chain lookup: when a multi-route layer has many
// contracts per version, transform.target = v2 from contract C on
// (version=v1, route=R, method=M) resolves to the contract on
// (version=v2, route=R, method=M) — same (route, method) at the new
// version. For single-route layers each version has exactly one contract,
// so any (route, method) match is trivially the right one. A v2 layer
// that does not bind C's (route, method) is a hard error.
func resolveChains(contracts []*Contract, byVersion map[string][]*Contract) error {
	for _, c := range contracts {
		var chain []*Contract
		seen := map[string]bool{}
		cur := c
		for {
			if seen[cur.contractVersion] {
				return &ValidationError{Contract: cur.contractVersion, Field: "transform.target", Reason: "transform chain cycles"}
			}
			seen[cur.contractVersion] = true
			chain = append(chain, cur)
			if cur.transformTarget == "" {
				break
			}
			candidates, ok := byVersion[cur.transformTarget]
			if !ok {
				return &ValidationError{Contract: cur.contractVersion, Field: "transform.target", Reason: "transform target names an unknown version"}
			}
			// Find the candidate that binds the same (route, method) as
			// the source contract. Single-route layers always have
			// exactly one candidate and it matches by construction.
			var next *Contract
			for _, cand := range candidates {
				if cand.route == c.route && cand.method == c.method {
					next = cand
					break
				}
			}
			if next == nil {
				return &ValidationError{Contract: cur.contractVersion, Field: "transform.target", Reason: "transform target version does not bind (" + c.route + ", " + c.method + ")"}
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

// checkPackageCollisions enforces the per-version proto-package convention
// at boot. It rejects bundles in which two or more version layers each
// uniquely contribute proto files into the same package.
//
// A shared dependency (a file present bit-identically in every layer that
// includes it) is not a collision: that's the same dedup path mergeDescriptors
// takes, and the resulting merged registry has one entry for that file.
// Only files that are unique to a single layer (file name absent from other
// layers' FDS, OR present with bit-identical bytes only in this same layer)
// contribute package ownership.
//
// On multi-package collision, only the lexically-first colliding package is
// reported — operators fix one at a time and re-boot, surfacing the next.
func checkPackageCollisions(layerNames []string, perLayer [][]*descriptorpb.FileDescriptorProto) error {
	if len(layerNames) != len(perLayer) {
		// Programmer error; the call site builds these in lockstep.
		return fmt.Errorf("checkPackageCollisions: %d layer names but %d FDS slices", len(layerNames), len(perLayer))
	}
	// fileLayers records the set of layers that contribute each file
	// (file name → set of layer names that include a file with that
	// name in their FDS). Bit-identical contributions across layers
	// indicate a shared dependency, not per-layer ownership.
	type contribution struct {
		layers map[string]struct{}
		bytes  map[string]struct{} // deterministic-marshalled wire bytes
	}
	fileContribs := map[string]*contribution{}
	for i, fdps := range perLayer {
		layerName := layerNames[i]
		for _, f := range fdps {
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(f)
			if err != nil {
				return &ParseError{File: fileDescriptors, Err: err}
			}
			c := fileContribs[f.GetName()]
			if c == nil {
				c = &contribution{
					layers: map[string]struct{}{},
					bytes:  map[string]struct{}{},
				}
				fileContribs[f.GetName()] = c
			}
			c.layers[layerName] = struct{}{}
			c.bytes[string(b)] = struct{}{}
		}
	}

	// packageOwners maps each proto package to the set of layers that
	// uniquely own a file in that package. A file with bit-identical
	// contributions across layers (a shared dep) contributes no
	// ownership; a file that differs across layers will be caught by
	// mergeDescriptors as a hard inconsistency. The package-collision
	// check only fires when two layers each contribute a DIFFERENT file
	// into the same package.
	packageOwners := map[string]map[string]struct{}{}
	for fileName, c := range fileContribs {
		if len(c.layers) > 1 && len(c.bytes) == 1 {
			// Shared dep: same file, same bytes, in every contributing
			// layer. Don't attribute package ownership.
			continue
		}
		if len(c.bytes) > 1 {
			// Same file name, different bytes across layers.
			// mergeDescriptors will reject this; let it produce its
			// own targeted error and don't conflate it with a package
			// collision.
			continue
		}
		// Exactly one layer contributes this file. Look up the
		// package from that layer's FDS.
		var pkg string
		for i, fdps := range perLayer {
			if _, ok := c.layers[layerNames[i]]; !ok {
				continue
			}
			for _, f := range fdps {
				if f.GetName() == fileName {
					pkg = f.GetPackage()
					break
				}
			}
			break
		}
		if pkg == "" {
			// Files without a package (the proto default package) are
			// not produced by the wavefront codegen path and would not
			// be referenced by any contract binding. Skip them rather
			// than flagging the empty-package case as a collision.
			continue
		}
		owners := packageOwners[pkg]
		if owners == nil {
			owners = map[string]struct{}{}
			packageOwners[pkg] = owners
		}
		for layer := range c.layers {
			owners[layer] = struct{}{}
		}
	}

	// Pick the lexically-first colliding package for a deterministic error.
	var colliding []string
	for pkg, owners := range packageOwners {
		if len(owners) > 1 {
			colliding = append(colliding, pkg)
		}
	}
	if len(colliding) == 0 {
		return nil
	}
	sort.Strings(colliding)
	pkg := colliding[0]
	layers := make([]string, 0, len(packageOwners[pkg]))
	for layer := range packageOwners[pkg] {
		layers = append(layers, layer)
	}
	sort.Strings(layers)
	return &PackageCollisionError{Package: pkg, Layers: layers}
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
	c.strict = yc.Strict
	if len(yc.ErrorMessages) > 0 {
		c.errorMessages = make(map[int]string, len(yc.ErrorMessages))
		for code, msg := range yc.ErrorMessages {
			status, perr := strconv.Atoi(strings.TrimSpace(code))
			if perr != nil {
				return nil, &ValidationError{Contract: cv, Field: "error_messages", Reason: "status key " + code + " is not a number"}
			}
			if status >= 200 && status <= 299 {
				return nil, &ValidationError{Contract: cv, Field: "error_messages", Reason: "status " + code + " is 2xx; success bodies bind via response_message"}
			}
			// 206-208/226 are caught by the 2xx guard above; only 3xx reaches here.
			if wireerror.IsCapabilityCeiling(status) {
				return nil, &ValidationError{Contract: cv, Field: "error_messages", Reason: "status " + code + " is a capability ceiling and is always 502"}
			}
			name := strings.TrimSpace(msg)
			if name == "" {
				return nil, &ValidationError{Contract: cv, Field: "error_messages", Reason: "status " + code + " has an empty message name"}
			}
			c.errorMessages[status] = name
		}
	}
	return c, nil
}
