package node

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect/v2"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/gen/oaf/node/v1/nodev1connect"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/pki"
	"openagentfabric.dev/oaf/internal/preflight"
)

// fakeDocker records docker invocations and answers `compose ps`.
type fakeDocker struct {
	mu    sync.Mutex
	calls []string
	ps    string
}

func (f *fakeDocker) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := hostexec.CmdLine(name, args...)
	f.calls = append(f.calls, line)
	if strings.Contains(line, " ps ") {
		return []byte(f.ps), nil
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

// newAgent starts an agent behind real mTLS and returns an oafd-side client.
func newAgent(t *testing.T) (nodev1connect.NodeServiceClient, *fakeDocker, string) {
	t.Helper()
	ca, err := pki.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	nodeCert, _ := pki.Issue(ca, pki.LeafOptions{CommonName: "spark-1", DNSNames: []string{"spark-1"}, Server: true})
	oafdCert, _ := pki.Issue(ca, pki.LeafOptions{CommonName: pki.ControlPlaneCN, Client: true})
	srvTLS, err := pki.ServerTLS(ca.CertPEM, nodeCert, pki.ControlPlaneCN)
	if err != nil {
		t.Fatal(err)
	}
	cliTLS, err := pki.ClientTLS(ca.CertPEM, oafdCert, "spark-1")
	if err != nil {
		t.Fatal(err)
	}

	docker := &fakeDocker{ps: `{"Service":"web","State":"running","Health":"healthy","Image":"busybox"}` + "\n"}
	replay, err := hostexec.LoadReplay("../preflight/testdata/threeofnine.json")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	srv := &Server{StateDir: state, Compose: Compose{Runner: docker}, Inspect: replay, PreflightOptions: preflight.Defaults}

	hs := httptest.NewUnstartedServer(Handler(srv))
	hs.TLS = srvTLS
	hs.EnableHTTP2 = true
	hs.StartTLS()
	t.Cleanup(hs.Close)
	return NewClient(strings.TrimPrefix(hs.URL, "https://"), cliTLS), docker, state
}

func project(compose string, extra ...*nodev1.File) *nodev1.ApplyProjectRequest {
	return &nodev1.ApplyProjectRequest{Name: "demo", Files: append([]*nodev1.File{{Path: "compose.yaml", Content: []byte(compose)}}, extra...)}
}

func TestApplyIsIdempotent(t *testing.T) {
	c, docker, state := newAgent(t)
	ctx := context.Background()
	req := project("services: {web: {image: busybox}}\n", &nodev1.File{Path: "conf/app.env", Content: []byte("A=1\n"), Mode: 0o600})

	first, err := c.ApplyProject(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !first.GetChanged() || len(first.GetServices()) != 1 || first.GetServices()[0].GetHealth() != "healthy" {
		t.Fatalf("first apply = %v", first)
	}
	info, err := os.Stat(filepath.Join(state, "projects/demo/conf/app.env"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file not written with mode 0600: %v %v", info, err)
	}

	second, err := c.ApplyProject(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.GetChanged() {
		t.Fatal("re-applying identical files must report changed=false")
	}
	if n := docker.count("--project-name oaf-demo"); n < 2 {
		t.Fatalf("compose should run on every apply (converge), ran %d times", n)
	}

	// A changed file replaces the tree, and removed files disappear.
	third, err := c.ApplyProject(ctx, project("services: {web: {image: busybox:1}}\n"))
	if err != nil || !third.GetChanged() {
		t.Fatalf("third apply: %v %v", third, err)
	}
	if _, err := os.Stat(filepath.Join(state, "projects/demo/conf/app.env")); !os.IsNotExist(err) {
		t.Fatal("files dropped from the project must be removed")
	}
}

func TestDryRunReportsWithoutWriting(t *testing.T) {
	c, docker, state := newAgent(t)
	ctx := context.Background()
	req := project("services: {web: {image: busybox}}\n", &nodev1.File{Path: ".env", Content: []byte("A=1\n"), Mode: 0o600})
	req.DryRun = true
	dry, err := c.ApplyProject(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.GetChanged() || strings.Join(dry.GetChangedPaths(), ",") != ".env,compose.yaml" {
		t.Fatalf("dry run of a new project: %v", dry)
	}
	if _, err := os.Stat(filepath.Join(state, "projects/demo")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote files")
	}
	if docker.count(" up ") != 0 {
		t.Fatal("dry run ran compose up")
	}

	req.DryRun = false
	if _, err := c.ApplyProject(ctx, req); err != nil {
		t.Fatal(err)
	}
	// Same content, different mode: only that path is reported.
	req.DryRun = true
	req.Files[1].Mode = 0o644
	dry, err = c.ApplyProject(ctx, req)
	if err != nil || strings.Join(dry.GetChangedPaths(), ",") != ".env" {
		t.Fatalf("mode change: %v %v", dry, err)
	}
	// A dropped file is reported as changed.
	req.Files = req.Files[:1]
	dry, err = c.ApplyProject(ctx, req)
	if err != nil || strings.Join(dry.GetChangedPaths(), ",") != ".env" {
		t.Fatalf("removed file: %v %v", dry, err)
	}
}

func TestListProjects(t *testing.T) {
	c, _, state := newAgent(t)
	ctx := context.Background()
	empty, err := c.ListProjects(ctx, &nodev1.ListProjectsRequest{})
	if err != nil || len(empty.GetNames()) != 0 {
		t.Fatalf("no projects yet: %v %v", empty, err)
	}
	for _, n := range []string{"zeta", "alpha"} {
		req := project("services: {}\n")
		req.Name = n
		if _, err := c.ApplyProject(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	// Leftover swap directories and stray files are not projects.
	_ = os.MkdirAll(filepath.Join(state, "projects", ".alpha.new-123"), 0o755)
	_ = os.WriteFile(filepath.Join(state, "projects", "README"), []byte("x"), 0o644)
	list, err := c.ListProjects(ctx, &nodev1.ListProjectsRequest{})
	if err != nil || strings.Join(list.GetNames(), ",") != "alpha,zeta" {
		t.Fatalf("list: %v %v", list, err)
	}
}

func TestFabricOverRPC(t *testing.T) {
	c, _, _ := newAgent(t)
	resp, err := c.Fabric(context.Background(), &nodev1.FabricRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ports, err := FabricFromProto(resp.GetPorts())
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 || len(ports[0].Halves) != 2 || ports[0].Halves[0].Name != "enp1s0f0np0" {
		t.Fatalf("ports: %+v", ports)
	}
	// The round trip keeps what the renderer needs: addresses and GIDs.
	h := ports[0].Halves[0]
	if len(h.Addrs) == 0 || h.RDMADev == "" {
		t.Fatalf("lost addresses or RDMA device: %+v", h)
	}
	if idx, ok := h.RoCEv2GIDFor(h.Addrs[0].Addr()); !ok || idx != 3 {
		t.Fatalf("GID for %s = %d, %v", h.Addrs[0], idx, ok)
	}
}

func TestApplyRejectsUnsafeInput(t *testing.T) {
	c, docker, state := newAgent(t)
	ctx := context.Background()
	bad := []*nodev1.ApplyProjectRequest{
		{Name: "../etc", Files: project("x").Files},
		{Name: "Demo", Files: project("x").Files},
		{Name: "demo", Files: []*nodev1.File{{Path: "other.yaml", Content: []byte("x")}}},
		project("x", &nodev1.File{Path: "../../etc/cron.d/evil", Content: []byte("x")}),
		project("x", &nodev1.File{Path: "/etc/passwd", Content: []byte("x")}),
		project("x", &nodev1.File{Path: "a/../../b", Content: []byte("x")}),
		project("x", &nodev1.File{Path: "s", Content: []byte("x"), Mode: 0o4755}),
		project("x", &nodev1.File{Path: "compose.yaml", Content: []byte("dup")}),
	}
	for i, req := range bad {
		_, err := c.ApplyProject(ctx, req)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("case %d: want InvalidArgument, got %v", i, err)
		}
	}
	if len(docker.calls) != 0 {
		t.Fatalf("rejected requests must not reach docker: %v", docker.calls)
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Fatalf("rejected requests must not write files: %v", entries)
	}
}

func TestRemoveProject(t *testing.T) {
	c, docker, state := newAgent(t)
	ctx := context.Background()
	resp, err := c.RemoveProject(ctx, &nodev1.RemoveProjectRequest{Name: "demo"})
	if err != nil || resp.GetExisted() {
		t.Fatalf("removing a missing project: %v %v", resp, err)
	}
	if _, err := c.ApplyProject(ctx, project("services: {}\n")); err != nil {
		t.Fatal(err)
	}
	resp, err = c.RemoveProject(ctx, &nodev1.RemoveProjectRequest{Name: "demo", RemoveVolumes: true})
	if err != nil || !resp.GetExisted() {
		t.Fatalf("remove: %v %v", resp, err)
	}
	if docker.count("down --remove-orphans --volumes") != 1 {
		t.Fatalf("expected one compose down with volumes: %v", docker.calls)
	}
	if _, err := os.Stat(filepath.Join(state, "projects/demo")); !os.IsNotExist(err) {
		t.Fatal("project directory must be removed")
	}
}

func TestInfoStatusAndPreflight(t *testing.T) {
	c, _, _ := newAgent(t)
	ctx := context.Background()
	info, err := c.Info(ctx, &nodev1.InfoRequest{})
	if err != nil || info.GetArch() == "" {
		t.Fatalf("info: %v %v", info, err)
	}
	st, err := c.ProjectStatus(ctx, &nodev1.ProjectStatusRequest{Name: "demo"})
	if err != nil || st.GetExists() {
		t.Fatalf("status of a missing project: %v %v", st, err)
	}
	pf, err := c.Preflight(ctx, &nodev1.PreflightRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if pf.GetHost() != "threeofnine" || pf.GetWorst() != "warn" || len(pf.GetResults()) != 12 {
		t.Fatalf("preflight over RPC: host=%s worst=%s results=%d", pf.GetHost(), pf.GetWorst(), len(pf.GetResults()))
	}
}

func TestParsePS(t *testing.T) {
	arr, err := parsePS([]byte(`[{"Service":"a","State":"running"},{"Service":"b","State":"exited"}]`))
	if err != nil || len(arr) != 2 || arr[1].State != "exited" {
		t.Fatalf("array form: %v %v", arr, err)
	}
	nd, err := parsePS([]byte("{\"Service\":\"a\"}\n{\"Service\":\"b\"}\n"))
	if err != nil || len(nd) != 2 {
		t.Fatalf("ndjson form: %v %v", nd, err)
	}
	if none, err := parsePS([]byte("\n")); err != nil || none != nil {
		t.Fatalf("empty: %v %v", none, err)
	}
}
