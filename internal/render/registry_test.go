package render

import (
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
)

// A local component renders with the image cluster.yaml pins; without a pin
// the render says how to push it; the registry runs on the control node only
// when its certificate is given.
func TestRegistryAndLocalImages(t *testing.T) {
	in := pairInput(t)
	c, err := config.LoadCluster("../../examples/cluster.pair-plus-single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	in.Cluster = c
	in.Fabric = map[string][]fabric.Port{
		"spark-1": {},
		"spark-2": {rails(1, "10.200.0.1/30", "10.200.0.5/30", 3)},
		"spark-3": {rails(1, "10.200.0.2/30", "10.200.0.6/30", 3)},
	}
	pinned := c.Spec.Images["vllm-glm53"]
	glm, registry := false, false
	for _, p := range mustRender(t, in) {
		if p.Name == ProjectRegistry {
			registry = true
		}
		if p.Name == modelProjectName("glm-5.3-flash") && strings.Contains(string(p.Files[0].Content), "image: "+pinned) {
			glm = true
		}
	}
	if !glm || registry {
		t.Fatalf("pinned image used: %v; registry without a certificate: %v", glm, registry)
	}

	in.Registry = &RegistryTLS{Cert: []byte("cert"), Key: []byte("key")}
	for _, p := range mustRender(t, in) {
		if p.Name != ProjectRegistry {
			continue
		}
		if p.Node != "spark-1" {
			t.Fatalf("registry on %s, not the control node", p.Node)
		}
		var key *File
		for i := range p.Files {
			if p.Files[i].Path == "registry.key" {
				key = &p.Files[i]
			}
		}
		if key == nil || key.Mode != 0o600 || !strings.Contains(string(p.Files[0].Content), "REGISTRY_HTTP_TLS_KEY") {
			t.Fatalf("registry project: %+v", p.Files)
		}
		registry = true
	}
	if !registry {
		t.Fatal("no registry with a certificate")
	}

	// Not pushed yet: that model is left out, the rest still renders.
	delete(c.Spec.Images, "vllm-glm53")
	if got := UnpinnedLocal(c, in.Catalog, in.Versions); got["glm-5.3-flash"] != "vllm-glm53" {
		t.Fatalf("unpinned: %v", got)
	}
	for _, p := range mustRender(t, in) {
		if p.Name == modelProjectName("glm-5.3-flash") {
			t.Fatal("a model without its image was rendered")
		}
	}
	c.Spec.Images["no-such"] = pinned
	if _, err := Render(in); err == nil || !strings.Contains(err.Error(), `pins "no-such"`) {
		t.Fatalf("pin of an unknown component: %v", err)
	}
}
