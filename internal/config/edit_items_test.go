package config

import (
	"os"
	"strings"
	"testing"
)

func TestSetAdapter(t *testing.T) {
	data, err := os.ReadFile(hospitalExample)
	if err != nil {
		t.Fatal(err)
	}
	// The example's adapter, as written by hand: no change, comments kept.
	same := Adapter{Name: "spetaar-ddx", Path: "/var/lib/oaf/adapters/spetaar-ddx", Rank: 16, Entitlement: "ddx"}
	out, old, changed, err := SetAdapter(data, "gpt-oss-20b", same)
	if err != nil || changed || old == nil || string(out) != string(data) {
		t.Fatalf("same adapter: changed=%v old=%v err=%v", changed, old, err)
	}
	// A second adapter is appended; guardrails are written on one line.
	out, old, changed, err = SetAdapter(data, "gpt-oss-20b", Adapter{Name: "triage", Path: "/a/triage", Rank: 8, Guardrails: []string{"g1", "g2"}})
	if err != nil || !changed || old != nil || !strings.Contains(string(out), "guardrails: [g1, g2]") ||
		!strings.Contains(string(out), "# PEFT LoRA (r=16") {
		t.Fatalf("new adapter: %v %v\n%s", changed, err, out)
	}
	c, _ := ParseCluster(out)
	if a := c.Spec.Models[1].Adapters; len(a) != 2 || a[1].Name != "triage" || a[0].Rank != 16 {
		t.Fatalf("adapters = %+v", a)
	}
	// A deployment without adapters gets the list.
	out, _, changed, err = SetAdapter(data, "medgemma", Adapter{Name: "m-lora", Path: "/a/m", Rank: 8})
	if c, _ := ParseCluster(out); err != nil || !changed || len(c.Spec.Models[2].Adapters) != 1 {
		t.Fatalf("first adapter on medgemma: %v %v", changed, err)
	}
	if _, _, _, err := SetAdapter(data, "nope", same); err == nil {
		t.Fatal("unknown deployment accepted")
	}
	// Removing the last adapter removes the list.
	out, err = RemoveAdapter(data, "gpt-oss-20b", "spetaar-ddx")
	if err != nil || strings.Contains(string(out), "adapters:") {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if _, err := RemoveAdapter(data, "gpt-oss-20b", "nope"); err == nil {
		t.Fatal("removing a missing adapter must fail")
	}
}

func TestSetRoutingRule(t *testing.T) {
	data, err := os.ReadFile(hospitalExample)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := ParseCluster(data)
	names := func(out []byte) string {
		c, err := ParseCluster(out)
		if err != nil {
			t.Fatal(err)
		}
		var n []string
		for _, r := range c.Spec.Routing.Rules {
			n = append(n, r.Name)
		}
		return strings.Join(n, ",")
	}
	// Written in flow style by hand, the same rule is no change.
	if out, old, changed, err := SetRoutingRule(data, c.Spec.Routing.Rules[0], ""); err != nil || changed || old == nil || string(out) != string(data) {
		t.Fatalf("same rule: %v %v", changed, err)
	}
	long := RoutingRule{Name: "long", Target: "ornith", Match: RuleMatch{Signals: []Signal{{Type: SignalContext, MinTokens: 4000}}}}
	out, _, changed, err := SetRoutingRule(data, long, "")
	if err != nil || !changed || names(out) != "differential,medical,long" {
		t.Fatalf("append: %v %v %s", changed, err, names(out))
	}
	out, _, _, err = SetRoutingRule(data, long, "medical")
	if err != nil || names(out) != "differential,long,medical" {
		t.Fatalf("insert before: %v %s", err, names(out))
	}
	// Moving an existing rule (unchanged otherwise) is a change.
	out, _, changed, err = SetRoutingRule(data, c.Spec.Routing.Rules[1], "differential")
	if err != nil || !changed || names(out) != "medical,differential" {
		t.Fatalf("move: %v %v %s", changed, err, names(out))
	}
	// Already right before: no change.
	if _, _, changed, _ := SetRoutingRule(data, c.Spec.Routing.Rules[0], "medical"); changed {
		t.Fatal("a rule already before its anchor was rewritten")
	}
	if _, _, _, err := SetRoutingRule(data, long, "nope"); err == nil {
		t.Fatal("unknown anchor accepted")
	}
	if _, _, _, err := SetRoutingRule(data, long, "long"); err == nil {
		t.Fatal("a rule before itself accepted")
	}
	// Changing a rule keeps its place.
	r := c.Spec.Routing.Rules[0]
	r.Target = "medgemma"
	out, old, changed, err := SetRoutingRule(data, r, "")
	if err != nil || !changed || old.Target != "spetaar-ddx" || names(out) != "differential,medical" {
		t.Fatalf("update: %v %v %s", changed, err, names(out))
	}
	if out, err := RemoveRoutingRule(data, "medical"); err != nil || names(out) != "differential" {
		t.Fatalf("remove: %v", err)
	}
}
