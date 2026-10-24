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

// Adding a model starts its download at once, before anyone approves the
// plan; a plan that cannot apply starts nothing.
func TestAddModelStartsDownload(t *testing.T) {
	f := newFixture(t)
	// The vllm image's terms are accepted, so the plan can apply.
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

	add := map[string]any{"name": "coder", "catalog_ref": "qwen3-coder-30b-a3b-instruct-fp8", "gpu_memory_utilization": 0.05}
	code, plan := f.do(t, "POST", "/api/v1/models", add, token)
	if code != 201 {
		t.Fatalf("plan: %d %v", code, plan)
	}
	select {
	case d := <-got:
		if d.Name != "coder" || d.CatalogRef != "qwen3-coder-30b-a3b-instruct-fp8" || d.Placement.Node == "" {
			t.Fatalf("prefetched %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("adding a model did not start its download")
	}

	// Without the licence its image needs, the plan is invalid: no download.
	cut := strings.Index(accepted, "  licenseAcceptances:")
	if err := os.WriteFile(f.cluster, []byte(accepted[:cut]), 0o644); err != nil {
		t.Fatal(err)
	}
	add["name"] = "coder2"
	if code, _ := f.do(t, "POST", "/api/v1/models", add, token); code != 422 {
		t.Fatalf("plan without licences: %d", code)
	}
	select {
	case d := <-got:
		t.Fatalf("an invalid plan started a download: %+v", d)
	case <-time.After(200 * time.Millisecond):
	}
}
