package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// yamlToJSONValue decodes a single YAML document into the JSON data model
// (map[string]any, []any, string, json.Number, bool, nil) for schema validation.
//
// Scalars are mapped by YAML tag, but timestamps stay strings: the schema
// describes dates as strings, and implicit YAML timestamps would otherwise
// turn `2026-11-01` into a time.Time.
func yamlToJSONValue(data []byte) (any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		return nil, errors.New("empty document")
	}
	return nodeToValue(&doc)
}

func nodeToValue(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		return nodeToValue(n.Content[0])
	case yaml.AliasNode:
		return nodeToValue(n.Alias)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: mapping keys must be scalars", k.Line)
			}
			if _, dup := m[k.Value]; dup {
				return nil, fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			val, err := nodeToValue(v)
			if err != nil {
				return nil, err
			}
			m[k.Value] = val
		}
		return m, nil
	case yaml.SequenceNode:
		s := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			val, err := nodeToValue(c)
			if err != nil {
				return nil, err
			}
			s = append(s, val)
		}
		return s, nil
	case yaml.ScalarNode:
		return scalarToValue(n)
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

func scalarToValue(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool":
		b, err := strconv.ParseBool(n.Value)
		if err != nil {
			// YAML 1.1 forms such as "yes" are not booleans in yaml.v3; keep as string.
			return n.Value, nil
		}
		return b, nil
	case "!!int", "!!float":
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		if f, ok := v.(float64); ok && (math.IsInf(f, 0) || math.IsNaN(f)) {
			return nil, fmt.Errorf("line %d: %s is not a valid number", n.Line, n.Value)
		}
		return json.Number(fmt.Sprint(v)), nil
	default: // !!str, !!timestamp, !!binary, custom tags
		return n.Value, nil
	}
}
