package importer

import (
	"bytes"

	"gopkg.in/yaml.v3"

	"github.com/xxddccaa/akey/internal/spec"
)

// Marshal renders a master as YAML like spec.Master.Marshal, but writes
// lists of scalars inline ([low, medium, high]) so the draft stays short.
func Marshal(m *spec.Master) ([]byte, error) {
	var n yaml.Node
	if err := n.Encode(m); err != nil {
		return nil, err
	}
	flowScalarLists(&n)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func flowScalarLists(n *yaml.Node) {
	if n.Kind == yaml.SequenceNode && len(n.Content) > 0 {
		flat := true
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode || bytes.ContainsAny([]byte(c.Value), "\n") {
				flat = false
				break
			}
		}
		if flat {
			n.Style = yaml.FlowStyle
		}
	}
	for _, c := range n.Content {
		flowScalarLists(c)
	}
}
