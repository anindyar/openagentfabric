package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/node"
	"openagentfabric.dev/oaf/internal/preflight"
	"openagentfabric.dev/oaf/internal/reconcile"
)

// fakeDocker tracks which compose projects are up.
type fakeDocker struct {
	mu sync.Mutex
	up map[string]bool
	n  int // mutating calls
}

func (f *fakeDocker) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	project := args[slices.Index(args, "--project-name")+1]
	switch {
	case slices.Contains(args, "up"):
		f.up[project] = true
		f.n++
	case slices.Contains(args, "down"):
		delete(f.up, project)
		f.n++
	case slices.Contains(args, "ps") && f.up[project]:
		return []byte(`{"Service":"main","State":"running","Health":"healthy"}`), nil
	}
	return nil, nil
}

func (f *fakeDocker) mutations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// fakeCluster swaps the agent dialer for in-process agents backed by fake
// docker; fabric discovery replays the capture from a real DGX Spark.
func fakeCluster(t *testing.T) *fakeDocker {
	t.Helper()
	docker := &fakeDocker{up: map[string]bool{}}
	replay, err := hostexec.LoadReplay("../preflight/testdata/threeofnine.json")
	if err != nil {
		t.Fatal(err)
	}
	agents := reconcile.Nodes{}
	for _, n := range []string{"spark-1", "spark-2"} {
		agents[n] = &node.Server{StateDir: t.TempDir(), Compose: node.Compose{Runner: docker}, Inspect: replay, PreflightOptions: preflight.Defaults}
	}
	orig := dialNodes
	dialNodes = func(*config.Cluster, string) (reconcile.Nodes, error) { return agents, nil }
	t.Cleanup(func() { dialNodes = orig })
	return docker
}

func TestApplyStatusUninstall(t *testing.T) {
	docker := fakeCluster(t)
	secretsDir := filepath.Join(t.TempDir(), "secrets")
	args := func(cmd string, extra ...string) []string {
		return append([]string{cmd, "-f", "../../examples/cluster.pair.yaml", "--catalog", "../../examples/catalog.yaml", "--secrets-dir", secretsDir}, extra...)
	}

	out, err := runCLI(t, args("apply", "--dry-run")...)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{"create    spark-1/node-exporter", "create    spark-2/model-think", "create    spark-1/gateway", "8 change(s), 0 unchanged"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if docker.mutations() != 0 {
		t.Fatal("dry run ran docker")
	}
	if _, err := os.Stat(secretsDir); !os.IsNotExist(err) {
		t.Fatal("dry run generated secrets")
	}

	out, err = runCLI(t, args("apply")...)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "created   spark-1/langfuse  main=running/healthy") {
		t.Fatalf("apply output:\n%s", out)
	}
	if entries, _ := os.ReadDir(secretsDir); len(entries) < 10 {
		t.Fatalf("apply should generate the stack secrets, found %d", len(entries))
	}

	out, err = runCLI(t, args("apply", "--dry-run")...)
	if err != nil || !strings.Contains(out, "0 change(s), 8 unchanged") {
		t.Fatalf("re-apply should be a no-op: %v\n%s", err, out)
	}

	out, err = runCLI(t, args("status")...)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "spark-2  model-think    healthy  main=running/healthy") {
		t.Fatalf("status output:\n%s", out)
	}

	if out, err = runCLI(t, args("uninstall")...); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("uninstall without --yes must refuse: %v\n%s", err, out)
	}
	out, err = runCLI(t, args("uninstall", "--yes")...)
	if err != nil || strings.Count(out, "removed") != 8 {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}

	out, err = runCLI(t, args("status")...)
	if err == nil || !strings.Contains(out, "missing") {
		t.Fatalf("status after uninstall should report missing projects and fail: %v\n%s", err, out)
	}
}

func TestInstallStopsOnPreflightFailure(t *testing.T) {
	docker := fakeCluster(t)
	dir := t.TempDir()
	// A data path that does not exist fails the disk check on the replayed node.
	src, _ := os.ReadFile("../../examples/cluster.pair.yaml")
	cl := strings.Replace(string(src), "roles: [inference]\n", "roles: [inference]\n      storage: { dataPath: /nonexistent }\n", 1)
	path := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(path, []byte(cl), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "install", "-f", path, "--catalog", "../../examples/catalog.yaml", "--secrets-dir", filepath.Join(dir, "s"))
	if err == nil || !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("install should stop on preflight failure: %v\n%s", err, out)
	}
	if docker.mutations() != 0 {
		t.Fatal("install deployed despite a failed preflight")
	}
}

func TestInstallStopsOnFabricFailure(t *testing.T) {
	docker := fakeCluster(t)
	args := []string{"install", "-f", "../../examples/cluster.pair.yaml", "--catalog", "../../examples/catalog.yaml", "--secrets-dir", filepath.Join(t.TempDir(), "s")}
	// The fake agents cannot run ib_write_bw, so the bandwidth test fails.
	out, err := runCLI(t, args...)
	if err == nil || !strings.Contains(err.Error(), "fabric check failed") || !strings.Contains(out, "FAIL  spark-1:0 ↔ spark-2:0") {
		t.Fatalf("install should stop on a failed fabric check: %v\n%s", err, out)
	}
	if docker.mutations() != 0 {
		t.Fatal("install deployed despite a failed fabric check")
	}
	if out, err = runCLI(t, append(args, "--force")...); err != nil || docker.mutations() == 0 {
		t.Fatalf("--force should deploy anyway: %v\n%s", err, out)
	}
}
