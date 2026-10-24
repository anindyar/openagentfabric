package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

const (
	pairExample     = "../../examples/cluster.pair.yaml"
	triangleExample = "../../examples/cluster.triangle.yaml"
	ppsExample      = "../../examples/cluster.pair-plus-single.yaml"
	catalogExample  = "../../examples/catalog.yaml"
)

func mustCluster(t *testing.T, path string) *Cluster {
	t.Helper()
	c, err := LoadCluster(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return c
}

func mustCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := LoadCatalog(catalogExample)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return c
}

func TestExamplesAreValid(t *testing.T) {
	cat := mustCatalog(t)
	for _, path := range []string{pairExample, triangleExample, ppsExample} {
		t.Run(path, func(t *testing.T) {
			r := Validate(mustCluster(t, path), cat, Options{})
			if !r.OK() {
				t.Fatalf("unexpected errors: %v", r.Errors())
			}
		})
	}
}

func TestPairExampleDecodes(t *testing.T) {
	c := mustCluster(t, pairExample)
	if got := c.Spec.Fabric.RoCE.GIDIndex; got != "auto" {
		t.Errorf("gidIndex = %q", got)
	}
	if len(c.Spec.Fabric.Links) != 1 || *c.Spec.Fabric.Links[0].B.Port != 0 {
		t.Errorf("links decoded wrong: %+v", c.Spec.Fabric.Links)
	}
	if c.Spec.Models[1].Placement.Group != "pair-a" {
		t.Errorf("placement decoded wrong: %+v", c.Spec.Models[1].Placement)
	}
}

func TestPairExampleWarnsAboutUnentitledDDx(t *testing.T) {
	r := Validate(mustCluster(t, pairExample), mustCatalog(t), Options{})
	w := r.Warnings()
	if len(w) != 1 || !strings.Contains(w[0].Message, `entitlement "ddx" is not present`) {
		t.Fatalf("warnings = %v", w)
	}
}

// --- schema-level rejections ------------------------------------------------

func TestSchemaRejections(t *testing.T) {
	base, err := os.ReadFile(pairExample)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, old, new, wantPath string
	}{
		{"missing gpuMemoryUtilization", "      gpuMemoryUtilization: 0.30\n", "", "spec.models[0]"},
		{"gpuMemoryUtilization too high", "gpuMemoryUtilization: 0.30", "gpuMemoryUtilization: 0.98", "spec.models[0].gpuMemoryUtilization"},
		{"served name auto", "    - name: fast\n", "    - name: auto\n", "spec.models[0].name"},
		{"ring topology", "topology: pair", "topology: ring", "spec.fabric.topology"},
		{"unknown field", "  bundle:\n", "  bogus: 1\n  bundle:\n", "spec"},
		{"inline secret", "secretRef: gateway-key-default", "secretRef: gateway-key-default\n        value: sk-123", "spec.routing.keys[0]"},
		{"bad apiVersion", "apiVersion: oaf/v1alpha1", "apiVersion: oaf/v1", "apiVersion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Replace(string(base), tc.old, tc.new, 1)
			if doc == string(base) {
				t.Fatalf("test setup: %q not found in example", tc.old)
			}
			_, err := ParseCluster([]byte(doc))
			var se *SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("want SchemaError, got %v", err)
			}
			for _, i := range se.Issues {
				if i.Path == tc.wantPath {
					return
				}
			}
			t.Fatalf("no issue at %s: %v", tc.wantPath, se)
		})
	}
}

func TestCatalogSchemaRejectsBranchRevision(t *testing.T) {
	base, _ := os.ReadFile(catalogExample)
	doc := strings.Replace(string(base), "revision: 5a5a776300a41aaa681dd7ff0106608ef2bc90db", "revision: main", 1)
	if _, err := ParseCatalog([]byte(doc)); err == nil {
		t.Fatal("a branch name must not be accepted as a revision")
	}
}

func TestYAMLEdgeCases(t *testing.T) {
	if _, err := yamlToJSONValue([]byte("a: 1\na: 2\n")); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("duplicate key: got %v", err)
	}
	if _, err := yamlToJSONValue([]byte("a: .nan\n")); err == nil {
		t.Error("NaN must be rejected")
	}
	v, err := yamlToJSONValue([]byte("d: 2026-11-01\nn: 4\nb: true\nz: ~\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["d"] != "2026-11-01" {
		t.Errorf("timestamp should stay a string, got %#v", m["d"])
	}
	if m["b"] != true || m["z"] != nil {
		t.Errorf("scalars decoded wrong: %#v", m)
	}
	if _, err := yamlToJSONValue([]byte("")); err == nil {
		t.Error("empty document must be rejected")
	}
}
