package bundle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const fileDiscriminators = "discriminators.yaml"

// discriminatorInfo is the per-wrapper discriminator metadata loaded from a
// layer's discriminators.yaml sidecar. It is held internally; the Bundle
// exposes it via Discriminator(name) using scalar returns, so the adapter can
// satisfy its DiscriminatorResolver interface structurally without importing
// this package's type.
type discriminatorInfo struct {
	propertyName string
	mapping      map[string]string
}

// yamlDiscriminatorsFile is the on-disk shape of discriminators.yaml. The file
// is optional per layer — a layer with no mapped discriminated unions ships no
// sidecar, so pre-#132 bundles stay byte-identical.
type yamlDiscriminatorsFile struct {
	Version int                 `yaml:"version"`
	Oneofs  []yamlDiscriminator `yaml:"oneofs"`
}

type yamlDiscriminator struct {
	Message      string            `yaml:"message"`
	PropertyName string            `yaml:"property_name"`
	Mapping      map[string]string `yaml:"mapping"`
}

// loadDiscriminators reads the optional discriminators.yaml sidecar for one
// layer. An absent file is not an error — most layers do not use mapped
// discriminated unions. A malformed or unknown-schema-version file is a hard
// error: shipping the sidecar implies the layer depends on the transform.
func loadDiscriminators(layerDir string) (map[string]discriminatorInfo, error) {
	raw, err := os.ReadFile(filepath.Join(layerDir, fileDiscriminators))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, &ReadError{File: fileDiscriminators, Err: err}
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var yd yamlDiscriminatorsFile
	if err := dec.Decode(&yd); err != nil {
		return nil, &ParseError{File: fileDiscriminators, Err: err}
	}
	if yd.Version != 1 {
		return nil, &UnsupportedVersionError{Version: yd.Version}
	}
	out := make(map[string]discriminatorInfo, len(yd.Oneofs))
	for _, o := range yd.Oneofs {
		msg := strings.TrimSpace(o.Message)
		if msg == "" {
			return nil, &ValidationError{Field: "oneofs.message", Reason: "wrapper message name must not be empty"}
		}
		if _, dup := out[msg]; dup {
			return nil, &ValidationError{Field: "oneofs.message", Reason: "duplicate wrapper message " + msg}
		}
		if strings.TrimSpace(o.PropertyName) == "" {
			return nil, &ValidationError{Field: "oneofs.property_name", Reason: "property_name must not be empty for " + msg}
		}
		if len(o.Mapping) == 0 {
			return nil, &ValidationError{Field: "oneofs.mapping", Reason: "mapping must not be empty for " + msg}
		}
		mapping := make(map[string]string, len(o.Mapping))
		for v, f := range o.Mapping {
			if strings.TrimSpace(f) == "" {
				return nil, &ValidationError{Field: "oneofs.mapping", Reason: "mapping value for " + v + " must not be empty"}
			}
			mapping[v] = f
		}
		out[msg] = discriminatorInfo{propertyName: strings.TrimSpace(o.PropertyName), mapping: mapping}
	}
	return out, nil
}

// Discriminator returns the mapped-discriminator metadata registered for a
// wrapper message, or (zero, nil, false) if the message is not a wrapper. The
// adapter type-asserts the resolver against this method via its
// DiscriminatorResolver interface and uses the pair to transform between the
// flattened upstream wire shape and the wrapped protojson shape (#132).
func (b *Bundle) Discriminator(messageFullName string) (propertyName string, mapping map[string]string, ok bool) {
	info, found := b.discriminators[messageFullName]
	if !found {
		return "", nil, false
	}
	return info.propertyName, info.mapping, true
}
