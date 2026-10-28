package oafd

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"openagentfabric.dev/oaf/bundle"
	"openagentfabric.dev/oaf/internal/config"
)

// A box's model can be swapped for one that only fits once the first is
// gone; keeping the name changes nothing else, a new name moves routing.
func TestReplaceModel(t *testing.T) {
	f := newFixture(t)
	raw, _ := os.ReadFile(f.cluster)
	accepted := string(raw) + "    - subject: image/vllm\n      url: https://docs.nvidia.com/cuda/eula/index.html\n" +
		"      acceptedBy: admin@example.com\n      acceptedAt: 2026-10-24T12:00:00Z\n"
	if err := os.WriteFile(f.cluster, []byte(accepted), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := config.ParseVersions(bundle.VersionsYAML)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan config.ModelDeployment, 4)
	s := New(Config{ClusterPath: f.cluster, CatalogPath: "../../examples/catalog.yaml", Secrets: f.store, StateDir: t.TempDir(),
		Versions: v,
		Apply:    func(context.Context, io.Writer) error { return nil },
		Prefetch: func(_ context.Context, _ *config.Cluster, d config.ModelDeployment) { got <- d },
	})
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)

	// Added beside ornith (0.40) and bge-m3, the 122B (0.80) does not fit.
	if code, _ := f.do(t, "POST", "/api/v1/models", map[string]any{"name": "big", "catalog_ref": "ornith-1.5-122b-nvfp4"}, token); code != 422 {
		t.Fatalf("adding beside: %d", code)
	}
	if code, _ := f.do(t, "POST", "/api/v1/models/nope/replace", map[string]any{"catalog_ref": "ornith-1.5-122b-nvfp4"}, token); code != 404 {
		t.Fatalf("unknown deployment: %d", code)
	}
	// In its place it does; the name, and so routing, stay.
	code, plan := f.do(t, "POST", "/api/v1/models/ornith/replace", map[string]any{"catalog_ref": "ornith-1.5-122b-nvfp4"}, token)
	diff, _ := plan["diff"].(string)
	if code != 201 || !strings.Contains(diff, "+      catalogRef: ornith-1.5-122b-nvfp4") || strings.Contains(diff, "+      fast:") {
		t.Fatalf("replace keeping the name: %d %v", code, plan)
	}
	select {
	case d := <-got:
		if d.Name != "ornith" || d.CatalogRef != "ornith-1.5-122b-nvfp4" || d.GPUMemoryUtilization != 0.8 {
			t.Fatalf("prefetched %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacing did not start the download")
	}
	// Under a new name the old deployment goes and routing follows.
	code, plan = f.do(t, "POST", "/api/v1/models/ornith/replace", map[string]any{"catalog_ref": "ornith-1.5-122b-nvfp4", "name": "ornith-big"}, token)
	diff, _ = plan["diff"].(string)
	if code != 201 || !strings.Contains(diff, "+      fast: ornith-big") || !strings.Contains(diff, "-    - name: ornith") {
		t.Fatalf("replace with a new name: %d %v", code, plan)
	}
	if code, _ := f.do(t, "POST", "/api/v1/models/ornith/replace", map[string]any{"catalog_ref": "ornith-1.5-122b-nvfp4", "name": "bge-m3"}, token); code != 409 {
		t.Fatalf("new name taken: %d", code)
	}
}
