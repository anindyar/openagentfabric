package oafd

import (
	"os"
	"testing"
)

// Companion models are always offered, and recommended once a pair runs a
// think-class model.
func TestCatalogCompanions(t *testing.T) {
	f := newFixture(t)
	companion := func() map[string]any {
		t.Helper()
		_, cat := f.do(t, "GET", "/api/v1/catalog", nil, token)
		for _, m := range cat["models"].([]any) {
			if v := m.(map[string]any); v["id"] == "ornith-1.5-35b-a3b-nvfp4" {
				return v
			}
		}
		t.Fatal("companion model missing from the catalog")
		return nil
	}
	if v := companion(); v["offered"] != true || v["recommended"] == true {
		t.Fatalf("single box: %v", v)
	}
	data, err := os.ReadFile("../../examples/cluster.pair-plus-single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cluster, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if v := companion(); v["offered"] != true || v["recommended"] != true || v["note"] == "" {
		t.Fatalf("beside a pair: %v", v)
	}
}
