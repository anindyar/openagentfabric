package reconcile

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/node"
)

// Fabric defaults, as in the cluster schema.
const (
	DefaultMTU             = 9000
	DefaultMinGbpsPerRail  = 85.0
	DefaultMinGbpsDualRail = 170.0
	DefaultBandwidthTime   = 5 * time.Second
	bandwidthQueuePairs    = 4
)

// FabricOptions control CheckFabric.
type FabricOptions struct {
	// Bandwidth runs ib_write_bw on each rail and on both rails together.
	Bandwidth bool
	// Duration of each bandwidth run; zero means DefaultBandwidthTime.
	Duration time.Duration
}

// LinkResult is the check of one cable.
type LinkResult struct {
	Name     string // "spark-1:0 ↔ spark-2:0"
	Problems []string
	Rails    []RailResult
	// DualGbps is the sum over all rails run together; 0 when not measured.
	DualGbps                float64
	MinGbpsPerRail, MinDual float64
}

// OK reports whether the link passed.
func (l LinkResult) OK() bool { return len(l.Problems) == 0 }

// RailResult is one rail: both ends and, when measured, its bandwidth.
type RailResult struct {
	A, B fabric.Rail
	Gbps float64
}

// CheckFabric verifies every link in fabric.links without changing anything
// on the nodes: carrier, MTU, a shared subnet per rail and a RoCE v2 GID for
// each rail address. With opts.Bandwidth it then measures each rail and both
// rails together against fabric.acceptance. A link's B end runs the server.
func CheckFabric(ctx context.Context, ns Nodes, c *config.Cluster, opts FabricOptions, progress io.Writer) []LinkResult {
	f := c.Spec.Fabric
	mtu := f.MTU
	if mtu == 0 {
		mtu = DefaultMTU
	}
	perRail, dual := DefaultMinGbpsPerRail, DefaultMinGbpsDualRail
	if a := f.Acceptance; a != nil {
		if a.MinGbpsPerRail != nil {
			perRail = *a.MinGbpsPerRail
		}
		if a.MinGbpsDualRail != nil {
			dual = *a.MinGbpsDualRail
		}
	}
	if opts.Duration == 0 {
		opts.Duration = DefaultBandwidthTime
	}

	discovered := map[string][]fabric.Port{}
	discover := func(n string) ([]fabric.Port, error) {
		if p, ok := discovered[n]; ok {
			return p, nil
		}
		cl, err := ns.client(n)
		if err != nil {
			return nil, err
		}
		resp, err := cl.Fabric(ctx, &nodev1.FabricRequest{})
		if err != nil {
			return nil, fmt.Errorf("fabric discovery on %s: %w", n, err)
		}
		p, err := node.FabricFromProto(resp.GetPorts())
		if err == nil {
			discovered[n] = p
		}
		return p, err
	}

	var out []LinkResult
	for i, l := range f.Links {
		res := LinkResult{Name: linkName(l), MinGbpsPerRail: perRail, MinDual: dual}
		ends, err := resolveLink(l, discover, f.RoCE.GIDIndex)
		if err != nil {
			res.Problems = append(res.Problems, err.Error())
			out = append(out, res)
			continue
		}
		a, b := ends[0], ends[1]
		if len(a.Rails) != len(b.Rails) {
			res.Problems = append(res.Problems, fmt.Sprintf("%d rails on %s but %d on %s", len(a.Rails), a.Node, len(b.Rails), b.Node))
		}
		for h := 0; h < min(len(a.Rails), len(b.Rails)); h++ {
			ra, rb := a.Rails[h], b.Rails[h]
			res.Rails = append(res.Rails, RailResult{A: ra, B: rb})
			for _, e := range []struct {
				node string
				r    fabric.Rail
			}{{a.Node, ra}, {b.Node, rb}} {
				if e.r.MTU != mtu {
					res.Problems = append(res.Problems, fmt.Sprintf("%s on %s has MTU %d, want %d", e.r.Netdev, e.node, e.r.MTU, mtu))
				}
				if opts.Bandwidth && (e.r.RDMADev == "" || e.r.GID < 0) {
					res.Problems = append(res.Problems, fmt.Sprintf("%s on %s has no RDMA device or RoCE v2 GID for %s", e.r.Netdev, e.node, e.r.Addr.Addr()))
				}
			}
			if ra.Addr.Masked() != rb.Addr.Masked() {
				res.Problems = append(res.Problems, fmt.Sprintf("rail %d: %s (%s) and %s (%s) are not in one subnet", h, ra.Addr, a.Node, rb.Addr, b.Node))
			}
		}
		if opts.Bandwidth && res.OK() {
			measure(ctx, ns, &res, a.Node, b.Node, i, opts.Duration, progress)
		}
		out = append(out, res)
	}
	return out
}

func linkName(l config.Link) string {
	end := func(e config.LinkEnd) string {
		if e.Port == nil {
			return e.Node + ":?"
		}
		return fmt.Sprintf("%s:%d", e.Node, *e.Port)
	}
	return end(l.A) + " ↔ " + end(l.B)
}

func resolveLink(l config.Link, discover func(string) ([]fabric.Port, error), gidIndex string) ([2]fabric.End, error) {
	var ends [2]fabric.End
	for i, e := range []config.LinkEnd{l.A, l.B} {
		if e.Port == nil {
			return ends, fmt.Errorf("the link end on %s has no port; run cabling discovery to fill in fabric.links", e.Node)
		}
		ports, err := discover(e.Node)
		if err != nil {
			return ends, err
		}
		if ends[i], err = fabric.ResolveEnd(e.Node, ports, *e.Port, l.Subnets, gidIndex); err != nil {
			return ends, err
		}
	}
	return ends, nil
}

// measure runs each rail alone, then all rails at once.
func measure(ctx context.Context, ns Nodes, res *LinkResult, aNode, bNode string, link int, d time.Duration, progress io.Writer) {
	run := func(h int) (float64, error) {
		r := res.Rails[h]
		return railBandwidth(ctx, ns, aNode, bNode, r, fabric.BandwidthPortMin+2*link+h, d)
	}
	for h, r := range res.Rails {
		gbps, err := run(h)
		if err != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("rail %d: %v", h, err))
			return
		}
		res.Rails[h].Gbps = gbps
		_, _ = fmt.Fprintf(progress, "%s  rail %d  %s → %s  %.1f Gb/s\n", res.Name, h, r.A.Addr.Addr(), r.B.Addr.Addr(), gbps)
		if gbps < res.MinGbpsPerRail {
			res.Problems = append(res.Problems, fmt.Sprintf("rail %d: %.1f Gb/s is below %.0f", h, gbps, res.MinGbpsPerRail))
		}
	}
	if len(res.Rails) < 2 {
		return
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		total float64
		errs  []string
	)
	for h := range res.Rails {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gbps, err := run(h)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Sprintf("rails together, rail %d: %v", h, err))
			}
			total += gbps
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		res.Problems = append(res.Problems, errs...)
		return
	}
	res.DualGbps = total
	_, _ = fmt.Fprintf(progress, "%s  %d rails together  %.1f Gb/s\n", res.Name, len(res.Rails), total)
	if total < res.MinDual {
		res.Problems = append(res.Problems, fmt.Sprintf("rails together: %.1f Gb/s is below %.0f", total, res.MinDual))
	}
}

// railBandwidth runs the server on b and the client on a. A failed client
// cancels the waiting server.
func railBandwidth(ctx context.Context, ns Nodes, aNode, bNode string, r RailResult, port int, d time.Duration) (float64, error) {
	ca, err := ns.client(aNode)
	if err != nil {
		return 0, err
	}
	cb, err := ns.client(bNode)
	if err != nil {
		return 0, err
	}
	req := func(rail fabric.Rail, server string) *nodev1.RailBandwidthRequest {
		return &nodev1.RailBandwidthRequest{
			RdmaDev: rail.RDMADev, GidIndex: int32(rail.GID), TcpPort: int32(port),
			DurationSeconds: int32(d / time.Second), QueuePairs: bandwidthQueuePairs, ServerAddr: server,
		}
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srvErr := make(chan error, 1)
	go func() {
		_, err := cb.RailBandwidth(sctx, req(r.B, ""))
		srvErr <- err
	}()
	resp, err := ca.RailBandwidth(ctx, req(r.A, r.B.Addr.Addr().String()))
	if err != nil {
		cancel()
		// Wait, so the exchange port is free for the next run.
		if serr := <-srvErr; serr != nil && connect.CodeOf(serr) != connect.CodeCanceled {
			return 0, fmt.Errorf("client on %s: %w (server on %s: %w)", aNode, err, bNode, serr)
		}
		return 0, fmt.Errorf("client on %s: %w", aNode, err)
	}
	if err := <-srvErr; err != nil {
		return 0, fmt.Errorf("server on %s: %w", bNode, err)
	}
	return resp.GetGbps(), nil
}
