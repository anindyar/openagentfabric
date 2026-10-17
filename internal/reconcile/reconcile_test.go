package reconcile

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/node"
	"openagentfabric.dev/oaf/internal/render"
)

// fakeDocker remembers which compose projects are up and answers `ps`.
type fakeDocker struct {
	mu      sync.Mutex
	up      map[string]bool
	volumes map[string]bool // projects whose volumes were removed
	calls   []string
}

func (f *fakeDocker) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, hostexec.CmdLine(name, args...))
	project := args[slices.Index(args, "--project-name")+1]
	switch {
	case slices.Contains(args, "up"):
		f.up[project] = true
	case slices.Contains(args, "down"):
		delete(f.up, project)
		if slices.Contains(args, "--volumes") {
			f.volumes[project] = true
		}
	case slices.Contains(args, "ps") && f.up[project]:
		return []byte(`{"Service":"main","State":"running","Health":"healthy","Image":"x"}` + "\n"), nil
	}
	return nil, nil
}

func (f *fakeDocker) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func agents(t *testing.T, names ...string) (Nodes, map[string]*fakeDocker) {
	t.Helper()
	ns, dockers := Nodes{}, map[string]*fakeDocker{}
	for _, n := range names {
		d := &fakeDocker{up: map[string]bool{}, volumes: map[string]bool{}}
		// node.Server implements the same interface as the generated client.
		ns[n] = &node.Server{StateDir: t.TempDir(), Compose: node.Compose{Runner: d}}
		dockers[n] = d
	}
	return ns, dockers
}

func proj(nodeName, name, compose string) render.Project {
	return render.Project{Node: nodeName, Name: name, Files: []render.File{{Path: "compose.yaml", Mode: 0o644, Content: []byte(compose)}}}
}

func steps(p Plan) string {
	var s []string
	for _, x := range p {
		s = append(s, string(x.Action)+" "+x.Node+"/"+x.Project)
	}
	return strings.Join(s, "; ")
}

func TestPlanApplyConverge(t *testing.T) {
	ctx := context.Background()
	ns, dockers := agents(t, "a", "b")
	desired := []render.Project{
		proj("a", render.ProjectGateway, "gw\n"),
		proj("a", "model-fast", "m\n"),
		proj("a", render.ProjectNodeExporter, "ne\n"),
		proj("b", render.ProjectNodeExporter, "ne\n"),
	}

	plan, err := PlanApply(ctx, ns, desired)
	if err != nil {
		t.Fatal(err)
	}
	// Dependency order: node-exporter, models, gateway last.
	want := "create a/node-exporter; create b/node-exporter; create a/model-fast; create a/gateway"
	if steps(plan) != want {
		t.Fatalf("first plan\n got %s\nwant %s", steps(plan), want)
	}
	if dockers["a"].count(" up ") != 0 {
		t.Fatal("planning must not change anything")
	}
	var out bytes.Buffer
	if err := Apply(ctx, ns, desired, plan, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "created   a/gateway  main=running/healthy") {
		t.Fatalf("progress output:\n%s", out.String())
	}

	// Re-running is a no-op plan; applying it only converges.
	plan, err = PlanApply(ctx, ns, desired)
	if err != nil || plan.Changes() != 0 {
		t.Fatalf("second plan should have no changes: %s %v", steps(plan), err)
	}

	// Change one project, drop another: one update, one removal (first).
	desired[0] = proj("a", render.ProjectGateway, "gw v2\n")
	desired = slices.Delete(desired, 1, 2)
	plan, err = PlanApply(ctx, ns, desired)
	if err != nil {
		t.Fatal(err)
	}
	want = "remove a/model-fast; unchanged a/node-exporter; unchanged b/node-exporter; update a/gateway"
	if steps(plan) != want {
		t.Fatalf("third plan\n got %s\nwant %s", steps(plan), want)
	}
	if err := Apply(ctx, ns, desired, plan, &out); err != nil {
		t.Fatal(err)
	}
	if dockers["a"].up["oaf-model-fast"] || dockers["a"].volumes["oaf-model-fast"] {
		t.Fatal("removed project should be down, with its volumes kept")
	}

	st := Status(ctx, ns, desired)
	var got []string
	for _, s := range st {
		got = append(got, s.Node+"/"+s.Project)
		if !s.Healthy() {
			t.Errorf("%s/%s not healthy: %+v", s.Node, s.Project, s)
		}
	}
	if strings.Join(got, " ") != "a/node-exporter a/gateway b/node-exporter" {
		t.Fatalf("status order: %v", got)
	}
}

func TestStatusReportsMissingAndExtraProjects(t *testing.T) {
	ctx := context.Background()
	ns, _ := agents(t, "a")
	if err := Apply(ctx, ns, []render.Project{proj("a", "model-old", "x\n")}, Plan{{Node: "a", Project: "model-old", Action: Create}}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	st := Status(ctx, ns, []render.Project{proj("a", render.ProjectGateway, "gw\n")})
	if len(st) != 2 || st[0].Project != "model-old" || !st[0].Healthy() || st[1].Project != "gateway" || st[1].Exists || st[1].Healthy() {
		t.Fatalf("status: %+v", st)
	}
}

func TestUninstall(t *testing.T) {
	ctx := context.Background()
	ns, dockers := agents(t, "a")
	desired := []render.Project{proj("a", render.ProjectGateway, "gw\n"), proj("a", render.ProjectLangfuse, "lf\n")}
	plan, _ := PlanApply(ctx, ns, desired)
	if err := Apply(ctx, ns, desired, plan, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Uninstall(ctx, ns, true, &out); err != nil {
		t.Fatal(err)
	}
	// Gateway stops taking traffic before its dependencies go.
	if out.String() != "removed   a/gateway\nremoved   a/langfuse\n" {
		t.Fatalf("uninstall order:\n%s", out.String())
	}
	if len(dockers["a"].up) != 0 || !dockers["a"].volumes["oaf-langfuse"] {
		t.Fatalf("after purge: up=%v volumes=%v", dockers["a"].up, dockers["a"].volumes)
	}
	srv := ns["a"].(*node.Server)
	if entries, _ := os.ReadDir(filepath.Join(srv.StateDir, "projects")); len(entries) != 0 {
		t.Fatalf("project files left behind: %v", entries)
	}
}

func TestPlanApplyUnknownNode(t *testing.T) {
	ns, _ := agents(t, "a")
	if _, err := PlanApply(context.Background(), ns, []render.Project{proj("zz", "gateway", "x")}); err == nil {
		t.Fatal("a project for a node without an agent must be an error")
	}
}
