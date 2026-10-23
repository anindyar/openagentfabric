package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
)

// A cluster.yaml written before the embedding model was a default gets it
// with one idempotent command, and `keys issue` then tells apps about it.
func TestModelsAddAndEmbeddingEnv(t *testing.T) {
	data, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(string(data), "    - name: bge-m3\n      catalogRef: bge-m3\n      placement: { node: spark-1 }\n      gpuMemoryUtilization: 0.03\n      maxModelLen: 8192\n", "", 1)
	cluster := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(cluster, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-f", cluster, "--catalog", "../../examples/catalog.yaml"}

	out, err := runCLI(t, append([]string{"models", "add", "bge-m3", "--catalog-ref", "bge-m3"}, args...)...)
	if err != nil || !strings.Contains(out, `added "bge-m3"`) {
		t.Fatalf("models add: %v\n%s", err, out)
	}
	c, err := config.LoadCluster(cluster)
	if err != nil {
		t.Fatal(err)
	}
	d := c.Spec.Models[len(c.Spec.Models)-1]
	if d.CatalogRef != "bge-m3" || d.GPUMemoryUtilization != 0.03 || d.MaxModelLen != 8192 || d.Placement.Node != "spark-1" {
		t.Fatalf("added deployment = %+v (want the catalog profile's defaults)", d)
	}
	out, err = runCLI(t, append([]string{"models", "add", "bge-m3", "--catalog-ref", "bge-m3"}, args...)...)
	if err != nil || !strings.Contains(out, "already deploys") {
		t.Fatalf("second add must be a no-op: %v\n%s", err, out)
	}
	out, err = runCLI(t, append([]string{"models", "add", "big", "--catalog-ref", "bge-m3", "--util", "0.5"}, args...)...)
	if err == nil {
		t.Fatalf("an over-budget model was added:\n%s", out)
	}

	cat, _ := config.LoadCatalog("../../examples/catalog.yaml")
	name, dims := embeddingModel(c, cat)
	if name != "bge-m3" || dims != 1024 {
		t.Fatalf("embeddingModel = %q, %d", name, dims)
	}
	if w := smallestContextWindow(c, cat); w != 262144 {
		t.Fatalf("context window = %d; the embedding model's 8192 must not count", w)
	}
}
