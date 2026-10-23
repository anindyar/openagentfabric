package config

import (
	"bytes"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// AddAPIKey adds a gateway API key to a cluster.yaml document unless one with
// that name already exists, editing the YAML tree so comments and key order
// survive. It reports whether the document changed. The result is validated,
// so a document AddAPIKey returns always loads.
func AddAPIKey(data []byte, key APIKey) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("cluster.yaml is not a mapping")
	}
	spec := mappingChild(doc.Content[0], "spec")
	if spec == nil {
		return nil, false, fmt.Errorf("cluster.yaml has no spec")
	}
	keys := sequenceChild(mappingChildOrCreate(spec, "routing"), "keys")
	for _, item := range keys.Content {
		if n := mappingChild(item, "name"); n != nil && n.Value == key.Name {
			return data, false, nil
		}
	}

	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	entry.Content = append(entry.Content, scalar("name"), scalar(key.Name), scalar("secretRef"), scalar(key.SecretRef))
	if len(key.Models) > 0 {
		models := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
		for _, m := range key.Models {
			models.Content = append(models.Content, scalar(m))
		}
		entry.Content = append(entry.Content, scalar("models"), models)
	}
	keys.Content = append(keys.Content, entry)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	if _, err := ParseCluster(buf.Bytes()); err != nil {
		return nil, false, fmt.Errorf("adding key %q: %w", key.Name, err)
	}
	return buf.Bytes(), true, nil
}

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// mappingChild returns the value node for key in mapping m, or nil.
func mappingChild(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mappingChildOrCreate(m *yaml.Node, key string) *yaml.Node {
	if c := mappingChild(m, key); c != nil {
		if c.Kind != yaml.MappingNode {
			// An empty value ("routing:") parses as a null scalar.
			*c = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		return c
	}
	c := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, scalar(key), c)
	return c
}

// sequenceChild returns the sequence under key in mapping m, creating it.
func sequenceChild(m *yaml.Node, key string) *yaml.Node {
	if c := mappingChild(m, key); c != nil {
		if c.Kind != yaml.SequenceNode {
			*c = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		}
		// Block style, so appended entries read like hand-written ones.
		c.Style = 0
		return c
	}
	c := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	m.Content = append(m.Content, scalar(key), c)
	return c
}
