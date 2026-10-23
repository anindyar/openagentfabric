package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// AddModel adds a model deployment to a cluster.yaml document unless one with
// that name exists, keeping comments and order. It reports whether the
// document changed. Like AddAPIKey, the result is validated as a document;
// the catalog checks are the caller's (oaf validate).
func AddModel(data []byte, d ModelDeployment) ([]byte, bool, error) {
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
	list := sequenceChild(spec, "models")
	for _, item := range list.Content {
		if n := mappingChild(item, "name"); n != nil && n.Value == d.Name {
			return data, false, nil
		}
	}

	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(k string, v *yaml.Node) { entry.Content = append(entry.Content, scalar(k), v) }
	num := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v} }
	add("name", scalar(d.Name))
	add("catalogRef", scalar(d.CatalogRef))
	placement := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
	if d.Placement.Node != "" {
		placement.Content = append(placement.Content, scalar("node"), scalar(d.Placement.Node))
	} else {
		placement.Content = append(placement.Content, scalar("group"), scalar(d.Placement.Group))
	}
	add("placement", placement)
	add("gpuMemoryUtilization", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(d.GPUMemoryUtilization, 'f', -1, 64)})
	if d.MaxModelLen > 0 {
		add("maxModelLen", num(strconv.Itoa(d.MaxModelLen)))
	}
	list.Content = append(list.Content, entry)

	out, err := encode(&doc)
	if err != nil {
		return nil, false, err
	}
	if _, err := ParseCluster(out); err != nil {
		return nil, false, fmt.Errorf("adding model %q: %w", d.Name, err)
	}
	return out, true, nil
}

// SetSpecBool sets the boolean at path under spec (e.g. observability,
// logs, enabled), creating the mappings on the way and keeping comments and
// order. It reports whether the document changed; the result is validated.
func SetSpecBool(data []byte, value bool, path ...string) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("cluster.yaml is not a mapping")
	}
	m := mappingChild(doc.Content[0], "spec")
	if m == nil {
		return nil, false, fmt.Errorf("cluster.yaml has no spec")
	}
	if len(path) == 0 {
		return nil, false, fmt.Errorf("empty path")
	}
	for _, k := range path[:len(path)-1] {
		m = mappingChildOrCreate(m, k)
	}
	want := strconv.FormatBool(value)
	leaf := path[len(path)-1]
	if c := mappingChild(m, leaf); c != nil {
		if c.Kind == yaml.ScalarNode && c.Value == want {
			return data, false, nil
		}
		*c = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: want}
	} else {
		m.Content = append(m.Content, scalar(leaf), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: want})
	}
	out, err := encode(&doc)
	if err != nil {
		return nil, false, err
	}
	if _, err := ParseCluster(out); err != nil {
		return nil, false, fmt.Errorf("setting spec.%s: %w", strings.Join(path, "."), err)
	}
	return out, true, nil
}

// SetSpecString sets the string at path under spec, like SetSpecBool; an
// empty value removes the key.
func SetSpecString(data []byte, value string, path ...string) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("cluster.yaml is not a mapping")
	}
	m := mappingChild(doc.Content[0], "spec")
	if m == nil {
		return nil, false, fmt.Errorf("cluster.yaml has no spec")
	}
	if len(path) == 0 {
		return nil, false, fmt.Errorf("empty path")
	}
	for _, k := range path[:len(path)-1] {
		m = mappingChildOrCreate(m, k)
	}
	leaf := path[len(path)-1]
	idx := -1
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == leaf {
			idx = i
		}
	}
	switch {
	case idx >= 0 && value == "":
		m.Content = append(m.Content[:idx], m.Content[idx+2:]...)
	case idx >= 0 && m.Content[idx+1].Value == value:
		return data, false, nil
	case idx >= 0:
		*m.Content[idx+1] = *scalar(value)
	case value == "":
		return data, false, nil
	default:
		m.Content = append(m.Content, scalar(leaf), scalar(value))
	}
	out, err := encode(&doc)
	if err != nil {
		return nil, false, err
	}
	if _, err := ParseCluster(out); err != nil {
		return nil, false, fmt.Errorf("setting spec.%s: %w", strings.Join(path, "."), err)
	}
	return out, true, nil
}

// SetScrapeTarget adds or replaces the app scrape target with t's name under
// spec.observability.metrics.scrape, keeping comments and order.
func SetScrapeTarget(data []byte, t ScrapeTarget) ([]byte, bool, error) {
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
	list := sequenceChild(mappingChildOrCreate(mappingChildOrCreate(spec, "observability"), "metrics"), "scrape")
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	entry.Content = append(entry.Content, scalar("name"), scalar(t.Name), scalar("url"), scalar(t.URL))
	if t.InsecureSkipVerify {
		entry.Content = append(entry.Content, scalar("insecureSkipVerify"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	}
	replaced := false
	for i, item := range list.Content {
		if n := mappingChild(item, "name"); n != nil && n.Value == t.Name {
			u := mappingChild(item, "url")
			ins := mappingChild(item, "insecureSkipVerify")
			if u != nil && u.Value == t.URL && (ins != nil && ins.Value == "true") == t.InsecureSkipVerify {
				return data, false, nil
			}
			list.Content[i], replaced = entry, true
		}
	}
	if !replaced {
		list.Content = append(list.Content, entry)
	}
	out, err := encode(&doc)
	if err != nil {
		return nil, false, err
	}
	if _, err := ParseCluster(out); err != nil {
		return nil, false, fmt.Errorf("adding scrape target %q: %w", t.Name, err)
	}
	return out, true, nil
}

// WriteFileAtomic replaces path with data, keeping its permissions, so a
// reader never sees a half-written cluster.yaml.
func WriteFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func encode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
