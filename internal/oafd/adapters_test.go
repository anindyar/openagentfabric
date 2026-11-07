package oafd

import (
	"os"
	"strings"
	"testing"
)

// GET /models lists each deployment's LoRA adapters. No entitlement is
// verified yet, so an entitled adapter shows as not served.
func TestListModelsShowsAdapters(t *testing.T) {
	f := newFixture(t)
	data, err := os.ReadFile("../../examples/cluster.hospital.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(string(data), "          entitlement: ddx\n",
		"          entitlement: ddx\n        - name: triage\n          path: /var/lib/oaf/adapters/triage\n          rank: 8\n", 1)
	if err := os.WriteFile(f.cluster, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	_, list := f.do(t, "GET", "/api/v1/models", nil, token)
	models, _ := list["models"].([]any)
	got := map[string][]any{}
	for _, m := range models {
		mm := m.(map[string]any)
		got[mm["name"].(string)], _ = mm["adapters"].([]any)
	}
	if a := got["medgemma"]; a == nil || len(a) != 0 {
		t.Errorf("a deployment without adapters lists []: %v", a)
	}
	a := got["gpt-oss-20b"]
	if len(a) != 2 {
		t.Fatalf("adapters = %v", a)
	}
	ddx, triage := a[0].(map[string]any), a[1].(map[string]any)
	if ddx["name"] != "spetaar-ddx" || ddx["entitlement"] != "ddx" || ddx["served"] != false || ddx["rank"] != float64(16) ||
		ddx["path"] != "/var/lib/oaf/adapters/spetaar-ddx" {
		t.Errorf("spetaar-ddx = %v", ddx)
	}
	if triage["name"] != "triage" || triage["served"] != true {
		t.Errorf("triage = %v", triage)
	}
}

// Adapters are trained for one base model: replacing the base drops them
// (and the plan shows it); a new placement of the same model keeps them.
func TestReplaceDropsAdapters(t *testing.T) {
	f := newFixture(t)
	data, err := os.ReadFile("../../examples/cluster.hospital.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cluster, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, plan := f.do(t, "POST", "/api/v1/models/gpt-oss-20b/replace", map[string]any{"catalog_ref": "qwen3-30b-a3b-instruct-2507-fp8"}, token)
	diff, _ := plan["diff"].(string)
	if !strings.Contains(diff, "-        - name: spetaar-ddx") && !strings.Contains(diff, "-      adapters:") {
		t.Fatalf("replacing the base keeps its adapters:\n%s", diff)
	}
	_, plan = f.do(t, "POST", "/api/v1/models/gpt-oss-20b/replace", map[string]any{"catalog_ref": "gpt-oss-20b", "max_model_len": 4096}, token)
	if diff, _ := plan["diff"].(string); strings.Contains(diff, "-        - name: spetaar-ddx") || !strings.Contains(diff, "+      maxModelLen: 4096") {
		t.Fatalf("same base, adapters dropped:\n%s", diff)
	}
}
