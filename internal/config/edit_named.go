package config

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Tree edits for the management API (oafd): add, replace or remove a named
// item of a list under spec, or set a field, keeping the rest of
// cluster.yaml (comments, order) as it is. Each returns the new document and
// whether it changed; results are parsed (not semantically validated: the
// caller validates with the catalog before offering a plan).

func editDoc(data []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("cluster.yaml is not a mapping")
	}
	spec := mappingChild(doc.Content[0], "spec")
	if spec == nil {
		return nil, nil, fmt.Errorf("cluster.yaml has no spec")
	}
	return &doc, spec, nil
}

func finishEdit(doc *yaml.Node, what string) ([]byte, error) {
	out, err := encode(doc)
	if err != nil {
		return nil, err
	}
	if _, err := ParseCluster(out); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

func nodeOf(v any) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}

// listAt walks path (mappings, created as needed) under spec and returns the
// sequence named by its last element.
func listAt(spec *yaml.Node, path []string) *yaml.Node {
	m := spec
	for _, k := range path[:len(path)-1] {
		m = mappingChildOrCreate(m, k)
	}
	return sequenceChild(m, path[len(path)-1])
}

func sameYAML(a, b *yaml.Node) bool {
	ea, err1 := yaml.Marshal(a)
	eb, err2 := yaml.Marshal(b)
	return err1 == nil && err2 == nil && string(ea) == string(eb)
}

// SetNamed adds item (a struct with a `name` field) to the list at path, or
// replaces the entry with the same name.
func SetNamed(data []byte, item any, name string, path ...string) ([]byte, bool, error) {
	doc, spec, err := editDoc(data)
	if err != nil {
		return nil, false, err
	}
	n, err := nodeOf(item)
	if err != nil {
		return nil, false, err
	}
	list := listAt(spec, path)
	for i, it := range list.Content {
		if c := mappingChild(it, "name"); c != nil && c.Value == name {
			if sameYAML(it, n) {
				return data, false, nil
			}
			list.Content[i] = n
			out, err := finishEdit(doc, "setting "+name)
			return out, err == nil, err
		}
	}
	list.Content = append(list.Content, n)
	out, err := finishEdit(doc, "adding "+name)
	return out, err == nil, err
}

// RemoveNamed removes the entry called name from the list at path. Removing
// a missing entry is an error, so a typo is not a silent no-op.
func RemoveNamed(data []byte, name string, path ...string) ([]byte, error) {
	doc, spec, err := editDoc(data)
	if err != nil {
		return nil, err
	}
	m := spec
	for _, k := range path[:len(path)-1] {
		if m = mappingChild(m, k); m == nil {
			return nil, fmt.Errorf("no %s %q", strings.Join(path, "."), name)
		}
	}
	list := mappingChild(m, path[len(path)-1])
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("no %s %q", strings.Join(path, "."), name)
	}
	for i, it := range list.Content {
		if c := mappingChild(it, "name"); c != nil && c.Value == name {
			list.Content = append(list.Content[:i], list.Content[i+1:]...)
			return finishEdit(doc, "removing "+name)
		}
	}
	return nil, fmt.Errorf("no %s %q", strings.Join(path, "."), name)
}

// SetProvider adds or replaces an external provider.
func SetProvider(data []byte, p Provider) ([]byte, bool, error) {
	return SetNamed(data, p, p.Name, "routing", "providers")
}

// RemoveProvider removes an external provider.
func RemoveProvider(data []byte, name string) ([]byte, error) {
	return RemoveNamed(data, name, "routing", "providers")
}

// SetGuardrail adds or replaces a guardrail policy.
func SetGuardrail(data []byte, g GuardrailPolicy) ([]byte, bool, error) {
	return SetNamed(data, g, g.Name, "routing", "guardrails")
}

// RemoveGuardrail removes a guardrail policy.
func RemoveGuardrail(data []byte, name string) ([]byte, error) {
	return RemoveNamed(data, name, "routing", "guardrails")
}

// RemoveModel removes a model deployment.
func RemoveModel(data []byte, name string) ([]byte, error) {
	return RemoveNamed(data, name, "models")
}

// SetModelGuardrails sets (or clears) the guardrail policies of a model
// deployment.
func SetModelGuardrails(data []byte, model string, policies []string) ([]byte, bool, error) {
	doc, spec, err := editDoc(data)
	if err != nil {
		return nil, false, err
	}
	list := mappingChild(spec, "models")
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil, false, fmt.Errorf("no model deployment %q", model)
	}
	for _, it := range list.Content {
		if c := mappingChild(it, "name"); c == nil || c.Value != model {
			continue
		}
		n, err := nodeOf(policies)
		if err != nil {
			return nil, false, err
		}
		n.Style = yaml.FlowStyle
		if cur := mappingChild(it, "guardrails"); cur != nil {
			if sameYAML(cur, n) {
				return data, false, nil
			}
			if len(policies) == 0 {
				removeKey(it, "guardrails")
			} else {
				*cur = *n
			}
		} else if len(policies) > 0 {
			it.Content = append(it.Content, scalar("guardrails"), n)
		} else {
			return data, false, nil
		}
		out, err := finishEdit(doc, "setting guardrails of "+model)
		return out, err == nil, err
	}
	return nil, false, fmt.Errorf("no model deployment %q", model)
}

// RoutingUpdate is what PUT /routing may change. Nil fields are left alone.
type RoutingUpdate struct {
	Mode     *string          `json:"mode,omitempty"`
	Defaults *RoutingDefaults `json:"defaults,omitempty"`
	Rules    *[]RoutingRule   `json:"rules,omitempty"`
}

// SetRouting applies u to spec.routing.
func SetRouting(data []byte, u RoutingUpdate) ([]byte, bool, error) {
	doc, spec, err := editDoc(data)
	if err != nil {
		return nil, false, err
	}
	routing := mappingChildOrCreate(spec, "routing")
	before, _ := yaml.Marshal(routing)
	set := func(key string, v any, empty bool) error {
		if empty {
			removeKey(routing, key)
			return nil
		}
		n, err := nodeOf(v)
		if err != nil {
			return err
		}
		if cur := mappingChild(routing, key); cur != nil {
			*cur = *n
		} else {
			routing.Content = append(routing.Content, scalar(key), n)
		}
		return nil
	}
	if u.Mode != nil {
		if err := set("mode", *u.Mode, *u.Mode == ""); err != nil {
			return nil, false, err
		}
	}
	if u.Defaults != nil {
		if err := set("defaults", *u.Defaults, *u.Defaults == (RoutingDefaults{})); err != nil {
			return nil, false, err
		}
	}
	if u.Rules != nil {
		if err := set("rules", *u.Rules, len(*u.Rules) == 0); err != nil {
			return nil, false, err
		}
	}
	after, _ := yaml.Marshal(routing)
	if string(before) == string(after) {
		return data, false, nil
	}
	out, err := finishEdit(doc, "setting routing")
	return out, err == nil, err
}

func removeKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// BasicGuardrail is the recommended starting policy for external models
// (ADR-0002 §3): reject prompts that carry credentials or private keys, and
// mask contact and card data in responses. Only detectors the pinned
// agentgateway has are used.
func BasicGuardrail() GuardrailPolicy {
	return GuardrailPolicy{
		Name:        "basic",
		Description: "Recommended minimum for external models: no credentials or private keys in prompts; emails, phone and card numbers masked in responses.",
		Request: []Guard{{Regex: &RegexGuard{Action: "reject", Patterns: []string{
			`AKIA[0-9A-Z]{16}`,                                         // AWS access key id
			`-----BEGIN [A-Z ]*PRIVATE KEY-----`,                       // PEM private key
			`gh[pousr]_[A-Za-z0-9]{36,}`,                               // GitHub token
			`sk-[A-Za-z0-9_-]{20,}`,                                    // OpenAI-style secret key
			`xox[abprs]-[A-Za-z0-9-]{10,}`,                             // Slack token
			`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`,            // password in a URL
			`(?i)(password|passwd|secret|api[_-]?key)\s*[:=]\s*\S{6,}`, // key = value
		}}}},
		Response: []Guard{{Regex: &RegexGuard{Action: "mask", Builtins: []string{"email", "phoneNumber", "creditCard"}}}},
		Message:  "Blocked by the appliance's guardrails: the request contains a credential or private key.",
	}
}

// SetNodeHeadless sets spec.nodes[name].headless, keeping comments and order.
func SetNodeHeadless(data []byte, name string, headless bool) ([]byte, bool, error) {
	doc, spec, err := editDoc(data)
	if err != nil {
		return nil, false, err
	}
	list := mappingChild(spec, "nodes")
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil, false, fmt.Errorf("no node %q", name)
	}
	want := "false"
	if headless {
		want = "true"
	}
	for _, it := range list.Content {
		if c := mappingChild(it, "name"); c == nil || c.Value != name {
			continue
		}
		v := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: want}
		if cur := mappingChild(it, "headless"); cur != nil {
			if cur.Value == want {
				return data, false, nil
			}
			*cur = *v
		} else {
			it.Content = append(it.Content, scalar("headless"), v)
		}
		out, err := finishEdit(doc, "setting headless on "+name)
		return out, err == nil, err
	}
	return nil, false, fmt.Errorf("no node %q", name)
}
