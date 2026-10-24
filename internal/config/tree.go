package config

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Editor makes several edits to a cluster.yaml document as one YAML tree,
// so comments and key order survive (like AddModel and friends, but for
// changes that touch many places, e.g. a topology plan). Paths are under
// spec. Bytes validates the result as a document.
type Editor struct {
	doc  yaml.Node
	spec *yaml.Node
}

// NewEditor parses data for editing.
func NewEditor(data []byte) (*Editor, error) {
	e := &Editor{}
	if err := yaml.Unmarshal(data, &e.doc); err != nil {
		return nil, err
	}
	if e.doc.Kind != yaml.DocumentNode || len(e.doc.Content) == 0 || e.doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("cluster.yaml is not a mapping")
	}
	if e.spec = mappingChild(e.doc.Content[0], "spec"); e.spec == nil || e.spec.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("cluster.yaml has no spec")
	}
	return e, nil
}

func valueNode(v any) (*yaml.Node, error) {
	n := &yaml.Node{}
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return n, nil
}

// Set replaces (or adds) the value at path under spec, creating mappings on
// the way.
func (e *Editor) Set(v any, path ...string) error {
	if len(path) == 0 {
		return fmt.Errorf("empty path")
	}
	m := e.spec
	for _, k := range path[:len(path)-1] {
		m = mappingChildOrCreate(m, k)
	}
	n, err := valueNode(v)
	if err != nil {
		return err
	}
	leaf := path[len(path)-1]
	if c := mappingChild(m, leaf); c != nil {
		*c = *n
		return nil
	}
	m.Content = append(m.Content, scalar(leaf), n)
	return nil
}

// Append adds v to the sequence at path under spec, creating it.
func (e *Editor) Append(v any, path ...string) error {
	if len(path) == 0 {
		return fmt.Errorf("empty path")
	}
	m := e.spec
	for _, k := range path[:len(path)-1] {
		m = mappingChildOrCreate(m, k)
	}
	n, err := valueNode(v)
	if err != nil {
		return err
	}
	seq := sequenceChild(m, path[len(path)-1])
	seq.Content = append(seq.Content, n)
	return nil
}

// Bytes encodes the edited document and checks that it still parses.
func (e *Editor) Bytes() ([]byte, error) {
	out, err := encode(&e.doc)
	if err != nil {
		return nil, err
	}
	if _, err := ParseCluster(out); err != nil {
		return nil, fmt.Errorf("edited cluster.yaml: %w", err)
	}
	return out, nil
}

// Path is a readable form of an edit path for messages.
func Path(path ...string) string { return "spec." + strings.Join(path, ".") }
