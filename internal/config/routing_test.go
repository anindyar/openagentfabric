package config

import (
	"strings"
	"testing"
)

func TestActiveRoutesPairExample(t *testing.T) {
	c := mustCluster(t, pairExample)
	rt := c.ActiveRoutes(mustCatalog(t), nil)
	if !rt.Enabled() || rt.Fast != "fast" || rt.Fallback != "fast" || !rt.NeedsEncoder {
		t.Fatalf("routes = %+v", rt)
	}
	if len(rt.Rules) != 1 || rt.Rules[0].Name != "think" || len(rt.Rules[0].Signals) != 3 {
		t.Fatalf("active rules = %+v", rt.Rules)
	}
	if len(rt.Skipped) != 1 || !strings.Contains(rt.Skipped[0], `entitlement "ddx"`) {
		t.Fatalf("skipped = %v", rt.Skipped)
	}
}

// Absent, embedding or unsupported targets are never routed to.
func TestActiveRoutesDropsWhatCannotWork(t *testing.T) {
	c := mustCluster(t, pairExample)
	c.Spec.Routing.Rules = []RoutingRule{
		{Name: "gone", Target: "nope", Match: RuleMatch{Signals: []Signal{{Type: SignalKeyword, Keywords: []string{"x"}}}}},
		{Name: "domain-only", Target: "think", Match: RuleMatch{Signals: []Signal{{Type: SignalDomain, Domains: []string{"law"}}}}},
		{Name: "keywords", Target: "think", Match: RuleMatch{Signals: []Signal{{Type: SignalKeyword, Keywords: []string{"prove"}}, {Type: SignalDomain, Domains: []string{"math"}}}}},
	}
	rt := c.ActiveRoutes(mustCatalog(t), nil)
	if len(rt.Rules) != 1 || rt.Rules[0].Name != "keywords" || len(rt.Rules[0].Signals) != 1 || rt.NeedsEncoder {
		t.Fatalf("routes = %+v", rt)
	}
	joined := strings.Join(rt.Skipped, "\n")
	for _, want := range []string{`rule "gone"`, `rule "domain-only": no supported signal`, `rule "keywords": domain signals`} {
		if !strings.Contains(joined, want) {
			t.Errorf("skipped lacks %q:\n%s", want, joined)
		}
	}

	c.Spec.Routing.Rules = nil
	if c.ActiveRoutes(mustCatalog(t), nil).Enabled() {
		t.Fatal("no rules must mean no router")
	}
}

// Reasoning signals charge the encoder's memory to the gateway node.
// At 0.32, spark-1 is just inside the budget without the encoder (0.32 +
// 0.50 + 2 x 4.6 GiB of vLLM overhead) and just over with it.
func TestEncoderMemoryCharged(t *testing.T) {
	overBudget := func(c *Cluster) bool {
		for _, w := range Validate(c, mustCatalog(t), Options{}).Warnings() {
			if strings.Contains(w.Message, "of vLLM overhead per model") {
				return true
			}
		}
		return false
	}
	c := mustCluster(t, pairExample)
	for i := range c.Spec.Models {
		if c.Spec.Models[i].Name == "fast" {
			c.Spec.Models[i].GPUMemoryUtilization = 0.32
		}
	}
	if !overBudget(c) {
		t.Fatal("with the encoder, spark-1 should be over budget")
	}
	c.Spec.Routing.Rules[0].Match.Signals = []Signal{{Type: SignalKeyword, Keywords: []string{"prove"}}}
	if overBudget(c) {
		t.Fatal("without the encoder, spark-1 should fit")
	}
}
