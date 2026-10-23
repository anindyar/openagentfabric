package config

import (
	"os"
	"strings"
	"testing"
)

const singleExample = "../../examples/cluster.single.yaml"

// The single-box example serves bge-m3 next to the chat model; it must stay
// valid and inside the memory budget, overhead included.
func TestSingleExampleServesEmbeddings(t *testing.T) {
	c := mustCluster(t, singleExample)
	cat := mustCatalog(t)
	r := Validate(c, cat, Options{})
	if !r.OK() {
		t.Fatalf("errors: %v", r.Errors())
	}
	if len(r.Warnings()) != 0 {
		t.Fatalf("warnings: %v", r.Warnings())
	}
	m, ok := cat.Lookup("bge-m3")
	if !ok || !m.IsEmbedding() || m.EmbeddingDimensions != 1024 {
		t.Fatalf("bge-m3 catalog entry = %+v", m)
	}
}

func TestEmbeddingModelIsNeverAChatRoute(t *testing.T) {
	for _, field := range []string{"fast", "fallback"} {
		c := mustCluster(t, singleExample)
		if field == "fast" {
			c.Spec.Routing.Defaults.Fast = "bge-m3"
		} else {
			c.Spec.Routing.Defaults.Fallback = "bge-m3"
		}
		r := Validate(c, mustCatalog(t), Options{})
		found := false
		for _, e := range r.Errors() {
			if e.Path == "spec.routing.defaults."+field && strings.Contains(e.Message, "embedding model") {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s = bge-m3 accepted: %v", field, r.Errors())
		}
	}
}

func TestOverheadWarning(t *testing.T) {
	c := mustCluster(t, singleExample)
	c.Spec.Models[0].GPUMemoryUtilization = 0.85 // + 0.03, under 0.90, but not with overhead
	r := Validate(c, mustCatalog(t), Options{})
	if !r.OK() {
		t.Fatalf("errors: %v", r.Errors())
	}
	for _, w := range r.Warnings() {
		if strings.Contains(w.Message, "vLLM overhead") {
			return
		}
	}
	t.Fatalf("no overhead warning: %v", r.Warnings())
}

func TestAddModel(t *testing.T) {
	data, err := os.ReadFile(singleExample)
	if err != nil {
		t.Fatal(err)
	}
	// A cluster.yaml written before the embedding model was a default.
	c := mustCluster(t, singleExample)
	before := strings.Replace(string(data), "    - name: bge-m3\n      catalogRef: bge-m3\n      placement: { node: spark-1 }\n      gpuMemoryUtilization: 0.03\n      maxModelLen: 8192\n", "", 1)
	if before == string(data) {
		t.Fatal("test fixture: could not strip the bge-m3 deployment")
	}
	d := ModelDeployment{Name: "bge-m3", CatalogRef: "bge-m3", Placement: Placement{Node: c.Spec.Nodes[0].Name}, GPUMemoryUtilization: 0.03, MaxModelLen: 8192}
	out, changed, err := AddModel([]byte(before), d)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, err := ParseCluster(out)
	if err != nil {
		t.Fatal(err)
	}
	last := got.Spec.Models[len(got.Spec.Models)-1]
	if last.Name != "bge-m3" || last.GPUMemoryUtilization != 0.03 || last.MaxModelLen != 8192 || last.Placement.Node != "spark-1" {
		t.Fatalf("added = %+v", last)
	}
	if !strings.Contains(string(out), "# Memory on one GB10") {
		t.Fatal("comments were lost")
	}
	if r := Validate(got, mustCatalog(t), Options{}); !r.OK() {
		t.Fatalf("result is invalid: %v", r.Errors())
	}
	again, changed, err := AddModel(out, d)
	if err != nil || changed || string(again) != string(out) {
		t.Fatalf("re-adding: changed=%v err=%v", changed, err)
	}
}
