package spec

import (
	"bytes"
	_ "embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

//go:embed template.yaml
var templateYAML []byte

// Template returns the commented starter master.yaml used when a passphrase
// has no stored config yet (CLI `akey edit` and the web panel).
func Template() []byte { return append([]byte(nil), templateYAML...) }

// Decode parses YAML strictly (unknown fields are errors) WITHOUT validating
// or filling defaults, so editors can round-trip exactly what the user wrote.
// Use Parse for anything that will be rendered or stored.
func Decode(b []byte) (*Master, error) {
	var m Master
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse master config: %w", err)
	}
	return &m, nil
}
