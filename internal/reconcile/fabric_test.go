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

// benchAgent serves fabric discovery and simulated ib_write_bw runs.
type benchAgent struct {
	nodev1connect.UnimplementedNodeServiceHandler
	ports []fabric.Port

	mu       sync.Mutex
	requests []*nodev1.RailBandwidthRequest
	gbps     float64 // one rail alone
	failing  bool    // the client side fails
	waiting  bool    // the server side waits until cancelled
}

func (a *benchAgent) Fabric(context.Context, *nodev1.FabricRequest) (*nodev1.FabricResponse, error) {
	return &nodev1.FabricResponse{Ports: node.FabricToProto(a.ports)}, nil
}

func (a *benchAgent) RailBandwidth(ctx context.Context, req *nodev1.RailBandwidthRequest) (*nodev1.RailBandwidthResponse, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	// Runs 1–2 are each rail alone, 3–4 both rails together.
	together, failing, waiting := len(a.requests) > 2, a.failing, a.waiting
	a.mu.Unlock()
	if req.GetServerAddr() == "" {
		if waiting {
			<-ctx.Done() // a server whose client never connects
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err().Error())
		}
		return &nodev1.RailBandwidthResponse{}, nil
	}
	if failing {
		return nil, connect.NewError(connect.CodeUnavailable, "ib_write_bw: Couldn't connect")
	}
	if together {
		return &nodev1.RailBandwidthResponse{Gbps: 98}, nil
	}
	return &nodev1.RailBandwidthResponse{Gbps: a.gbps}, nil
}

func qsfp(index int, ip0, ip1 string, mtu int) fabric.Port {
	p := fabric.Port{Index: index}
	for h, ip := range []string{ip0, ip1} {
		a := netip.MustParsePrefix(ip)
		v := a.Addr().As4()
		p.Halves = append(p.Halves, fabric.Netdev{
			Name: fmt.Sprintf("%s%dnp%d", []string{"enp1s0f", "enP2p1s0f"}[h], index, index), OperState: "up", MTU: mtu,
			Addrs: []netip.Prefix{a}, RDMADev: fmt.Sprintf("%s%d", []string{"rocep1s0f", "roceP2p1s0f"}[h], index),
			GIDs: []fabric.GID{{Index: 3, Type: "RoCE v2", GID: fmt.Sprintf("0000:0000:0000:0000:0000:ffff:%02x%02x:%02x%02x", v[0], v[1], v[2], v[3])}},
		})
	}
	return p
}

func pairCluster() *config.Cluster {
	one := 1
	return &config.Cluster{Spec: config.ClusterSpec{Fabric: config.Fabric{
		Topology: config.TopologyPair,
		Links:    []config.Link{{A: config.LinkEnd{Node: "a", Port: &one}, B: config.LinkEnd{Node: "b", Port: &one}}},
	}}}
}

type pairAgents struct{ a, b *benchAgent }

// newPair is a cable between port 1 of a and b; a runs the clients.
func newPair(mtuB int, ipB0 string) (Nodes, pairAgents) {
	a := &benchAgent{ports: []fabric.Port{qsfp(1, "192.168.31.2/30", "192.168.41.2/30", 9000)}, gbps: 111.6}
	b := &benchAgent{ports: []fabric.Port{qsfp(1, ipB0, "192.168.41.1/30", mtuB)}}
	return Nodes{"a": a, "b": b}, pairAgents{a, b}
}

func TestCheckFabricMeasuresEachRailAndBoth(t *testing.T) {
	ns, p := newPair(9000, "192.168.31.1/30")
	var out bytes.Buffer
	res := CheckFabric(context.Background(), ns, pairCluster(), FabricOptions{Bandwidth: true}, &out)
	if len(res) != 1 || !res[0].OK() {
		t.Fatalf("result: %+v", res)
	}
	r := res[0]
	if r.Rails[0].Gbps != 111.6 || r.Rails[1].Gbps != 111.6 || r.DualGbps != 196 {
		t.Fatalf("bandwidth: %+v dual %v", r.Rails, r.DualGbps)
	}
	if !strings.Contains(out.String(), "a:1 ↔ b:1  rail 0  192.168.31.2 → 192.168.31.1  111.6 Gb/s") || !strings.Contains(out.String(), "2 rails together  196.0 Gb/s") {
		t.Fatalf("progress:\n%s", out.String())
	}
	// a runs the clients against b's rail addresses with the detected GID;
	// b runs the servers. Concurrent rails use their own exchange ports.
	for _, req := range p.a.requests {
		if req.GetGidIndex() != 3 || req.GetDurationSeconds() != 5 || !strings.HasPrefix(req.GetServerAddr(), "192.168.") {
			t.Fatalf("client request: %v", req)
		}
	}
	if len(p.a.requests) != 4 || len(p.b.requests) != 4 || p.a.requests[1].GetTcpPort() != fabric.BandwidthPortMin+1 {
		t.Fatalf("requests: a=%v b=%v", p.a.requests, p.b.requests)
	}
}

func TestCheckFabricProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		mtuB  int
		ipB0  string
		slow  bool
		fail  bool
		want  string
		bench int // client runs on a
	}{
		"jumbo frames missing": {mtuB: 1500, ipB0: "192.168.31.1/30", want: "has MTU 1500, want 9000"},
		"subnets differ":       {mtuB: 9000, ipB0: "10.0.0.1/30", want: "are not in one subnet"},
		"slow rail":            {mtuB: 9000, ipB0: "192.168.31.1/30", slow: true, want: "rail 0: 40.0 Gb/s is below 85", bench: 4},
		"client fails":         {mtuB: 9000, ipB0: "192.168.31.1/30", fail: true, want: "client on a", bench: 1},
	} {
		t.Run(name, func(t *testing.T) {
			ns, p := newPair(tc.mtuB, tc.ipB0)
			if tc.slow {
				p.a.gbps = 40
			}
			p.a.failing, p.b.waiting = tc.fail, tc.fail
			res := CheckFabric(context.Background(), ns, pairCluster(), FabricOptions{Bandwidth: true}, &bytes.Buffer{})
			if res[0].OK() || !strings.Contains(strings.Join(res[0].Problems, "; "), tc.want) {
				t.Fatalf("problems %q, want %q", res[0].Problems, tc.want)
			}
			if len(p.a.requests) != tc.bench {
				t.Fatalf("%d bandwidth runs, want %d", len(p.a.requests), tc.bench)
			}
		})
	}

	// Without --bandwidth only the configuration is checked.
	ns, p := newPair(9000, "192.168.31.1/30")
	if res := CheckFabric(context.Background(), ns, pairCluster(), FabricOptions{}, &bytes.Buffer{}); !res[0].OK() || len(p.a.requests) != 0 {
		t.Fatalf("config-only check: %+v, %d runs", res, len(p.a.requests))
	}
	c := pairCluster()
	c.Spec.Fabric.Links[0].B.Port = nil
	if res := CheckFabric(context.Background(), ns, c, FabricOptions{}, &bytes.Buffer{}); res[0].OK() || !strings.Contains(res[0].Problems[0], "cabling discovery") {
		t.Fatalf("a link without a port: %+v", res)
	}
}
