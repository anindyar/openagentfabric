package config

import (
	"fmt"
	"reflect"

	"go.yaml.in/yaml/v3"
)

// Upserts for the CLI an installer drives (`oaf models adapter`, `oaf
// routing rule`): the item is compared by value, not by YAML spelling, so a
// hand-written flow-style entry with the same settings is left untouched.
// The rest of cluster.yaml (comments, order) is kept. Each returns the old
// item (nil when it is new) and whether the document changed.

// SetAdapter adds or replaces adapter a on deployment.
func SetAdapter(data []byte, deployment string, a Adapter) ([]byte, *Adapter, bool, error) {
	doc, spec, err := editDoc(data)
	if err != nil {
		return nil, nil, false, err
	}
	d := namedItem(mappingChild(spec, "models"), deployment)
	if d == nil {
		return nil, nil, false, fmt.Errorf("no model deployment %q", deployment)
	}
	var list *yaml.Node
	if cur := mappingChild(d, "adapters"); cur != nil && cur.Kind == yaml.SequenceNode {
		list = cur // keep its style
	} else {
		list = sequenceChild(d, "adapters")
	}
	old, changed, err := upsert(list, a.Name, a, -1)
	if err != nil || !changed {
		return data, old, false, err
	}
	out, err := finishEdit(doc, "setting adapter "+a.Name)
	return out, old, err == nil, err
}

// RemoveAdapter removes adapter name from deployment (and the empty list).
// Removing a missing adapter is an error, so a typo is not a silent no-op.
func RemoveAdapter(data []byte, deployment, name string) ([]byte, error) {
	doc, spec, err := editDoc(data)
	if err != nil {
		return nil, err
	}
	d := namedItem(mappingChild(spec, "models"), deployment)
	if d == nil {
		return nil, fmt.Errorf("no model deployment %q", deployment)
	}
	list := mappingChild(d, "adapters")
	i := namedIndex(list, name)
	if i < 0 {
		return nil, fmt.Errorf("deployment %q has no adapter %q", deployment, name)
	}
	list.Content = append(list.Content[:i], list.Content[i+1:]...)
	if len(list.Content) == 0 {
		removeKey(d, "adapters")
	}
	return finishEdit(doc, "removing adapter "+name)
}

// SetRoutingRule adds or replaces the rule with r's name. A new rule goes
// last, or before rule `before`; an existing one keeps its place unless
// before is given (rules are ordered: the first match wins).
func SetRoutingRule(data []byte, r RoutingRule, before string) ([]byte, *RoutingRule, bool, error) {
	doc, spec, err := editDoc(data)
	if err != nil {
		return nil, nil, false, err
	}
	routing := mappingChildOrCreate(spec, "routing")
	var list *yaml.Node
	if cur := mappingChild(routing, "rules"); cur != nil && cur.Kind == yaml.SequenceNode {
		list = cur
	} else {
		list = sequenceChild(routing, "rules")
	}
	pos := -1
	if before != "" {
		if before == r.Name {
			return nil, nil, false, fmt.Errorf("a rule cannot go before itself")
		}
		if pos = namedIndex(list, before); pos < 0 {
			return nil, nil, false, fmt.Errorf("no routing rule %q", before)
		}
	}
	old, changed, err := upsert(list, r.Name, r, pos)
	if err != nil || !changed {
		return data, old, false, err
	}
	out, err := finishEdit(doc, "setting rule "+r.Name)
	return out, old, err == nil, err
}

// RemoveRoutingRule removes a routing rule.
func RemoveRoutingRule(data []byte, name string) ([]byte, error) {
	return RemoveNamed(data, name, "routing", "rules")
}

// upsert puts item (named name) in list: replaced where it is, or appended;
// with pos >= 0 it ends up right before the entry now at pos. It reports the
// old value (decoded into item's type) and whether anything changed.
func upsert[T any](list *yaml.Node, name string, item T, pos int) (*T, bool, error) {
	n, err := nodeOf(item)
	if err != nil {
		return nil, false, err
	}
	flowScalarLists(n)
	i := namedIndex(list, name)
	var old *T
	if i >= 0 {
		var cur T
		if err := list.Content[i].Decode(&cur); err != nil {
			return nil, false, err
		}
		old = &cur
		inPlace := pos < 0 || pos == i+1 || pos == i
		if reflect.DeepEqual(normalized(cur), normalized(item)) && inPlace {
			return old, false, nil
		}
		if inPlace {
			list.Content[i] = n
			return old, true, nil
		}
		list.Content = append(list.Content[:i], list.Content[i+1:]...)
		if pos > i {
			pos--
		}
	}
	if pos < 0 || pos >= len(list.Content) {
		list.Content = append(list.Content, n)
	} else {
		list.Content = append(list.Content[:pos], append([]*yaml.Node{n}, list.Content[pos:]...)...)
	}
	return old, true, nil
}

// normalized round-trips v through YAML so that nil and empty optional
// lists left out by omitempty compare equal.
func normalized[T any](v T) T {
	var out T
	b, err := yaml.Marshal(v)
	if err == nil {
		_ = yaml.Unmarshal(b, &out)
	}
	return out
}

// flowScalarLists writes lists of plain values ([a, b, c]) on one line, as
// hand-written cluster.yaml files do.
func flowScalarLists(n *yaml.Node) {
	if n.Kind == yaml.SequenceNode && len(n.Content) > 0 {
		flat := true
		for _, c := range n.Content {
			flat = flat && c.Kind == yaml.ScalarNode
		}
		if flat {
			n.Style = yaml.FlowStyle
			return
		}
	}
	for _, c := range n.Content {
		flowScalarLists(c)
	}
}

func namedIndex(list *yaml.Node, name string) int {
	if list == nil || list.Kind != yaml.SequenceNode {
		return -1
	}
	for i, it := range list.Content {
		if c := mappingChild(it, "name"); c != nil && c.Value == name {
			return i
		}
	}
	return -1
}

func namedItem(list *yaml.Node, name string) *yaml.Node {
	if i := namedIndex(list, name); i >= 0 {
		return list.Content[i]
	}
	return nil
}
