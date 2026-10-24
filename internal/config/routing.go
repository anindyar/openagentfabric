package config

import "fmt"

// Signal types vSR evaluates for `model: auto`. keyword and context need no
// model; reasoning is vSR's complexity signal, which embeds the prompt with
// the vsr-encoder artifact (~1.7 GiB on CPU). domain needs vSR's domain
// classifier, which no bundle pins yet.
const (
	SignalKeyword   = "keyword"
	SignalContext   = "context"
	SignalReasoning = "reasoning"
	SignalDomain    = "domain"
)

// RouterEncoderArtifact is the bundle artifact reasoning signals need.
const RouterEncoderArtifact = "vsr-encoder"

// ActiveRule is a routing rule that will be rendered: its target is an
// enabled chat deployment, its entitlement (if any) is granted, and it has at
// least one signal vSR can evaluate.
type ActiveRule struct {
	RoutingRule
	Signals []Signal // the supported signals of the rule, in order
}

// Routes is what `model: auto` does on this cluster.
type Routes struct {
	Fast, Fallback string
	Rules          []ActiveRule
	// NeedsEncoder: some active rule has a reasoning signal.
	NeedsEncoder bool
	// Skipped explains each rule (or signal) that is not rendered.
	Skipped []string
}

// Enabled reports whether vSR runs at all: with no active rule, `auto` goes
// straight to the fast model.
func (r Routes) Enabled() bool { return len(r.Rules) > 0 }

// ActiveRoutes resolves spec.routing against the enabled deployments and the
// granted entitlements. Absent backends are never routed to (CLAUDE.md).
func (c *Cluster) ActiveRoutes(cat *Catalog, features map[string]bool) Routes {
	chat := map[string]bool{}
	firstChat := ""
	for _, d := range c.Spec.Models {
		if !d.IsEnabled() {
			continue
		}
		if m, ok := cat.Lookup(d.CatalogRef); ok && m.IsEmbedding() {
			continue
		}
		chat[d.Name] = true
		if firstChat == "" {
			firstChat = d.Name
		}
	}
	rt := c.Spec.Routing
	out := Routes{Fast: rt.Defaults.Fast, Fallback: rt.Defaults.Fallback}
	if out.Fast == "" {
		out.Fast = firstChat
	}
	if out.Fallback == "" || !chat[out.Fallback] {
		out.Fallback = out.Fast
	}
	if out.Fast == "" || !chat[out.Fast] {
		return Routes{Skipped: []string{"no chat model is deployed; model: auto is not served"}}
	}
	for _, rule := range rt.Rules {
		if rule.Requires != nil && rule.Requires.Entitlement != "" && !features[rule.Requires.Entitlement] {
			out.Skipped = append(out.Skipped, fmt.Sprintf("rule %q: entitlement %q is not present", rule.Name, rule.Requires.Entitlement))
			continue
		}
		if !chat[rule.Target] {
			out.Skipped = append(out.Skipped, fmt.Sprintf("rule %q: %q is not an enabled chat deployment", rule.Name, rule.Target))
			continue
		}
		var sigs []Signal
		for _, s := range rule.Match.Signals {
			switch s.Type {
			case SignalKeyword, SignalContext:
				sigs = append(sigs, s)
			case SignalReasoning:
				sigs = append(sigs, s)
				out.NeedsEncoder = true
			default:
				out.Skipped = append(out.Skipped, fmt.Sprintf("rule %q: %s signals are not supported yet (vSR's domain classifier is not pinned); signal ignored", rule.Name, s.Type))
			}
		}
		if len(sigs) == 0 {
			out.Skipped = append(out.Skipped, fmt.Sprintf("rule %q: no supported signal left", rule.Name))
			continue
		}
		out.Rules = append(out.Rules, ActiveRule{RoutingRule: rule, Signals: sigs})
	}
	return out
}
