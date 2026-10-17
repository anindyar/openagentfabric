package preflight

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/fabric"
	"openagentfabric.dev/oaf/internal/hostexec"
)

// The fixture is a real, read-only capture from a DGX Spark in use
// (threeofnine), scrubbed of MACs and GPU UUIDs.
func replay(t *testing.T) hostexec.Replay {
	t.Helper()
	r, err := hostexec.LoadReplay("testdata/threeofnine.json")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func run(t *testing.T, r hostexec.Runner) Report {
	t.Helper()
	return Run(context.Background(), r, Defaults)
}

func result(t *testing.T, rep Report, id string) Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no result %q", id)
	return Result{}
}

func TestBusyProductionNode(t *testing.T) {
	rep := run(t, replay(t))
	if rep.Host != "threeofnine" {
		t.Errorf("host = %q", rep.Host)
	}
	want := map[string]Status{
		"host": Pass, "gpu": Pass, "memory": Warn, "disk": Pass, "docker": Pass,
		"nvidia-runtime": Pass, "workloads": Warn, "ports": Pass, "fabric": Info,
		"rdma-tools": Pass, "nccl": Warn, "time": Pass,
	}
	for id, st := range want {
		if got := result(t, rep, id); got.Status != st {
			t.Errorf("%s: status %s, want %s (%s)", id, got.Status, st, got.Summary)
		}
	}
	if rep.Worst() != Warn {
		t.Errorf("worst = %s", rep.Worst())
	}
	for _, r := range rep.Results {
		if strings.Contains(r.Summary, "refused") {
			t.Errorf("%s ran a command outside the read-only allowlist: %s", r.ID, r.Summary)
		}
	}
}

func TestWorkloadsNamesExistingInference(t *testing.T) {
	res := result(t, run(t, replay(t)), "workloads")
	joined := strings.Join(res.Details, "\n")
	for _, name := range []string{"vllm-ornith", "vllm-qwen"} {
		if !strings.Contains(joined, "inference: "+name) {
			t.Errorf("missing %s in %v", name, res.Details)
		}
	}
}

func TestFabricDiscovery(t *testing.T) {
	ports, err := fabric.Discover(context.Background(), replay(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 {
		t.Fatalf("ports = %d", len(ports))
	}
	p0 := ports[0]
	if p0.Index != 0 || len(p0.Halves) != 2 || p0.Halves[0].Name != "enp1s0f0np0" || p0.Halves[1].Name != "enP2p1s0f0np0" {
		t.Fatalf("port 0 = %+v", p0)
	}
	if !p0.Up() || !p0.InUse() {
		t.Errorf("port 0 should be up and in use")
	}
	// The GID index depends on the address, not the device: same netdev, two IPs.
	h := p0.Halves[0]
	for ip, want := range map[string]int{"192.168.1.166": 3, "10.0.31.1": 5} {
		got, ok := h.RoCEv2GIDFor(netip.MustParseAddr(ip))
		if !ok || got != want {
			t.Errorf("GID for %s = %d (%v), want %d", ip, got, ok, want)
		}
	}
	if _, ok := h.RoCEv2GIDFor(netip.MustParseAddr("10.9.9.9")); ok {
		t.Error("unknown IP must not resolve to a GID")
	}
	if ports[1].Halves[0].InUse() || !ports[1].Halves[1].InUse() {
		t.Errorf("port 1 halves in-use = %v/%v", ports[1].Halves[0].InUse(), ports[1].Halves[1].InUse())
	}
}

// override replaces individual command results on top of the fixture.
type override struct {
	base hostexec.Replay
	with map[string]hostexec.Recording
}

func (o override) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if rec, ok := o.with[hostexec.CmdLine(name, args...)]; ok {
		return hostexec.Replay{"x": rec}.Run(ctx, "x")
	}
	return o.base.Run(ctx, name, args...)
}

func TestFailuresAndWarnings(t *testing.T) {
	cases := []struct {
		name, check string
		want        Status
		with        map[string]hostexec.Recording
	}{
		{"x86 host", "host", Fail, map[string]hostexec.Recording{"uname -m": {Stdout: "x86_64\n"}}},
		{"no GPU path", "nvidia-runtime", Fail, map[string]hostexec.Recording{
			"docker info --format {{json .DiscoveredDevices}}": {Stdout: "null\n"},
		}},
		{"legacy nvidia runtime", "nvidia-runtime", Pass, map[string]hostexec.Recording{
			"docker info --format {{json .DiscoveredDevices}}":                 {Stdout: "null\n"},
			"docker info --format {{range $k, $v := .Runtimes}}{{$k}} {{end}}": {Stdout: "nvidia runc \n"},
		}},
		{"docker down", "docker", Fail, map[string]hostexec.Recording{
			"docker version --format {{.Server.Version}}": {Exit: 1, Stderr: "Cannot connect to the Docker daemon"},
		}},
		{"plenty of memory", "memory", Pass, map[string]hostexec.Recording{
			"cat /proc/meminfo": {Stdout: "MemTotal: 127535336 kB\nMemAvailable: 120000000 kB\n"},
		}},
		{"gateway port taken", "ports", Warn, map[string]hostexec.Recording{
			"ss -Hltn": {Stdout: "LISTEN 0 4096 0.0.0.0:4000 0.0.0.0:*\n"},
		}},
		{"no perftest", "rdma-tools", Warn, map[string]hostexec.Recording{"which ib_write_bw": {Exit: 1}}},
		{"clock unsynced", "time", Warn, map[string]hostexec.Recording{
			"timedatectl show -p NTPSynchronized --value": {Stdout: "no\n"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := result(t, run(t, override{base: replay(t), with: tc.with}), tc.check)
			if res.Status != tc.want {
				t.Fatalf("%s = %s (%s), want %s", tc.check, res.Status, res.Summary, tc.want)
			}
		})
	}
}
