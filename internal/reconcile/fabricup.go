package reconcile

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/node"
)

// Fabric bring-up (backlog G1, G3). `oaf apply` gives every rail of every
// link in fabric.links its address through the node agents, then checks
// each rail: ping, a ping at the full MTU with fragmentation off, and, on
// a link it just brought up, ib_write_bw per rail.

// railPlan is one rail of a link as both ends get it.
type railPlan struct {
	link       int
	half       int
	aNode      string
	bNode      string
	aDev, bDev string
	aAddr      netip.Prefix
	bAddr      netip.Prefix
}

// endHosts are the addresses of a link's two ends in one /30: the first and
// second host.
func endHosts(subnet string) (netip.Prefix, netip.Prefix, error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil {
		return netip.Prefix{}, netip.Prefix{}, fmt.Errorf("fabric subnet %q: %w", subnet, err)
	}
	p = p.Masked()
	if !p.Addr().Is4() || p.Bits() > 30 {
		return netip.Prefix{}, netip.Prefix{}, fmt.Errorf("fabric subnet %q must be IPv4 with room for two hosts", subnet)
	}
	a := p.Addr().Next()
	b := a.Next()
	return netip.PrefixFrom(a, p.Bits()), netip.PrefixFrom(b, p.Bits()), nil
}

// manualLink reports whether a link's addresses are set outside OAF: a link
// without subnets (clusters set up by hand before OAF addressed the fabric).
func manualLink(l config.Link) bool { return len(l.Subnets) == 0 }

// manualNodes are nodes on a hand-addressed link: OAF neither writes nor
// removes their netplan file.
func manualNodes(c *config.Cluster) map[string]bool {
	out := map[string]bool{}
	for _, l := range c.Spec.Fabric.Links {
		if manualLink(l) {
			out[l.A.Node], out[l.B.Node] = true, true
		}
	}
	return out
}

// planRails resolves fabric.links to netdevs and addresses. A link needs one
// subnet per rail (half) of its ports; hand-addressed links are skipped.
func planRails(c *config.Cluster, ports map[string][]fabric.Port) ([]railPlan, error) {
	halves := func(node string, port int) ([]string, error) {
		for _, p := range ports[node] {
			if p.Index == port {
				var out []string
				for _, h := range p.Halves {
					out = append(out, h.Name)
				}
				return out, nil
			}
		}
		return nil, fmt.Errorf("node %s has no QSFP port %d", node, port)
	}
	var out []railPlan
	for i, l := range c.Spec.Fabric.Links {
		if manualLink(l) {
			continue
		}
		if l.A.Port == nil || l.B.Port == nil {
			return nil, fmt.Errorf("link %s has no port on one end", linkName(l))
		}
		ha, err := halves(l.A.Node, *l.A.Port)
		if err != nil {
			return nil, err
		}
		hb, err := halves(l.B.Node, *l.B.Port)
		if err != nil {
			return nil, err
		}
		n := min(len(ha), len(hb))
		if len(l.Subnets) < n {
			return nil, fmt.Errorf("link %s has %d rails but %d subnets", linkName(l), n, len(l.Subnets))
		}
		for h := 0; h < n; h++ {
			a, b, err := endHosts(l.Subnets[h])
			if err != nil {
				return nil, err
			}
			out = append(out, railPlan{link: i, half: h, aNode: l.A.Node, bNode: l.B.Node, aDev: ha[h], bDev: hb[h], aAddr: a, bAddr: b})
		}
	}
	return out, nil
}

// FabricUp gives each node exactly the rails fabric.links assigns it (nodes
// with none have OAF's netplan file removed), then checks every rail. It
// reports whether anything changed. A rail that does not answer a ping is
// an error: models spanning that link could not run.
func FabricUp(ctx context.Context, ns Nodes, c *config.Cluster, dryRun bool, progress io.Writer) (bool, error) {
	ports := map[string][]fabric.Port{}
	for _, l := range c.Spec.Fabric.Links {
		if manualLink(l) {
			continue
		}
		for _, n := range []string{l.A.Node, l.B.Node} {
			if _, ok := ports[n]; ok {
				continue
			}
			cl, err := ns.client(n)
			if err != nil {
				return false, err
			}
			resp, err := cl.Fabric(ctx, &nodev1.FabricRequest{})
			if err != nil {
				return false, fmt.Errorf("fabric discovery on %s: %w", n, err)
			}
			p, err := node.FabricFromProto(resp.GetPorts())
			if err != nil {
				return false, err
			}
			ports[n] = p
		}
	}
	rails, err := planRails(c, ports)
	if err != nil {
		return false, err
	}
	byNode := map[string][]*nodev1.FabricRail{}
	mtu := c.Spec.Fabric.MTU
	if mtu == 0 {
		mtu = DefaultMTU
	}
	for _, r := range rails {
		byNode[r.aNode] = append(byNode[r.aNode], &nodev1.FabricRail{Netdev: r.aDev, Address: r.aAddr.String(), Mtu: int32(mtu)})
		byNode[r.bNode] = append(byNode[r.bNode], &nodev1.FabricRail{Netdev: r.bDev, Address: r.bAddr.String(), Mtu: int32(mtu)})
	}
	names := make([]string, 0, len(c.Spec.Nodes))
	for _, n := range c.Spec.Nodes {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	changed := false
	manual := manualNodes(c)
	for _, n := range names {
		if manual[n] {
			continue
		}
		cl, err := ns.client(n)
		if err != nil {
			return changed, err
		}
		resp, err := cl.ConfigureFabric(ctx, &nodev1.ConfigureFabricRequest{Rails: byNode[n], DryRun: dryRun})
		if err != nil {
			// An older agent cannot configure the fabric; that only matters
			// for a node that has rails.
			if connect.CodeOf(err) == connect.CodeUnimplemented && len(byNode[n]) == 0 {
				continue
			}
			if connect.CodeOf(err) == connect.CodeUnimplemented {
				return changed, fmt.Errorf("%s: its agent is too old to configure the fabric; upgrade oaf on it", n)
			}
			return changed, fmt.Errorf("%s: fabric: %w", n, err)
		}
		if len(byNode[n]) == 0 && !resp.GetChanged() {
			continue // no rails and nothing to remove: nothing to say
		}
		verb := "unchanged"
		if resp.GetChanged() {
			changed = true
			verb = "update   "
			if !dryRun {
				verb = "updated  "
			}
		}
		var rs []string
		for _, r := range resp.GetRails() {
			rs = append(rs, r.GetNetdev()+" "+r.GetAddress())
		}
		desc := strings.Join(rs, ", ")
		if desc == "" {
			desc = "no rails (OAF's netplan file removed)"
		}
		_, _ = fmt.Fprintf(progress, "%s %s/fabric  %s\n", verb, n, desc)
		for _, w := range resp.GetWarnings() {
			_, _ = fmt.Fprintf(progress, "warning: %s: fabric: %s\n", n, w)
		}
	}
	if dryRun || len(rails) == 0 {
		return changed, nil
	}
	if err := CheckRails(ctx, ns, rails, mtu, progress); err != nil {
		return changed, err
	}
	if changed {
		// A link just brought up is measured once (fabric.acceptance); a slow
		// rail is a warning, not a failure: models still run, only slower.
		for _, res := range CheckFabric(ctx, ns, c, FabricOptions{Bandwidth: true}, progress) {
			for _, p := range res.Problems {
				_, _ = fmt.Fprintf(progress, "warning: link %s: %s\n", res.Name, p)
			}
		}
	}
	return changed, nil
}

// railRetry is how long CheckRails waits between pings of a rail that is
// still coming up (five tries).
var railRetry = 2 * time.Second

// CheckRails pings every rail from its A end, then again at the full MTU
// with fragmentation off. Output lines start with "link" (the console's
// wizard shows them).
func CheckRails(ctx context.Context, ns Nodes, rails []railPlan, mtu int, progress io.Writer) error {
	var bad []string
	for _, r := range rails {
		cl, err := ns.client(r.aNode)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%s:%s ↔ %s:%s", r.aNode, r.aDev, r.bNode, r.bDev)
		var resp *nodev1.PingResponse
		for try := 0; try < 5; try++ { // the rail may still be coming up
			resp, err = cl.Ping(ctx, &nodev1.PingRequest{Netdev: r.aDev, Target: r.bAddr.Addr().String(), Count: 2})
			if err == nil && resp.GetOk() {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(railRetry):
			}
		}
		if err != nil || !resp.GetOk() {
			_, _ = fmt.Fprintf(progress, "link  %s  no answer from %s\n", name, r.bAddr.Addr())
			bad = append(bad, name)
			continue
		}
		jumbo, err := cl.Ping(ctx, &nodev1.PingRequest{Netdev: r.aDev, Target: r.bAddr.Addr().String(), Count: 2, Size: int32(mtu - 28), DontFragment: true})
		mtuOK := err == nil && jumbo.GetOk()
		note := fmt.Sprintf("MTU %d ok", mtu)
		if !mtuOK {
			note = fmt.Sprintf("MTU %d FAILS (set it on both ends)", mtu)
			bad = append(bad, name+" (MTU)")
		}
		_, _ = fmt.Fprintf(progress, "link  %s  %.2f ms, %s\n", name, resp.GetRttMs(), note)
	}
	if len(bad) > 0 {
		return fmt.Errorf("fabric check failed on %s", strings.Join(bad, ", "))
	}
	return nil
}

// --- cabling probe (backlog G2) ---------------------------------------------------

// ProbePool is where cabling probes take their temporary addresses: the
// RFC 2544 benchmarking range, never routed, so it cannot clash with a LAN.
var ProbePool = netip.MustParsePrefix("198.18.0.0/24")

// ProbedCable is one cable a probe found.
type ProbedCable struct {
	ANode string
	APort int
	BNode string
	BPort int
}

// ProbeCabling finds which QSFP port of which box is cabled to which, among
// boxes, without leaving anything behind:
//
//  1. every up QSFP netdev that is free (or carries only OAF's own
//     addresses) gets a temporary address from ProbePool, all in one /24;
//  2. each such netdev pings every other box's probe addresses; on a
//     point-to-point cable only the peer port can answer ARP there, so the
//     MAC that answered names the peer netdev, hence the peer port;
//  3. the probe addresses are removed (also on error).
//
// A box whose agent cannot probe is skipped. Cables to a box outside nodes
// are not seen.
func ProbeCabling(ctx context.Context, ns Nodes, nodes []string) ([]ProbedCable, error) {
	type rail struct {
		node, dev string
		port      int
		addr      netip.Prefix
	}
	byMAC := map[string]rail{}
	var probes []rail
	next := ProbePool.Masked().Addr()
	for _, n := range nodes {
		cl, err := ns.client(n)
		if err != nil {
			return nil, err
		}
		resp, err := cl.Fabric(ctx, &nodev1.FabricRequest{})
		if err != nil {
			return nil, fmt.Errorf("fabric discovery on %s: %w", n, err)
		}
		ports, err := node.FabricFromProto(resp.GetPorts())
		if err != nil {
			return nil, err
		}
		for _, p := range ports {
			for _, h := range p.Halves {
				if h.MAC != "" {
					byMAC[strings.ToLower(h.MAC)] = rail{node: n, dev: h.Name, port: p.Index}
				}
				if h.OperState != "up" {
					continue
				}
				next = next.Next()
				if !ProbePool.Contains(next) {
					return nil, fmt.Errorf("probe pool %s is too small", ProbePool)
				}
				probes = append(probes, rail{node: n, dev: h.Name, port: p.Index, addr: netip.PrefixFrom(next, ProbePool.Bits())})
			}
		}
	}
	var added []rail
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, r := range added {
			if cl, err := ns.client(r.node); err == nil {
				_, _ = cl.ProbeAddress(cctx, &nodev1.ProbeAddressRequest{Netdev: r.dev, Address: r.addr.String(), Remove: true})
			}
		}
	}()
	for _, r := range probes {
		cl, _ := ns.client(r.node)
		if _, err := cl.ProbeAddress(ctx, &nodev1.ProbeAddressRequest{Netdev: r.dev, Address: r.addr.String()}); err != nil {
			continue // not free (something else configured it), or no host changes
		}
		added = append(added, r)
	}
	type key struct {
		a, b string
		ap   int
		bp   int
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		found = map[key]bool{}
	)
	for _, from := range added {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl, _ := ns.client(from.node)
			for _, to := range added {
				if to.node == from.node {
					continue
				}
				resp, err := cl.Ping(ctx, &nodev1.PingRequest{Netdev: from.dev, Target: to.addr.Addr().String(), Count: 1})
				if err != nil || resp.GetNeighborMac() == "" {
					continue
				}
				peer, ok := byMAC[resp.GetNeighborMac()]
				if !ok || peer.node == from.node {
					continue
				}
				k := key{a: from.node, ap: from.port, b: peer.node, bp: peer.port}
				if k.a > k.b {
					k = key{a: k.b, ap: k.bp, b: k.a, bp: k.ap}
				}
				mu.Lock()
				found[k] = true
				mu.Unlock()
				break // this rail's peer is known
			}
		}()
	}
	wg.Wait()
	out := make([]ProbedCable, 0, len(found))
	for k := range found {
		out = append(out, ProbedCable{ANode: k.a, APort: k.ap, BNode: k.b, BPort: k.bp})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ANode != out[j].ANode {
			return out[i].ANode < out[j].ANode
		}
		return out[i].APort < out[j].APort
	})
	return out, nil
}
