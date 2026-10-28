package config

import (
	"os"
	"strings"
	"testing"
)

func singleDoc(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validateDoc(t *testing.T, data []byte) Report {
	t.Helper()
	c, err := ParseCluster(data)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := LoadCatalog("../../examples/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return Validate(c, cat, Options{})
}

func hasIssue(issues []Issue, path, text string) bool {
	for _, i := range issues {
		if strings.HasPrefix(i.Path, path) && strings.Contains(i.Message, text) {
			return true
		}
	}
	return false
}

// A provider without guardrails is allowed, with the exact warning; one
// without allowEgress, a key or an endpoint is not.
func TestProviderValidation(t *testing.T) {
	data := singleDoc(t)
	out, changed, err := SetProvider(data, Provider{Name: "openai", Type: ProviderOpenAI, SecretRef: "provider-openai", AllowEgress: true,
		Models: []ProviderModel{{Name: "gpt-4o"}}})
	if err != nil || !changed {
		t.Fatalf("SetProvider: %v %v", changed, err)
	}
	r := validateDoc(t, out)
	if !r.OK() {
		t.Fatalf("errors: %v", r.Errors())
	}
	if !hasIssue(r.Warnings(), "spec.routing.providers[0].guardrails", NoGuardrailWarning) {
		t.Fatalf("no guardrail warning: %v", r.Warnings())
	}

	// Adding the basic policy and attaching it clears the warning.
	out, _, err = SetGuardrail(out, BasicGuardrail())
	if err != nil {
		t.Fatal(err)
	}
	out, changed, err = SetProvider(out, Provider{Name: "openai", Type: ProviderOpenAI, SecretRef: "provider-openai", AllowEgress: true,
		Guardrails: []string{"basic"}, Models: []ProviderModel{{Name: "gpt-4o"}}})
	if err != nil || !changed {
		t.Fatalf("replace provider: %v %v", changed, err)
	}
	if r := validateDoc(t, out); !r.OK() || hasIssue(r.Warnings(), "spec.routing.providers", NoGuardrailWarning) {
		t.Fatalf("with basic: %v %v", r.Errors(), r.Warnings())
	}
	if _, changed, _ := SetProvider(out, Provider{Name: "openai", Type: ProviderOpenAI, SecretRef: "provider-openai", AllowEgress: true,
		Guardrails: []string{"basic"}, Models: []ProviderModel{{Name: "gpt-4o"}}}); changed {
		t.Fatal("setting the same provider again changed the document")
	}

	bad := []struct {
		p          Provider
		path, text string
	}{
		{Provider{Name: "a", Type: ProviderOpenAI, SecretRef: "k", Models: []ProviderModel{{Name: "m1"}}}, ".allowEgress", "allowEgress"},
		{Provider{Name: "b", Type: ProviderAnthropic, AllowEgress: true, Models: []ProviderModel{{Name: "m2"}}}, ".secretRef", "API key"},
		{Provider{Name: "c", Type: ProviderOpenAICompatible, AllowEgress: true, Models: []ProviderModel{{Name: "m3"}}}, ".endpoint", "endpoint"},
		{Provider{Name: "d", Type: ProviderAzureOpenAI, SecretRef: "k", AllowEgress: true, Models: []ProviderModel{{Name: "m4"}}}, ".azure", "resourceName"},
		{Provider{Name: "e", Type: ProviderOpenAI, SecretRef: "k", AllowEgress: true, Guardrails: []string{"nope"}, Models: []ProviderModel{{Name: "m5"}}}, ".guardrails[0]", "no guardrail policy"},
		{Provider{Name: "f", Type: ProviderOpenAI, SecretRef: "k", AllowEgress: true, Models: []ProviderModel{{Name: "ornith"}}}, ".models[0].name", "already used"},
	}
	for _, b := range bad {
		doc, _, err := SetProvider(singleDoc(t), b.p)
		if err != nil {
			t.Fatalf("%s: %v", b.p.Name, err)
		}
		if r := validateDoc(t, doc); !hasIssue(r.Errors(), "spec.routing.providers[0]"+b.path, b.text) {
			t.Errorf("%s: want error at %s containing %q, got %v", b.p.Name, b.path, b.text, r.Errors())
		}
	}

	out, err = RemoveProvider(out, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := ParseCluster(out); len(c.Spec.Routing.Providers) != 0 {
		t.Fatal("provider not removed")
	}
	if _, err := RemoveProvider(out, "openai"); err == nil {
		t.Fatal("removing a missing provider must fail")
	}
}

func TestGuardrailValidation(t *testing.T) {
	cases := []struct {
		g          GuardrailPolicy
		path, text string
	}{
		{GuardrailPolicy{Name: "x", Request: []Guard{{Regex: &RegexGuard{Action: "reject", Patterns: []string{"("}}}}}, ".request[0].regex.patterns[0]", "invalid regular expression"},
		{GuardrailPolicy{Name: "x", Request: []Guard{{}}}, ".request[0]", "exactly one"},
		{GuardrailPolicy{Name: "x"}, "", "no guards"},
	}
	for _, c := range cases {
		doc, _, err := SetGuardrail(singleDoc(t), c.g)
		if err != nil {
			t.Fatal(err)
		}
		if r := validateDoc(t, doc); !hasIssue(r.Errors(), "spec.routing.guardrails[0]"+c.path, c.text) {
			t.Errorf("want %q at %s, got %v", c.text, c.path, r.Errors())
		}
	}
	// A detector the pinned agentgateway lacks is refused when the document is
	// edited (schema), before any plan.
	if _, _, err := SetGuardrail(singleDoc(t), GuardrailPolicy{Name: "x", Request: []Guard{{Regex: &RegexGuard{Action: "reject", Builtins: []string{"passport"}}}}}); err == nil || !strings.Contains(err.Error(), "creditCard") {
		t.Fatalf("unknown detector accepted: %v", err)
	}
	doc, _, _ := SetGuardrail(singleDoc(t), BasicGuardrail())
	if r := validateDoc(t, doc); !r.OK() {
		t.Fatalf("basic does not validate: %v", r.Errors())
	}
	doc, changed, err := SetModelGuardrails(doc, "ornith", []string{"basic"})
	if err != nil || !changed {
		t.Fatalf("SetModelGuardrails: %v %v", changed, err)
	}
	if c, _ := ParseCluster(doc); len(c.Spec.Models) == 0 || c.Spec.Models[0].Guardrails[0] != "basic" {
		t.Fatal("model guardrails not set")
	}
	if _, err := RemoveGuardrail(doc, "basic"); err != nil {
		t.Fatal(err)
	}
}

func TestSetRoutingAndRemoveModel(t *testing.T) {
	data := singleDoc(t)
	mode := "active"
	out, changed, err := SetRouting(data, RoutingUpdate{Mode: &mode})
	if err != nil || !changed {
		t.Fatalf("SetRouting: %v %v", changed, err)
	}
	if c, _ := ParseCluster(out); c.Spec.Routing.Mode != "active" {
		t.Fatal("mode not set")
	}
	if _, changed, _ := SetRouting(out, RoutingUpdate{Mode: &mode}); changed {
		t.Fatal("same mode changed the document")
	}
	if !strings.Contains(string(out), "#") {
		t.Fatal("comments were lost")
	}
	out, err = RemoveModel(out, "bge-m3")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := ParseCluster(out)
	for _, d := range c.Spec.Models {
		if d.Name == "bge-m3" {
			t.Fatal("model not removed")
		}
	}
}
