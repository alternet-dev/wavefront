package bundlegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

// routesFileName is the on-disk name of the typed route map emitted at the
// root of --out. It sits next to the protoc-gen-es message files so a
// consumer can `import { routes } from "./routes"`.
const routesFileName = "routes.ts"

// emitRoutesFile writes routes.ts under outDir. The file declares a typed
// `Route` interface and a `routes` const map keyed by operationId (one
// entry per contract in the layer's versions.yaml). Schema constants are
// imported from the protoc-gen-es output files that emitMessageClasses
// produced — paths are computed by walking the FileDescriptorSet so this
// step never hard-codes the protoc-gen-es naming convention more than
// once (in messageFileFromProto).
//
// Determinism: routes are sorted by (path, method); imports are sorted by
// specifier; per-file imported symbols are sorted alphabetically. A second
// run against the same bundle layer must produce a byte-identical
// routes.ts (asserted by TestGenTSClientRoutesFileDeterministic).
//
// The emitted file's import paths assume routes.ts lives at the root of
// outDir and the protoc-gen-es .ts files live under their proto package
// path (e.g., wavefront/gen/<sanitized>_pb.ts). It also assumes the
// `import_extension=none` parameter — no `.js` suffix on the specifier —
// matching the contract documented in protocGenESParams.
func emitRoutesFile(layerDir, outDir, version string, fds *descriptorpb.FileDescriptorSet) (string, error) {
	contracts, err := loadLayerContracts(layerDir)
	if err != nil {
		return "", err
	}
	if len(contracts) == 0 {
		// bundle.Load already enforces "layer has no contracts" as a hard
		// error so this branch is defence-in-depth, but the message
		// should still be unambiguous if it ever fires.
		return "", fmt.Errorf("gen-ts-client: layer %q has no contracts in versions.yaml", version)
	}

	opIDs, err := loadOperationIDs(layerDir)
	if err != nil {
		return "", err
	}

	// Build the per-contract route record. Each record carries enough state
	// to emit one map entry and to derive the import requirements.
	type routeRec struct {
		opID            string
		method          string
		path            string
		contractVersion string
		reqType         string // bare TypeScript type name (e.g., "Req")
		respType        string
		reqSchema       string // schema constant name (e.g., "ReqSchema")
		respSchema      string
		reqImport       string // specifier with no extension (e.g., "./wavefront/gen/2026_05_17_pb")
		respImport      string
	}
	records := make([]*routeRec, 0, len(contracts))
	for _, c := range contracts {
		reqType, reqImport, ierr := messageImport(fds, c.RequestMessage)
		if ierr != nil {
			return "", fmt.Errorf("gen-ts-client: contract %q request: %w", c.ContractVersion, ierr)
		}
		respType, respImport, ierr := messageImport(fds, c.ResponseMessage)
		if ierr != nil {
			return "", fmt.Errorf("gen-ts-client: contract %q response: %w", c.ContractVersion, ierr)
		}
		rec := &routeRec{
			method:          strings.ToUpper(c.Method),
			path:            c.Route,
			contractVersion: c.ContractVersion,
			reqType:         reqType,
			respType:        respType,
			reqSchema:       reqType + "Schema",
			respSchema:      respType + "Schema",
			reqImport:       reqImport,
			respImport:      respImport,
		}
		// Prefer an explicit operationId from the OpenAPI doc when present
		// for the (method, path) pair; fall back to a deterministic derivation
		// otherwise. Camel-casing both branches keeps map keys idiomatic for
		// TypeScript callers.
		if explicit := opIDs[methodPathKey(c.Method, c.Route)]; explicit != "" {
			rec.opID = camelCaseOperationID(explicit)
		} else {
			rec.opID = deriveOperationID(c.Method, c.Route)
		}
		records = append(records, rec)
	}

	// Deterministic order: sort by (path, method). Same-(path, method)
	// across different contract_version values keeps the first-seen entry
	// — the bundle layer's versions.yaml authors that ordering, so we
	// don't override it. Tie-break on contractVersion just to keep
	// pathologic duplicates stable.
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].path != records[j].path {
			return records[i].path < records[j].path
		}
		if records[i].method != records[j].method {
			return records[i].method < records[j].method
		}
		return records[i].contractVersion < records[j].contractVersion
	})

	// Detect duplicate opIDs — they would shadow each other in the map and
	// silently drop a route. Better to fail loudly with a message that
	// names both colliding contracts than to emit broken TS.
	seenOpID := map[string]string{} // opID → contractVersion
	for _, r := range records {
		if prev, dup := seenOpID[r.opID]; dup {
			return "", fmt.Errorf("gen-ts-client: operation id %q is shared by contracts %q and %q; set an explicit operationId in the OpenAPI doc to disambiguate", r.opID, prev, r.contractVersion)
		}
		seenOpID[r.opID] = r.contractVersion
	}

	// Group symbols per import file so we emit one combined import line per
	// file rather than one per symbol. Each unique import-specifier maps to
	// a sorted set of "type Foo"/"FooSchema" pieces.
	type importGroup struct {
		specifier string
		types     map[string]struct{} // bare type names (Req, Pong)
		values    map[string]struct{} // schema constants (ReqSchema, PongSchema)
	}
	groups := map[string]*importGroup{}
	addSymbol := func(spec, typ, val string) {
		g, ok := groups[spec]
		if !ok {
			g = &importGroup{
				specifier: spec,
				types:     map[string]struct{}{},
				values:    map[string]struct{}{},
			}
			groups[spec] = g
		}
		g.types[typ] = struct{}{}
		g.values[val] = struct{}{}
	}
	for _, r := range records {
		addSymbol(r.reqImport, r.reqType, r.reqSchema)
		addSymbol(r.respImport, r.respType, r.respSchema)
	}
	sortedImports := make([]*importGroup, 0, len(groups))
	for _, g := range groups {
		sortedImports = append(sortedImports, g)
	}
	sort.Slice(sortedImports, func(i, j int) bool {
		return sortedImports[i].specifier < sortedImports[j].specifier
	})

	// Render. Use \n as the line separator on every platform — the file is
	// consumed by tsc/bundlers, not native shells, and a stable byte stream
	// keeps the determinism test honest on Windows runners.
	var b strings.Builder
	b.WriteString("// @generated by wavefront-bundle gen-ts-client\n")
	b.WriteString("// Source: bundle layer \"" + version + "\"\n")
	b.WriteString("// DO NOT EDIT. Regenerate via `wavefront-bundle gen-ts-client`.\n")
	b.WriteString("//\n")
	b.WriteString("// Route map: each entry exposes the HTTP method, path, contract\n")
	b.WriteString("// version, and the request/response Schema constants from the\n")
	b.WriteString("// emitted protoc-gen-es message files. Consumers index by the\n")
	b.WriteString("// operation id (camel-cased from the OpenAPI operationId, or\n")
	b.WriteString("// derived from method+path when absent).\n")
	b.WriteString("/* eslint-disable */\n")
	b.WriteString("\n")
	b.WriteString("import type { Message } from \"@bufbuild/protobuf\";\n")
	b.WriteString("import type { GenMessage } from \"@bufbuild/protobuf/codegenv2\";\n")
	b.WriteString("\n")
	for _, g := range sortedImports {
		types := sortedSetKeys(g.types)
		values := sortedSetKeys(g.values)
		// One combined import per file: types are inlined with the `type`
		// modifier so bundlers can drop them during tree-shaking.
		var parts []string
		for _, t := range types {
			parts = append(parts, "type "+t)
		}
		parts = append(parts, values...)
		b.WriteString("import { " + strings.Join(parts, ", ") + " } from \"" + g.specifier + "\";\n")
	}
	b.WriteString("\n")
	b.WriteString("export interface Route<TReq extends Message, TResp extends Message> {\n")
	b.WriteString("  readonly method: \"GET\" | \"POST\" | \"PUT\" | \"PATCH\" | \"DELETE\" | \"HEAD\" | \"OPTIONS\";\n")
	b.WriteString("  readonly path: string;\n")
	b.WriteString("  readonly contractVersion: string;\n")
	b.WriteString("  readonly requestSchema: GenMessage<TReq>;\n")
	b.WriteString("  readonly responseSchema: GenMessage<TResp>;\n")
	b.WriteString("}\n")
	b.WriteString("\n")
	b.WriteString("export const routes = {\n")
	for _, r := range records {
		b.WriteString("  " + r.opID + ": {\n")
		b.WriteString("    method: \"" + r.method + "\",\n")
		b.WriteString("    path: \"" + r.path + "\",\n")
		b.WriteString("    contractVersion: \"" + r.contractVersion + "\",\n")
		b.WriteString("    requestSchema: " + r.reqSchema + ",\n")
		b.WriteString("    responseSchema: " + r.respSchema + ",\n")
		b.WriteString("  } as const satisfies Route<" + r.reqType + ", " + r.respType + ">,\n")
	}
	b.WriteString("} as const;\n")
	b.WriteString("\n")
	b.WriteString("export type Routes = typeof routes;\n")

	out := filepath.Join(outDir, routesFileName)
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("gen-ts-client: write routes.ts: %w", err)
	}
	return routesFileName, nil
}

// sortedSetKeys returns the keys of a set-typed map as a sorted slice. Kept
// distinct from drift.go's sortedKeys (which is typed for the OpenAPI
// schema map) so we don't have to widen either signature with generics.
func sortedSetKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// methodPathKey is the canonical lookup key for an OpenAPI operation by its
// (method, path) pair. Method is upper-cased so HTTP-method casing in the
// OpenAPI doc (which is lower-case by spec) and in versions.yaml (which we
// upper-case at emit time) can be reconciled to the same map entry.
func methodPathKey(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

// loadOperationIDs reads <layerDir>/openapi.json and returns a map of
// (METHOD path) → operationId for every operation whose `operationId` is
// non-empty. Operations without an operationId contribute nothing to the
// map (the caller falls back to deriveOperationID for those).
func loadOperationIDs(layerDir string) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(layerDir, "openapi.json"))
	if err != nil {
		return nil, fmt.Errorf("gen-ts-client: read openapi.json: %w", err)
	}
	// Decode just enough of the doc to read paths.*.{method}.operationId.
	// A minimal local schema keeps this independent of bundlegen.go's
	// stricter generator-input shape.
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("gen-ts-client: parse openapi.json: %w", err)
	}
	out := map[string]string{}
	for path, methods := range doc.Paths {
		for method, op := range methods {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			if id := strings.TrimSpace(op.OperationID); id != "" {
				out[methodPathKey(method, path)] = id
			}
		}
	}
	return out, nil
}

// layerContract mirrors versions.yaml's contract entry shape, kept local
// to this file so routes-emission stays decoupled from bundlegen.go's
// generator and from bundle.go's runtime-loader strict-decoder.
type layerContract struct {
	ContractVersion string `yaml:"contract_version"`
	Route           string `yaml:"route"`
	Method          string `yaml:"method"`
	RequestMessage  string `yaml:"request_message"`
	ResponseMessage string `yaml:"response_message"`
}

// loadLayerContracts reads <layerDir>/versions.yaml and returns the
// contracts list. Errors propagate the underlying yaml or filesystem
// failure verbatim.
func loadLayerContracts(layerDir string) ([]layerContract, error) {
	raw, err := os.ReadFile(filepath.Join(layerDir, "versions.yaml"))
	if err != nil {
		return nil, fmt.Errorf("gen-ts-client: read versions.yaml: %w", err)
	}
	var doc struct {
		Contracts []layerContract `yaml:"contracts"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("gen-ts-client: parse versions.yaml: %w", err)
	}
	return doc.Contracts, nil
}

// messageImport resolves a fully-qualified proto message name (e.g.
// "wavefront.gen.v2026_05_17.Req") to the TypeScript-side symbol name
// ("Req") and the import specifier ("./wavefront/gen/2026_05_17_pb") of
// the file that protoc-gen-es emitted for that message's package.
//
// The proto-file → ts-file map follows the protoc-gen-es convention:
// `foo/bar.proto` → `foo/bar_pb.ts`. The specifier omits the `.ts`
// extension because we run protoc-gen-es with `import_extension=none`.
// Leading `./` makes the specifier resolve relative to routes.ts which
// lives at the root of --out.
func messageImport(fds *descriptorpb.FileDescriptorSet, fullName string) (typeName, specifier string, err error) {
	dot := strings.LastIndex(fullName, ".")
	if dot < 0 {
		return "", "", fmt.Errorf("invalid message name %q (no package separator)", fullName)
	}
	pkg := fullName[:dot]
	typeName = fullName[dot+1:]

	// Find the file in the FDS that owns this package + declares this
	// message. Most v0.1 layers have a single file per package so this is
	// a one-shot lookup; v0.2 layers with multiple files-per-package would
	// still find the right one because we also check the message list.
	for _, f := range fds.File {
		if f.GetPackage() != pkg {
			continue
		}
		for _, m := range f.GetMessageType() {
			if m.GetName() == typeName {
				name := f.GetName()
				// foo/bar.proto → foo/bar_pb (no extension; pulled in by
				// import_extension=none on the protoc-gen-es side).
				stripped := strings.TrimSuffix(name, ".proto")
				if stripped == name {
					return "", "", fmt.Errorf("file %q does not end in .proto", name)
				}
				specifier = "./" + stripped + "_pb"
				return typeName, specifier, nil
			}
		}
	}
	return "", "", fmt.Errorf("message %q not found in any descriptor file", fullName)
}

// deriveOperationID computes a deterministic camel-case identifier from an
// HTTP method and path when the OpenAPI doc does not supply an explicit
// operationId. Algorithm:
//
//   - Method lower-cased becomes the leading word.
//   - Path split on "/"; each non-empty segment that does NOT look like
//     `{param}` is split further on non-identifier runes ("-", ".",
//     etc.) and title-cased; the lower-cased pieces are appended.
//   - Path-parameter segments contribute nothing — the derived ID stays
//     stable across parameter renamings.
//   - "/" by itself yields just the method name.
//
// The result is suitable as both a JavaScript identifier and a TypeScript
// object literal key.
func deriveOperationID(method, path string) string {
	method = strings.ToLower(strings.TrimSpace(method))
	var b strings.Builder
	b.WriteString(method)
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			// Path parameter — skip so route renamings keep the same id.
			continue
		}
		for _, word := range splitNonIdent(seg) {
			b.WriteString(titleCase(word))
		}
	}
	return b.String()
}

// camelCaseOperationID converts an explicit OpenAPI operationId into the
// camelCase shape we use as a routes-map key. Hyphens, dots, slashes,
// underscores, and whitespace become word boundaries. If the input already
// is a single word it is returned as-is so already-mixed-case ids
// (`getItems`) survive the round-trip without being mangled. Multi-word
// inputs are stitched together with the first word lower-cased and later
// words title-cased.
func camelCaseOperationID(id string) string {
	words := splitNonIdent(id)
	if len(words) == 0 {
		return ""
	}
	if len(words) == 1 {
		return words[0]
	}
	var b strings.Builder
	for i, w := range words {
		if i == 0 {
			b.WriteString(strings.ToLower(w))
			continue
		}
		b.WriteString(titleCase(w))
	}
	return b.String()
}

// splitNonIdent breaks s into ASCII word pieces at separator runes — any
// rune that is not an ASCII letter or digit (so "-", ".", "/", "_",
// whitespace, etc. all act as word boundaries). Empty pieces are dropped.
// Underscore is treated as a separator on purpose so snake_case inputs
// camelCase cleanly; an explicit operationId of "get_items" should yield
// the same key as "get-items".
func splitNonIdent(s string) []string {
	out := []string{}
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// titleCase upper-cases the first rune of word and leaves the rest
// unchanged. Used by the camel-case helpers so that already-mixed-case
// inputs (e.g. "getItems") survive a round-trip without being mangled.
func titleCase(word string) string {
	if word == "" {
		return ""
	}
	rs := []rune(word)
	rs[0] = unicode.ToUpper(rs[0])
	return string(rs)
}
