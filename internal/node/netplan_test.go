package node

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openagentfabric.dev/oaf/internal/fabric"
	"openagentfabric.dev/oaf/internal/hostexec"
)

// fakeHost plays the box: netplan apply makes OAF's file live (addresses and
// MTU on the netdevs), ip addr add/del change addresses directly.
type fakeHost struct {
	mu    sync.Mutex
	file  string
	ports []fabric.Port
	cmds  []string
}

func (h *fakeHost) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cmds = append(h.cmds, hostexec.CmdLine(name, args...))
	switch {
	case name == "netplan":
		owned := map[string][]netip.Prefix{}
		if b, err := os.ReadFile(h.file); err == nil {
			owned, _ = parseNetplan(b)
		}
		for i := range h.ports {
			for j := range h.ports[i].Halves {
				nd := &h.ports[i].Halves[j]
				nd.Addrs = owned[nd.Name]
				if len(owned[nd.Name]) > 0 {
					nd.MTU = 9000
				}
			}
		}
	case name == "ip" && args[1] == "add":
		h.each(args[4], func(nd *fabric.Netdev) { nd.Addrs = append(nd.Addrs, netip.MustParsePrefix(args[2])) })
	case name == "ip" && args[1] == "del":
		h.each(args[4], func(nd *fabric.Netdev) {
			var keep []netip.Prefix
			for _, a := range nd.Addrs {
				if a.String() != args[2] {
					keep = append(keep, a)
				}
			}
			nd.Addrs = keep
		})
	}
	return nil, nil
}

func (h *fakeHost) each(dev string, f func(*fabric.Netdev)) {
	for i := range h.ports {
		for j := range h.ports[i].Halves {
			if h.ports[i].Halves[j].Name == dev {
				f(&h.ports[i].Halves[j])
			}
		}
	}
}

func (h *fakeHost) discover(context.Context) ([]fabric.Port, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]fabric.Port, len(h.ports))
	for i, p := range h.ports {
		out[i] = fabric.Port{Index: p.Index, Halves: append([]fabric.Netdev(nil), p.Halves...)}
	}
	return out, nil
}

func (h *fakeHost) applies() int {
	n := 0
	for _, c := range h.cmds {
		if c == "netplan apply" {
			n++
		}
	}
	return n
}

func gb10Ports() []fabric.Port {
	var ports []fabric.Port
	for _, i := range []int{0, 1} {
		p := fabric.Port{Index: i}
		for _, pre := range []string{"enp1s0f", "enP2p1s0f"} {
			n := pre + map[int]string{0: "0np0", 1: "1np1"}[i]
			p.Halves = append(p.Halves, fabric.Netdev{Name: n, OperState: "up", MTU: 1500})
		}
		ports = append(ports, p)
	}
	return ports
}

func newFabricServer(t *testing.T) (*Server, *fakeHost) {
	t.Helper()
	h := &fakeHost{file: filepath.Join(t.TempDir(), "40-oaf-fabric.yaml"), ports: gb10Ports()}
	return &Server{Host: h, NetplanFile: h.file, DiscoverFabric: h.discover, FabricWait: time.Second}, h
}

func rail(nd, addr string) RailSpec {
	return RailSpec{Netdev: nd, Addr: netip.MustParsePrefix(addr)}
}

const wantNetplan = `# Written by OAF (oaf-node): the QSFP fabric rails in cluster.yaml's fabric.links.
# Do not edit: ` + "`oaf apply`" + ` rewrites this file. Other netplan files are never touched.
network:
  version: 2
  renderer: NetworkManager
  ethernets:
    enP2p1s0f1np1: {dhcp4: false, dhcp6: false, link-local: [], mtu: 9000, addresses: [10.200.0.5/30]}
    enp1s0f1np1: {dhcp4: false, dhcp6: false, link-local: [], mtu: 9000, addresses: [10.200.0.1/30]}
`

// The file is the one the user guide had people write, the rails come up,
// and a second identical request changes nothing.
func TestConfigureFabricWritesAppliesAndIsIdempotent(t *testing.T) {
	s, h := newFabricServer(t)
	rails := []RailSpec{rail("enp1s0f1np1", "10.200.0.1/30"), rail("enP2p1s0f1np1", "10.200.0.5/30")}
	res, err := s.ConfigureFabricRails(context.Background(), rails, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(h.file)
	if string(b) != wantNetplan {
		t.Fatalf("netplan file:\n%s", b)
	}
	if fi, _ := os.Stat(h.file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v (netplan warns about readable files)", fi.Mode().Perm())
	}
	if !res.Changed || h.applies() != 1 || len(res.Rails) != 2 || !res.Rails[0].Present || res.Rails[0].MTU != 9000 {
		t.Fatalf("result %+v, applies %d", res, h.applies())
	}
	res, err = s.ConfigureFabricRails(context.Background(), rails, false)
	if err != nil || res.Changed || h.applies() != 1 {
		t.Fatalf("second run must change nothing: %+v %v applies=%d", res, err, h.applies())
	}
	// Dry run of a change writes nothing.
	res, err = s.ConfigureFabricRails(context.Background(), append(rails, rail("enp1s0f0np0", "10.200.0.9/30"), rail("enP2p1s0f0np0", "10.200.0.13/30")), true)
	if err != nil || !res.Changed || h.applies() != 1 {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if b2, _ := os.ReadFile(h.file); string(b2) != wantNetplan {
		t.Fatal("dry run wrote the file")
	}
}

// A hand-written file at OAF's path (the user guide's) is adopted: its
// addresses are OAF's, so adding the second cable's rails is allowed.
func TestConfigureFabricAdoptsHandWrittenFile(t *testing.T) {
	s, h := newFabricServer(t)
	hand := "network:\n  version: 2\n  renderer: NetworkManager\n  ethernets:\n" +
		"    enp1s0f1np1:   {dhcp4: false, dhcp6: false, link-local: [], mtu: 9000, addresses: [10.200.0.1/30]}\n" +
		"    enP2p1s0f1np1: {dhcp4: false, dhcp6: false, link-local: [], mtu: 9000, addresses: [10.200.0.5/30]}\n"
	if err := os.WriteFile(h.file, []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Run(context.Background(), "netplan", "apply"); err != nil {
		t.Fatal(err)
	}
	rails := []RailSpec{rail("enp1s0f1np1", "10.200.0.1/30"), rail("enP2p1s0f1np1", "10.200.0.5/30"),
		rail("enp1s0f0np0", "10.200.0.9/30"), rail("enP2p1s0f0np0", "10.200.0.13/30")}
	res, err := s.ConfigureFabricRails(context.Background(), rails, false)
	if err != nil || !res.Changed {
		t.Fatalf("adopt: %+v %v", res, err)
	}
	b, _ := os.ReadFile(h.file)
	if !strings.Contains(string(b), "enp1s0f0np0: {dhcp4: false, dhcp6: false, link-local: [], mtu: 9000, addresses: [10.200.0.9/30]}") || !strings.HasPrefix(string(b), "# Written by OAF") {
		t.Fatalf("file:\n%s", b)
	}
}

// A port something else configured is never taken over, and only this box's
// QSFP netdevs can be named.
func TestConfigureFabricRefuses(t *testing.T) {
	s, h := newFabricServer(t)
	h.each("enp1s0f0np0", func(nd *fabric.Netdev) { nd.Addrs = []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24")} })
	_, err := s.ConfigureFabricRails(context.Background(), []RailSpec{rail("enp1s0f0np0", "10.200.0.9/30")}, false)
	if err == nil || !strings.Contains(err.Error(), "which OAF did not assign") {
		t.Fatalf("foreign address: %v", err)
	}
	if _, err := s.ConfigureFabricRails(context.Background(), []RailSpec{rail("wlan0", "10.200.0.9/30")}, false); err == nil || !strings.Contains(err.Error(), "not a QSFP netdev") {
		t.Fatalf("non-QSFP netdev: %v", err)
	}
	if h.applies() != 0 {
		t.Fatal("a refused request ran netplan")
	}
	s.Host = nil
	if _, err := s.ConfigureFabricRails(context.Background(), []RailSpec{rail("enp1s0f1np1", "10.200.0.1/30")}, false); err == nil {
		t.Fatal("an agent without host changes changed the fabric")
	}
}

// No rails: OAF's file goes (links dropped from cluster.yaml); without one,
// nothing happens.
func TestConfigureFabricRemove(t *testing.T) {
	s, h := newFabricServer(t)
	if res, err := s.ConfigureFabricRails(context.Background(), nil, false); err != nil || res.Changed || h.applies() != 0 {
		t.Fatalf("no file: %+v %v", res, err)
	}
	if _, err := s.ConfigureFabricRails(context.Background(), []RailSpec{rail("enp1s0f1np1", "10.200.0.1/30")}, false); err != nil {
		t.Fatal(err)
	}
	res, err := s.ConfigureFabricRails(context.Background(), nil, false)
	if err != nil || !res.Changed || h.applies() != 2 {
		t.Fatalf("remove: %+v %v applies=%d", res, err, h.applies())
	}
	if _, err := os.Stat(h.file); !os.IsNotExist(err) {
		t.Fatal("file still there")
	}
}

// Probes go on free netdevs only, and come off again.
func TestProbeAddr(t *testing.T) {
	s, h := newFabricServer(t)
	p := netip.MustParsePrefix("198.18.0.1/24")
	if err := s.ProbeAddr(context.Background(), "enp1s0f0np0", p, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ProbeAddr(context.Background(), "enp1s0f0np0", p, true); err != nil {
		t.Fatal(err)
	}
	h.each("enp1s0f1np1", func(nd *fabric.Netdev) { nd.Addrs = []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24")} })
	if err := s.ProbeAddr(context.Background(), "enp1s0f1np1", p, false); err == nil || !strings.Contains(err.Error(), "not probing") {
		t.Fatalf("probe on a foreign port: %v", err)
	}
	want := []string{"ip addr add 198.18.0.1/24 dev enp1s0f0np0", "ip addr del 198.18.0.1/24 dev enp1s0f0np0"}
	if strings.Join(h.cmds, "|") != strings.Join(want, "|") {
		t.Fatalf("commands: %q", h.cmds)
	}
}

func TestPingParsing(t *testing.T) {
	out := "PING 10.200.0.2 (10.200.0.2) from 10.200.0.1 enp1s0f1np1: 56(84) bytes of data.\n\n--- 10.200.0.2 ping statistics ---\n2 packets transmitted, 2 received, 0% packet loss, time 1001ms\nrtt min/avg/max/mdev = 0.276/0.458/0.641/0.182 ms\n"
	if v := pingRTT(out); v != 0.458 {
		t.Fatalf("rtt %v", v)
	}
	nb := `[{"dst":"198.18.0.7","lladdr":"4C:BB:47:00:00:0B","state":["REACHABLE"]},{"dst":"198.18.0.8","state":["FAILED"]}]`
	if m := neighborMAC([]byte(nb), netip.MustParseAddr("198.18.0.7")); m != "4c:bb:47:00:00:0b" {
		t.Fatalf("mac %q", m)
	}
	if m := neighborMAC([]byte(nb), netip.MustParseAddr("198.18.0.8")); m != "" {
		t.Fatalf("failed entry gave %q", m)
	}
}
