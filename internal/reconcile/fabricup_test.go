package reconcile

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect/v2"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/gen/oaf/node/v1/nodev1connect"
	"openagentfabric.dev/oaf/internal/node"
)

// wire is the cabling of a fake two-box fabric: which netdev of which box
// is on the other end of a netdev, and the addresses each netdev has.
type wire struct {
	mu    sync.Mutex
	peer  map[string]string // "box/netdev" -> "box/netdev"
	addrs map[string][]netip.Prefix
	mtu   map[string]int
}

func (w *wire) owner(addr netip.Addr) string {
	for k, as := range w.addrs {
		for _, a := range as {
			if a.Addr() == addr {
				return k
			}
		}
	}
	return ""
}

// fabricAgent is one box: ports with MACs, ConfigureFabric, probes and
// pings over the wire.
type fabricAgent struct {
	nodev1connect.UnimplementedNodeServiceHandler
	name       string
	w          *wire
	configured [][]*nodev1.FabricRail
	old        bool // an agent without ConfigureFabric
}

func gb10(box string, w *wire) []fabric.Port {
	var ports []fabric.Port
	for i := 0; i < 2; i++ {
		p := fabric.Port{Index: i}
		for h, pre := range []string{"enp1s0f", "enP2p1s0f"} {
			n := fmt.Sprintf("%s%dnp%d", pre, i, i)
			k := box + "/" + n
			mac := fmt.Sprintf("02:00:00:%02x:%02x:%02x", len(box), i, h)
			if box == "b" {
				mac = fmt.Sprintf("02:00:01:%02x:%02x:%02x", len(box), i, h)
			}
			p.Halves = append(p.Halves, fabric.Netdev{Name: n, OperState: "up", MTU: w.mtu[k], MAC: mac, Addrs: w.addrs[k]})
		}
		ports = append(ports, p)
	}
	return ports
}

func (a *fabricAgent) Fabric(context.Context, *nodev1.FabricRequest) (*nodev1.FabricResponse, error) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	return &nodev1.FabricResponse{Ports: node.FabricToProto(gb10(a.name, a.w))}, nil
}

func (a *fabricAgent) ConfigureFabric(_ context.Context, req *nodev1.ConfigureFabricRequest) (*nodev1.ConfigureFabricResponse, error) {
	if a.old {
		return nil, connect.NewError(connect.CodeUnimplemented, "no")
	}
	a.configured = append(a.configured, req.GetRails())
	resp := &nodev1.ConfigureFabricResponse{}
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	for _, r := range req.GetRails() {
		k := a.name + "/" + r.GetNetdev()
		p := netip.MustParsePrefix(r.GetAddress())
		if len(a.w.addrs[k]) != 1 || a.w.addrs[k][0] != p || a.w.mtu[k] != int(r.GetMtu()) {
			resp.Changed = true
		}
		if !req.GetDryRun() {
			a.w.addrs[k], a.w.mtu[k] = []netip.Prefix{p}, int(r.GetMtu())
		}
		resp.Rails = append(resp.Rails, &nodev1.RailState{Netdev: r.GetNetdev(), Address: r.GetAddress(), Present: true, Mtu: r.GetMtu(), OperState: "up"})
	}
	return resp, nil
}

func (a *fabricAgent) ProbeAddress(_ context.Context, req *nodev1.ProbeAddressRequest) (*nodev1.ProbeAddressResponse, error) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	k := a.name + "/" + req.GetNetdev()
	p := netip.MustParsePrefix(req.GetAddress())
	if req.GetRemove() {
		var keep []netip.Prefix
		for _, x := range a.w.addrs[k] {
			if x != p {
				keep = append(keep, x)
			}
		}
		a.w.addrs[k] = keep
		return &nodev1.ProbeAddressResponse{}, nil
	}
	a.w.addrs[k] = append(a.w.addrs[k], p)
	return &nodev1.ProbeAddressResponse{}, nil
}

// Ping: the target answers if its owner is the box on the other end of the
// cable (any of its addresses: arp_ignore 0), with the MAC of the peer
// netdev; a jumbo ping fails unless both ends have the MTU.
func (a *fabricAgent) Ping(_ context.Context, req *nodev1.PingRequest) (*nodev1.PingResponse, error) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	from := a.name + "/" + req.GetNetdev()
	peer, ok := a.w.peer[from]
	if !ok {
		return &nodev1.PingResponse{}, nil
	}
	t := netip.MustParseAddr(req.GetTarget())
	owner := a.w.owner(t)
	if owner == "" || strings.Split(owner, "/")[0] != strings.Split(peer, "/")[0] {
		return &nodev1.PingResponse{}, nil
	}
	peerBox, peerDev := strings.Split(peer, "/")[0], strings.Split(peer, "/")[1]
	var mac string
	for _, p := range gb10(peerBox, a.w) {
		for _, h := range p.Halves {
			if h.Name == peerDev {
				mac = h.MAC
			}
		}
	}
	ok = true
	if req.GetDontFragment() && int(req.GetSize())+28 > min(a.w.mtu[from], a.w.mtu[peer]) {
		ok = false
	}
	return &nodev1.PingResponse{Ok: ok, RttMs: 0.4, NeighborMac: mac}, nil
}

// Two boxes cabled port 0 ↔ port 0 and port 1 ↔ port 1, rail h to rail h.
func twoCables() *wire {
	w := &wire{peer: map[string]string{}, addrs: map[string][]netip.Prefix{}, mtu: map[string]int{}}
	for i := 0; i < 2; i++ {
		for _, pre := range []string{"enp1s0f", "enP2p1s0f"} {
			n := fmt.Sprintf("%s%dnp%d", pre, i, i)
			w.peer["a/"+n], w.peer["b/"+n] = "b/"+n, "a/"+n
			w.mtu["a/"+n], w.mtu["b/"+n] = 1500, 1500
		}
	}
	return w
}

func twoLinkCluster() *config.Cluster {
	zero, one := 0, 1
	return &config.Cluster{Spec: config.ClusterSpec{
		Nodes: []config.Node{{Name: "a"}, {Name: "b"}, {Name: "ctl"}},
		Fabric: config.Fabric{Topology: config.TopologyPairPlusSingle, MTU: 9000, Links: []config.Link{
			{A: config.LinkEnd{Node: "a", Port: &one}, B: config.LinkEnd{Node: "b", Port: &one}, Subnets: []string{"10.200.0.0/30", "10.200.0.4/30"}},
			{A: config.LinkEnd{Node: "a", Port: &zero}, B: config.LinkEnd{Node: "b", Port: &zero}, Subnets: []string{"10.200.0.8/30", "10.200.0.12/30"}},
		}},
	}}
}

// Bring-up gives each end the first/second host of each rail's /30, checks
// every rail (ping and MTU), and a second run changes nothing. A node with no
// rails is told so; an old agent there is fine.
func TestFabricUp(t *testing.T) {
	w := twoCables()
	a, b := &fabricAgent{name: "a", w: w}, &fabricAgent{name: "b", w: w}
	ctl := &fabricAgent{name: "ctl", w: w, old: true}
	ns := Nodes{"a": a, "b": b, "ctl": ctl}
	var out bytes.Buffer
	changed, err := FabricUp(context.Background(), ns, twoLinkCluster(), false, &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !changed {
		t.Fatal("nothing changed")
	}
	want := map[string]string{"enp1s0f1np1": "10.200.0.1/30", "enP2p1s0f1np1": "10.200.0.5/30", "enp1s0f0np0": "10.200.0.9/30", "enP2p1s0f0np0": "10.200.0.13/30"}
	for _, r := range a.configured[0] {
		if want[r.GetNetdev()] != r.GetAddress() || r.GetMtu() != 9000 {
			t.Fatalf("a got %v", a.configured[0])
		}
	}
	if b.configured[0][0].GetAddress() != "10.200.0.2/30" {
		t.Fatalf("b got %v", b.configured[0])
	}
	for _, line := range []string{"updated   a/fabric", "updated   b/fabric", "link  a:enp1s0f1np1 ↔ b:enp1s0f1np1  0.40 ms, MTU 9000 ok", "link  a:enP2p1s0f0np0 ↔ b:enP2p1s0f0np0"} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("output lacks %q:\n%s", line, out.String())
		}
	}
	out.Reset()
	if changed, err := FabricUp(context.Background(), ns, twoLinkCluster(), false, &out); err != nil || changed {
		t.Fatalf("second run: changed=%v err=%v\n%s", changed, err, out.String())
	}
}

// A rail that does not answer fails the apply; a rail without the MTU too.
func TestFabricUpCheckFails(t *testing.T) {
	old := railRetry
	railRetry = 0
	defer func() { railRetry = old }()
	w := twoCables()
	delete(w.peer, "a/enp1s0f0np0") // port 0 rail 0 unplugged
	ns := Nodes{"a": &fabricAgent{name: "a", w: w}, "b": &fabricAgent{name: "b", w: w}, "ctl": &fabricAgent{name: "ctl", w: w}}
	var out bytes.Buffer
	_, err := FabricUp(context.Background(), ns, twoLinkCluster(), false, &out)
	if err == nil || !strings.Contains(err.Error(), "a:enp1s0f0np0 ↔ b:enp1s0f0np0") {
		t.Fatalf("err %v\n%s", err, out.String())
	}
}

// Hand-addressed links (no subnets) and their nodes are left alone.
func TestFabricUpLeavesManualLinks(t *testing.T) {
	w := twoCables()
	a, b := &fabricAgent{name: "a", w: w}, &fabricAgent{name: "b", w: w}
	c := twoLinkCluster()
	c.Spec.Fabric.Links = c.Spec.Fabric.Links[:1]
	c.Spec.Fabric.Links[0].Subnets = nil
	if _, err := FabricUp(context.Background(), Nodes{"a": a, "b": b, "ctl": &fabricAgent{name: "ctl", w: w, old: true}}, c, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(a.configured) != 0 || len(b.configured) != 0 {
		t.Fatal("a hand-addressed node was reconfigured")
	}
}

// An old agent on a node that needs rails is an error naming the fix.
func TestFabricUpOldAgent(t *testing.T) {
	w := twoCables()
	ns := Nodes{"a": &fabricAgent{name: "a", w: w, old: true}, "b": &fabricAgent{name: "b", w: w}, "ctl": &fabricAgent{name: "ctl", w: w}}
	if _, err := FabricUp(context.Background(), ns, twoLinkCluster(), false, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "upgrade oaf") {
		t.Fatalf("err %v", err)
	}
}

// The probe finds both cables and leaves no probe address behind.
func TestProbeCabling(t *testing.T) {
	w := twoCables()
	ns := Nodes{"a": &fabricAgent{name: "a", w: w}, "b": &fabricAgent{name: "b", w: w}}
	cables, err := ProbeCabling(context.Background(), ns, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cables) != 2 || cables[0] != (ProbedCable{ANode: "a", APort: 0, BNode: "b", BPort: 0}) || cables[1] != (ProbedCable{ANode: "a", APort: 1, BNode: "b", BPort: 1}) {
		t.Fatalf("cables %+v", cables)
	}
	for k, as := range w.addrs {
		for _, p := range as {
			if ProbePool.Contains(p.Addr()) {
				t.Fatalf("probe address %s left on %s", p, k)
			}
		}
	}
	// One cable only (port 1 crossed to port 0 on the other box).
	w = &wire{peer: map[string]string{"a/enp1s0f1np1": "b/enp1s0f0np0", "b/enp1s0f0np0": "a/enp1s0f1np1",
		"a/enP2p1s0f1np1": "b/enP2p1s0f0np0", "b/enP2p1s0f0np0": "a/enP2p1s0f1np1"}, addrs: map[string][]netip.Prefix{}, mtu: map[string]int{}}
	ns = Nodes{"a": &fabricAgent{name: "a", w: w}, "b": &fabricAgent{name: "b", w: w}}
	cables, err = ProbeCabling(context.Background(), ns, []string{"a", "b"})
	if err != nil || len(cables) != 1 || cables[0] != (ProbedCable{ANode: "a", APort: 1, BNode: "b", BPort: 0}) {
		t.Fatalf("crossed cable: %+v %v", cables, err)
	}
}
