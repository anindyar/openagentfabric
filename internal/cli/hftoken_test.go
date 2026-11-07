package cli

import (
	"bytes"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/reconcile"
	"openagentfabric.dev/oaf/internal/render"
	"openagentfabric.dev/oaf/internal/secrets"
)

// Gated weights (MedGemma) carry the token to the node; open ones never do.
func TestHFTokenOnlyForGatedWeights(t *testing.T) {
	c, err := config.LoadCluster("../../examples/cluster.hospital.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := config.LoadCatalog("../../examples/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var projects []render.Project
	for _, d := range c.Spec.Models {
		for node, w := range render.ModelWeights(c, cat, d) {
			projects = append(projects, render.Project{Node: node, Name: "model-" + d.Name, Weights: w})
		}
	}
	var out bytes.Buffer
	attachHFToken(projects, "", &out)
	if !strings.Contains(out.String(), "medgemma-27b-it (google/medgemma-27b-it) is gated") || strings.Count(out.String(), "warning") != 1 {
		t.Fatalf("missing-token warning: %q", out.String())
	}
	attachHFToken(projects, "hf_x", &bytes.Buffer{})
	for _, p := range projects {
		spec := reconcile.WeightsSpec(p.Weights)
		if want := p.Weights.ModelID == "medgemma-27b-it"; (spec.GetToken() == "hf_x") != want {
			t.Errorf("%s: token %q", p.Weights.ModelID, spec.GetToken())
		}
	}
}

func TestHFTokenSources(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HF_TOKEN", "")
	if got := hfToken(dir); got != "" {
		t.Fatalf("no token anywhere = %q", got)
	}
	if err := (secrets.Store{Dir: dir}).Set(SecretHFToken, "hf_secret"); err != nil {
		t.Fatal(err)
	}
	if got := hfToken(dir); got != "hf_secret" {
		t.Fatalf("from the secret = %q", got)
	}
	t.Setenv("HF_TOKEN", "hf_env")
	if got := hfToken(dir); got != "hf_env" {
		t.Fatalf("the environment wins: %q", got)
	}
}
