// Package bundle loads and validates the read-only descriptor bundle
// (descriptors.binpb + openapi.json + versions.yaml) at boot, fail-fast. The
// route → message binding is read verbatim (zero inference); every binding is
// resolved against the FileDescriptorSet here so the runtime never has to.
// A v0.2 bundle (transform stanzas) is refused by strict decode, not
// half-applied. SIGHUP hot-reload is v0.3; v0.1 loads once.
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

// Contract is one resolved v0.1 binding. Fields are unexported with accessors
// so it satisfies an adapter Binding interface (RequestMessage/ResponseMessage)
// structurally, with no import cycle.
type Contract struct {
	contractVersion string
	route           string
	method          string
	requestMessage  string
	responseMessage string
}

func (c *Contract) ContractVersion() string { return c.contractVersion }
func (c *Contract) Route() string           { return c.route }
func (c *Contract) Method() string          { return c.method }
func (c *Contract) RequestMessage() string  { return c.requestMessage }
func (c *Contract) ResponseMessage() string { return c.responseMessage }

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
		for _, msg := range []string{c.requestMessage, c.responseMessage} {
			d, ferr := files.FindDescriptorByName(protoreflect.FullName(msg))
			if ferr != nil {
				return nil, &MessageNotFoundError{Contract: c.contractVersion, Message: msg}
			}
			if _, ok := d.(protoreflect.MessageDescriptor); !ok {
				return nil, &MessageNotFoundError{Contract: c.contractVersion, Message: msg}
			}
		}
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
	dec.KnownFields(true) // refuse v0.2 transform stanzas / any unknown key
	var yb yamlBundle
	if err := dec.Decode(&yb); err != nil {
		return nil, &ParseError{File: fileVersions, Err: err}
	}
	return &yb, nil
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
