package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"openagentfabric.dev/oaf/bundle"
	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

// fakeSecrets derives each secret from its name, so golden files are stable
// and a test can find every secret value in the output.
type fakeSecrets struct{}

func (fakeSecrets) Value(name string) (string, error) { return fakeSecret(name), nil }

func fakeSecret(name string) string {
	s := sha256.Sum256([]byte("test-" + name))
	return hex.EncodeToString(s[:])
}

// rails builds a discovered port whose two halves carry ip0 and ip1, each with
// its RoCE v2 GID at index gid (as seen on a DGX Spark).
func rails(index int, ip0, ip1 string, gid int) fabric.Port {
	half := func(pcie, ip string) fabric.Netdev {
		a := netip.MustParsePrefix(ip)
		v4 := a.Addr().As4()
		return fabric.Netdev{
			Name:      [2]string{"enp1s0f", "enP2p1s0f"}[map[string]int{"0000": 0, "0002": 1}[pcie]] + string(rune('0'+index)) + "np" + string(rune('0'+index)),
			PCI:       pcie + ":01:00." + string(rune('0'+index)),
			OperState: "up", MTU: 9000, SpeedMbps: 200000,
			Addrs:   []netip.Prefix{a},
			RDMADev: map[string]string{"0000": "rocep1s0f", "0002": "roceP2p1s0f"}[pcie] + string(rune('0'+index)),
			GIDs: []fabric.GID{
				{Index: gid - 1, Type: "IB/RoCE v1", GID: "0000:0000:0000:0000:0000:ffff:" + hex.EncodeToString(v4[:2]) + ":" + hex.EncodeToString(v4[2:])},
				{Index: gid, Type: "RoCE v2", GID: "0000:0000:0000:0000:0000:ffff:" + hex.EncodeToString(v4[:2]) + ":" + hex.EncodeToString(v4[2:])},
			},
		}
	}
	return fabric.Port{Index: index, Halves: []fabric.Netdev{half("0000", ip0), half("0002", ip1)}}
}

func pairInput(t *testing.T) Input {
	t.Helper()
	c, err := config.LoadCluster("../../examples/cluster.pair.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := config.LoadCatalog("../../examples/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	v, err := config.ParseVersions(bundle.VersionsYAML)
	if err != nil {
		t.Fatal(err)
	}
	return Input{
		Cluster: c, Catalog: cat, Versions: v, Secrets: fakeSecrets{},
		Fabric: map[string][]fabric.Port{
			"spark-1": {rails(0, "10.10.0.1/30", "10.10.1.1/30", 3), rails(1, "192.168.77.1/24", "192.168.78.1/24", 3)},
			"spark-2": {rails(0, "10.10.0.2/30", "10.10.1.2/30", 3)},
		},
	}
}

func mustRender(t *testing.T, in Input) []Project {
	t.Helper()
	ps, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestGoldenPair(t *testing.T) { checkGolden(t, "pair", mustRender(t, pairInput(t))) }

func TestGoldenSingle(t *testing.T) { checkGolden(t, "single", mustRender(t, singleInput(t))) }

func singleInput(t *testing.T) Input {
	t.Helper()
	in := pairInput(t)
	c, err := config.LoadCluster("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	in.Cluster, in.Fabric = c, nil
	return in
}

// TestSparkEngine checks that a catalog profile's engine and container
// settings reach the model's compose file, and that the image's terms must be
// accepted like the upstream vLLM image's.
func TestSparkEngine(t *testing.T) {
	in := singleInput(t)
	var cf composeFile
	for _, p := range mustRender(t, in) {
		if p.Name == "model-qwen-flash" {
			if err := yaml.Unmarshal(p.Files[0].Content, &cf); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := cf.Services["vllm"]
	if s == nil {
		t.Fatal("no model-qwen-flash project")
	}
	if !strings.HasPrefix(s.Image, "docker.io/eugr/spark-vllm-b12x:latest@sha256:") {
		t.Errorf("image = %s", s.Image)
	}
	if s.IPC != "host" || s.ShmSize != "16g" || s.Ulimits["stack"] != 67108864 || s.Ulimits["memlock"] != -1 {
		t.Errorf("container settings not applied: ipc=%q shm=%q ulimits=%v", s.IPC, s.ShmSize, s.Ulimits)
	}
	if strings.Join(s.SecurityOpt, ",") != "seccomp=unconfined,apparmor=unconfined" {
		t.Errorf("security_opt = %v", s.SecurityOpt)
	}
	if !slices.Contains(s.Volumes, "cache-root-cache-b12x:/root/.cache/b12x") || cf.Volumes["cache-root-triton"] == nil {
		t.Errorf("cache volumes missing: %v / %v", s.Volumes, cf.Volumes)
	}
	if len(s.Command) < 3 || s.Command[0] != "vllm" || s.Command[1] != "serve" || s.Command[2] != "/models/qwen3.8-flash-next-fp4" {
		t.Errorf("command must start with vllm serve <weights>: %v", s.Command[:min(3, len(s.Command))])
	}
	if !slices.Contains(s.Command, "--load-format") || s.Environment["B12X_DISK_BACKEND"] != "io_uring" {
		t.Errorf("profile args/env missing")
	}

	in.Cluster.Spec.LicenseAcceptances = in.Cluster.Spec.LicenseAcceptances[1:]
	if _, err := Render(in); err == nil || !strings.Contains(err.Error(), "image/vllm-spark-b12x") {
		t.Fatalf("want an acceptance error for image/vllm-spark-b12x, got %v", err)
	}
}

func checkGolden(t *testing.T, name string, ps []Project) {
	t.Helper()
	root := filepath.Join("testdata", "golden", name)
	got := map[string][]byte{}
	for _, p := range ps {
		for _, f := range p.Files {
			got[filepath.Join(p.Node, p.Name, f.Path)] = f.Content
		}
	}
	if *update {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		for rel, b := range got {
			dst := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			// .env holds fake secrets only; keep the golden copy readable in the repo.
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		seen[rel] = true
		want, _ := os.ReadFile(p)
		if g, ok := got[rel]; !ok {
			t.Errorf("%s: golden file no longer rendered", rel)
		} else if !bytes.Equal(g, want) {
			t.Errorf("%s differs from golden (run go test ./internal/render -update and review the diff)\n--- got ---\n%s", rel, g)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel := range got {
		if !seen[rel] {
			t.Errorf("%s: rendered but has no golden file (run with -update)", rel)
		}
	}
}

func TestProjectsPerNode(t *testing.T) {
	var names []string
	for _, p := range mustRender(t, pairInput(t)) {
		names = append(names, p.Node+"/"+p.Name)
	}
	want := []string{
		"spark-1/gateway", "spark-1/langfuse", "spark-1/metrics", "spark-1/model-fast", "spark-1/model-think", "spark-1/node-exporter",
		"spark-2/model-think", "spark-2/node-exporter",
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("projects\n got %v\nwant %v", names, want)
	}
}

func TestDeterministic(t *testing.T) {
	a := mustRender(t, pairInput(t))
	b := mustRender(t, pairInput(t))
	for i := range a {
		for j := range a[i].Files {
			if !bytes.Equal(a[i].Files[j].Content, b[i].Files[j].Content) {
				t.Fatalf("%s/%s/%s differs between renders", a[i].Node, a[i].Name, a[i].Files[j].Path)
			}
		}
	}
}

// Secret values may appear only in .env (mode 0600), never in compose files or
// configs, which are readable and shown by `oaf apply --dry-run`.
func TestSecretsOnlyInEnvFiles(t *testing.T) {
	in := pairInput(t)
	var values []string
	for _, n := range []string{SecretPostgres, SecretClickHouse, SecretValkey, SecretS3AccessKey, SecretS3SecretKey,
		SecretLangfuseSalt, SecretLangfuseEncrypt, SecretLangfuseNextAuth, SecretLangfusePublicKey, SecretLangfuseSecretKey,
		SecretLangfuseAdmin, SecretGrafanaAdmin, SecretVLLMAPIKey, "gateway-key-default"} {
		values = append(values, fakeSecret(n))
	}
	values = append(values, base64.StdEncoding.EncodeToString([]byte("pk-lf-"+fakeSecret(SecretLangfusePublicKey)+":sk-lf-"+fakeSecret(SecretLangfuseSecretKey))))
	envs := 0
	for _, p := range mustRender(t, in) {
		for _, f := range p.Files {
			if f.Path == ".env" {
				envs++
				if f.Mode != 0o600 {
					t.Errorf("%s/%s/.env mode %o, want 600", p.Node, p.Name, f.Mode)
				}
				continue
			}
			for _, v := range values {
				if bytes.Contains(f.Content, []byte(v)) {
					t.Errorf("%s/%s/%s contains a secret value", p.Node, p.Name, f.Path)
				}
			}
		}
	}
	if envs == 0 {
		t.Fatal("no .env files rendered")
	}
	// The gateway key is present as its hash.
	sum := sha256.Sum256([]byte(fakeSecret("gateway-key-default")))
	if !bytes.Contains(fileOf(t, in, "spark-1", ProjectGateway, "config.yaml"), []byte("sha256:"+hex.EncodeToString(sum[:]))) {
		t.Error("gateway config does not hold the API key hash")
	}
}

func TestImagesArePinnedComponents(t *testing.T) {
	in := pairInput(t)
	pinned := map[string]bool{}
	for _, c := range in.Versions.Components {
		pinned[c.Image] = true
	}
	for _, p := range mustRender(t, in) {
		var cf composeFile
		if err := yaml.Unmarshal(fileOf(t, in, p.Node, p.Name, "compose.yaml"), &cf); err != nil {
			t.Fatal(err)
		}
		for name, s := range cf.Services {
			if !pinned[s.Image] || !strings.Contains(s.Image, "@sha256:") {
				t.Errorf("%s/%s service %s: image %q is not a pinned bundle image", p.Node, p.Name, name, s.Image)
			}
		}
	}
}

func TestGPURequestUsesCDINotRuntime(t *testing.T) {
	in := pairInput(t)
	b := fileOf(t, in, "spark-1", "model-fast", "compose.yaml")
	if !bytes.Contains(b, []byte("gpus: all")) || bytes.Contains(b, []byte("runtime:")) {
		t.Fatalf("vLLM must request the GPU with gpus: all, never runtime: nvidia\n%s", b)
	}
}

func TestPairNCCL(t *testing.T) {
	in := pairInput(t)
	r0 := string(fileOf(t, in, "spark-1", "model-think", "compose.yaml"))
	r1 := string(fileOf(t, in, "spark-2", "model-think", "compose.yaml"))
	for _, want := range []string{"NCCL_IB_HCA: rocep1s0f0,roceP2p1s0f0", "NCCL_IB_GID_INDEX: \"3\"", "VLLM_HOST_IP: 10.10.0.1", "- --master-addr\n      - 10.10.0.1", "/dev/infiniband", "- --port\n      - \"8001\""} {
		if !strings.Contains(r0, want) {
			t.Errorf("rank 0 compose lacks %q\n%s", want, r0)
		}
	}
	for _, want := range []string{"VLLM_HOST_IP: 10.10.0.2", "- --node-rank\n      - \"1\"", "- --headless", "- --master-addr\n      - 10.10.0.1"} {
		if !strings.Contains(r1, want) {
			t.Errorf("rank 1 compose lacks %q\n%s", want, r1)
		}
	}
	if strings.Contains(r1, "VLLM_API_KEY") || strings.Contains(r1, "healthcheck") {
		t.Error("headless rank serves no API, so it needs no key or HTTP healthcheck")
	}
}

func TestPairSocketTransport(t *testing.T) {
	in := pairInput(t)
	in.Cluster.Spec.Fabric.NCCL.Transport = config.TransportSocket
	b := string(fileOf(t, in, "spark-2", "model-think", "compose.yaml"))
	if !strings.Contains(b, `NCCL_IB_DISABLE: "1"`) || strings.Contains(b, "/dev/infiniband") || strings.Contains(b, "NCCL_IB_HCA") {
		t.Fatalf("socket transport rendered RDMA settings\n%s", b)
	}
}

func TestLinkSubnetsSelectRailAddress(t *testing.T) {
	in := pairInput(t)
	// A rail with two addresses: the link's subnet decides, and the GID follows the address.
	p := &in.Fabric["spark-1"][0].Halves[0]
	p.Addrs = append([]netip.Prefix{netip.MustParsePrefix("192.168.1.11/24")}, p.Addrs...)
	p.GIDs = append(p.GIDs, fabric.GID{Index: 5, Type: "RoCE v2", GID: "0000:0000:0000:0000:0000:ffff:c0a8:010b"})
	in.Cluster.Spec.Fabric.Links[0].Subnets = []string{"10.10.0.0/30", "10.10.1.0/30"}
	b := string(fileOf(t, in, "spark-1", "model-think", "compose.yaml"))
	if !strings.Contains(b, "VLLM_HOST_IP: 10.10.0.1") || !strings.Contains(b, `NCCL_IB_GID_INDEX: "3"`) {
		t.Fatalf("rail address/GID not chosen by subnet\n%s", b)
	}
}

func TestRenderErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
		want   string
	}{
		{"vllm terms not accepted", func(in *Input) { in.Cluster.Spec.LicenseAcceptances = nil }, "record image/vllm in spec.licenseAcceptances"},
		{"no fabric facts", func(in *Input) { in.Fabric = nil }, "no fabric discovery for node"},
		{"port without carrier", func(in *Input) { in.Fabric["spark-2"][0].Halves[1].OperState = "down" }, "has no carrier"},
		{"rail without address", func(in *Input) { in.Fabric["spark-2"][0].Halves[1].Addrs = nil }, "has no IPv4 address"},
		{"rails disagree on GID", func(in *Input) {
			h := &in.Fabric["spark-2"][0].Halves[1]
			h.GIDs[1].Index = 4
		}, "different RoCE v2 GID indexes"},
		{"invalid cluster", func(in *Input) { in.Cluster.Spec.Models[0].GPUMemoryUtilization = 0.8 }, "cluster.yaml is not valid"},
		{"component missing from bundle", func(in *Input) { in.Versions.Components = in.Versions.Components[1:] }, `pins no "agentgateway" component`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := pairInput(t)
			tc.mutate(&in)
			_, err := Render(in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDisabledModelNotRendered(t *testing.T) {
	in := pairInput(t)
	off := false
	in.Cluster.Spec.Models[1].Enabled = &off
	in.Cluster.Spec.Routing.Rules = in.Cluster.Spec.Routing.Rules[1:] // the "think" rule targets it
	for _, p := range mustRender(t, in) {
		if p.Name == "model-think" {
			t.Fatalf("disabled model rendered on %s", p.Node)
		}
	}
	if bytes.Contains(fileOf(t, in, "spark-1", ProjectGateway, "config.yaml"), []byte("name: think")) {
		t.Error("gateway still routes to a disabled model")
	}
}

func TestModelProjectName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	for served, want := range map[string]string{
		"fast":                   "model-fast",
		"qwen3-30b":              "model-qwen3-30b",
		"openai/gpt":             "",
		"openai-gpt":             "model-openai-gpt",
		"a.b_c":                  "",
		strings.Repeat("x", 100): "",
	} {
		got := modelProjectName(served)
		if !valid.MatchString(got) {
			t.Errorf("%q -> %q is not a valid project name", served, got)
		}
		if want != "" && got != want {
			t.Errorf("%q -> %q, want %q", served, got, want)
		}
	}
	if modelProjectName("openai/gpt") == modelProjectName("openai-gpt") {
		t.Error("lossy mapping collides with an exact name")
	}
}

func fileOf(t *testing.T, in Input, node, project, path string) []byte {
	t.Helper()
	for _, p := range mustRender(t, in) {
		if p.Node == node && p.Name == project {
			for _, f := range p.Files {
				if f.Path == path {
					return f.Content
				}
			}
		}
	}
	t.Fatalf("no %s/%s/%s rendered", node, project, path)
	return nil
}

// TestMemoryLimits: every stack container is capped, vLLM is not, and the
// whole stack fits in 8 GiB.
func TestMemoryLimits(t *testing.T) {
	total := 0
	for _, mib := range memoryLimitsMiB {
		total += mib
	}
	if total > 8*1024 {
		t.Fatalf("stack memory limits add up to %d MiB", total)
	}
	in := pairInput(t)
	for _, p := range mustRender(t, in) {
		var cf composeFile
		if err := yaml.Unmarshal(fileOf(t, in, p.Node, p.Name, "compose.yaml"), &cf); err != nil {
			t.Fatal(err)
		}
		for name, s := range cf.Services {
			if name == "vllm" {
				if s.MemLimit != "" {
					t.Errorf("%s/%s: vLLM must not get a cgroup limit (unified memory is sized by --gpu-memory-utilization)", p.Node, p.Name)
				}
				continue
			}
			if s.MemLimit == "" || s.MemswapLimit != s.MemLimit {
				t.Errorf("%s/%s/%s: no memory limit (add it to memoryLimitsMiB)", p.Node, p.Name, name)
			}
		}
	}
}

// TestConfigChangeRecreatesContainer: a change to a mounted config file must
// change the compose file, or compose leaves the old container running.
func TestConfigChangeRecreatesContainer(t *testing.T) {
	label := func(in Input, project, service string) string {
		for _, p := range mustRender(t, in) {
			if p.Name == project {
				var cf composeFile
				if err := yaml.Unmarshal(p.Files[0].Content, &cf); err != nil {
					t.Fatal(err)
				}
				return cf.Services[service].Labels[ConfigHashLabel]
			}
		}
		t.Fatalf("no project %s", project)
		return ""
	}
	in := singleInput(t)
	gw, prom, graf := label(in, "gateway", "agentgateway"), label(in, "metrics", "prometheus"), label(in, "metrics", "grafana")
	if gw == "" || prom == "" || graf == "" {
		t.Fatalf("config hash labels missing: gateway %q prometheus %q grafana %q", gw, prom, graf)
	}
	in.Cluster.Spec.Routing.Gateway.Port = 4100 // changes config.yaml only
	in.Cluster.Spec.Observability.Metrics.ScrapeInterval = "30s"
	if label(in, "gateway", "agentgateway") == gw || label(in, "metrics", "prometheus") == prom {
		t.Fatal("a config file change did not change the config hash label")
	}
	if label(in, "metrics", "grafana") != graf {
		t.Fatal("grafana's label changed though its files did not")
	}
}

func TestDashboards(t *testing.T) {
	files, err := dashboardFiles("test")
	if err != nil {
		t.Fatal(err)
	}
	uids := map[string]bool{}
	for _, f := range files[1:] {
		var d struct {
			UID    string `json:"uid"`
			Panels []struct {
				Type    string         `json:"type"`
				GridPos map[string]int `json:"gridPos"`
				Targets []struct {
					Expr string `json:"expr"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal(f.Content, &d); err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		if uids[d.UID] || f.Path != "grafana/dashboards/"+d.UID+".json" {
			t.Errorf("%s: uid %q duplicated or not matching the file name", f.Path, d.UID)
		}
		uids[d.UID] = true
		for _, p := range d.Panels {
			if p.GridPos["x"]+p.GridPos["w"] > 24 {
				t.Errorf("%s: panel overflows the grid: %v", f.Path, p.GridPos)
			}
			if p.Type != "row" && len(p.Targets) == 0 {
				t.Errorf("%s: panel without a query", f.Path)
			}
			for _, tg := range p.Targets {
				if strings.Count(tg.Expr, "(") != strings.Count(tg.Expr, ")") || strings.Count(tg.Expr, "{") != strings.Count(tg.Expr, "}") {
					t.Errorf("%s: unbalanced query %s", f.Path, tg.Expr)
				}
			}
		}
	}
	if !uids[strings.TrimSuffix(homeDashboard, ".json")] {
		t.Errorf("home dashboard %s is not rendered", homeDashboard)
	}
}
