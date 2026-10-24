// Package topology recommends how to grow a cluster with the boxes that
// joined (ADR-0002 §5) and turns the chosen option into a cluster.yaml plan.
//
// Options, for a single box today:
//
//	a) pair:             +1 box over ConnectX-7; both form a TP=2 group.
//	c) pair-plus-single: +2 boxes cabled to each other; this box stays single
//	                     on the LAN, a large model runs on the new pair, and
//	                     complex queries route to it.
//	b) ha:               +1 box as a fully redundant 1:1 appliance (ADR §6):
//	                     described here, planned by the HA milestone.
package topology

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/join"
)

// Option IDs.
const (
	OptionPair           = "pair"
	OptionPairPlusSingle = "pair-plus-single"
	OptionHA             = "ha"
)

// Cable is one QSFP cable between two boxes, observed or declared.
type Cable struct {
	A config.LinkEnd `json:"a"`
	B config.LinkEnd `json:"b"`
}

// Input is what a recommendation is made from.
type Input struct {
	Cluster *config.Cluster
	Catalog *config.Catalog
	Pending []join.PendingNode
	// Current is the hardware of boxes already in the cluster, by node name
	// (from their agents); optional, but needed to pick free ports.
	Current map[string]join.Hardware
	// Cabling is what is cabled already (ObservedCables, or declared).
	Cabling []Cable
}

// ModelFit is a catalog model the option can serve.
type ModelFit struct {
	CatalogRef           string   `json:"catalog_ref"`
	DisplayName          string   `json:"display_name"`
	Profile              string   `json:"profile"`
	Nodes                int      `json:"nodes"`
	Classes              []string `json:"classes"`
	SizeGiB              float64  `json:"size_gib"`
	GPUMemoryUtilization float64  `json:"gpu_memory_utilization"`
	Where                string   `json:"where"` // node name, or "pair a+b"
}

// CableStep is one cable the admin connects (or that is connected).
type CableStep struct {
	A    config.LinkEnd `json:"a"`
	B    config.LinkEnd `json:"b"`
	Text string         `json:"text"`
	Done bool           `json:"done"`
}

// Option is one way to grow the cluster.
type Option struct {
	ID             string      `json:"id"`
	Letter         string      `json:"letter"`
	Title          string      `json:"title"`
	Summary        string      `json:"summary"`
	Possible       bool        `json:"possible"`
	Plannable      bool        `json:"plannable"`
	Recommended    bool        `json:"recommended"`
	Topology       string      `json:"topology,omitempty"`
	NewNodes       []string    `json:"new_nodes"`
	Nodes          []string    `json:"nodes"` // every node after the change
	TotalMemoryGiB float64     `json:"total_memory_gib"`
	Cabling        []CableStep `json:"cabling,omitempty"`
	Models         []ModelFit  `json:"models,omitempty"`
	Routing        []string    `json:"routing,omitempty"`
	Notes          []string    `json:"notes,omitempty"` // caveats, or why it is not possible
}

func memOf(hw join.Hardware) float64 {
	if hw.MemTotalGiB > 0 {
		return hw.MemTotalGiB
	}
	return config.GB10MemoryGiB
}

// usedFraction is the share of a node's unified memory its enabled
// deployments take, with vLLM's per-instance overhead (as validation counts it).
func usedFraction(c *config.Cluster, node string) float64 {
	used := 0.0
	for _, d := range c.Spec.Models {
		if !d.IsEnabled() {
			continue
		}
		for _, n := range c.PlacementNodes(d.Placement) {
			if n == node {
				used += d.GPUMemoryUtilization + config.VLLMInstanceOverhead
			}
		}
	}
	return used
}

// fits lists catalog models with a profile for n nodes that fit in free
// (a fraction of each node's memory), largest first. Entitled and embedding
// models are left out: they are added deliberately, not recommended.
func fits(cat *config.Catalog, n int, free float64, where string) []ModelFit {
	var out []ModelFit
	for _, m := range cat.Models {
		if m.Entitlement != nil || m.IsEmbedding() {
			continue
		}
		for _, p := range m.Profiles {
			if p.Nodes != n || p.GPUMemoryUtilization+config.VLLMInstanceOverhead > free+1e-9 {
				continue
			}
			out = append(out, ModelFit{CatalogRef: m.ID, DisplayName: m.DisplayName, Profile: p.Name, Nodes: n,
				Classes: m.Classes, SizeGiB: m.SizeGiB, GPUMemoryUtilization: p.GPUMemoryUtilization, Where: where})
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SizeGiB > out[j].SizeGiB })
	return out
}

// largest is the biggest reasoning-class model in fs, else the biggest.
func largest(fs []ModelFit) (ModelFit, bool) {
	for _, f := range fs {
		for _, c := range f.Classes {
			if c == "think" {
				return f, true
			}
		}
	}
	if len(fs) > 0 {
		return fs[0], true
	}
	return ModelFit{}, false
}

func freePorts(hw join.Hardware, known bool) []int {
	if !known {
		return []int{1, 0} // unknown hardware: assume both, prefer port 1
	}
	var out []int
	for _, i := range []int{1, 0} {
		for _, p := range hw.Ports {
			if p.Index == i && !p.InUse() {
				out = append(out, i)
			}
		}
	}
	return out
}

func intp(i int) *int { return &i }

// cableBetween finds an observed cable between two boxes.
func cableBetween(cs []Cable, a, b string) (Cable, bool) {
	for _, c := range cs {
		if c.A.Node == a && c.B.Node == b {
			return c, true
		}
		if c.A.Node == b && c.B.Node == a {
			return Cable{A: c.B, B: c.A}, true
		}
	}
	return Cable{}, false
}

// cablePlan picks the ports for a pair cable: what is cabled already, else a
// free port on each box, port 1 first (port 0 is often in use).
func cablePlan(in Input, a, b string) (CableStep, string) {
	if c, ok := cableBetween(in.Cabling, a, b); ok && c.A.Port != nil && c.B.Port != nil {
		return CableStep{A: c.A, B: c.B, Done: true, Text: fmt.Sprintf("%s port %d ↔ %s port %d (cabled)", a, *c.A.Port, b, *c.B.Port)}, ""
	}
	hwA, okA := hardwareOf(in, a)
	hwB, okB := hardwareOf(in, b)
	// A box that has not reported its ports (not joined yet) is unknown.
	okA, okB = okA && len(hwA.Ports) > 0, okB && len(hwB.Ports) > 0
	pa, pb := freePorts(hwA, okA), freePorts(hwB, okB)
	if len(pa) == 0 {
		return CableStep{}, fmt.Sprintf("%s has no free QSFP port (both carry addresses something else configured)", a)
	}
	if len(pb) == 0 {
		return CableStep{}, fmt.Sprintf("%s has no free QSFP port (both carry addresses something else configured)", b)
	}
	return CableStep{A: config.LinkEnd{Node: a, Port: intp(pa[0])}, B: config.LinkEnd{Node: b, Port: intp(pb[0])},
		Text: fmt.Sprintf("connect %s port %d ↔ %s port %d with one QSFP cable (one cable = two rails)", a, pa[0], b, pb[0])}, ""
}

func hardwareOf(in Input, name string) (join.Hardware, bool) {
	for _, p := range in.Pending {
		if p.Name == name {
			return p.Hardware, true
		}
	}
	hw, ok := in.Current[name]
	return hw, ok
}

func nodeNames(c *config.Cluster) []string {
	out := make([]string, 0, len(c.Spec.Nodes))
	for _, n := range c.Spec.Nodes {
		out = append(out, n.Name)
	}
	return out
}

// Recommend lists every option with whether it is possible for these boxes.
func Recommend(in Input) []Option {
	c := in.Cluster
	cur := nodeNames(c)
	single := c.Spec.Fabric.Topology == config.TopologySingle && len(cur) == 1
	var pend []string
	for _, p := range in.Pending {
		pend = append(pend, p.Name)
	}
	memory := func(names ...string) float64 {
		t := 0.0
		for _, n := range names {
			hw, _ := hardwareOf(in, n)
			t += memOf(hw)
		}
		return float64(int(t*10+0.5)) / 10
	}

	// a) +1 box over ConnectX-7: this box and the new one become a pair.
	a := Option{ID: OptionPair, Letter: "a", Title: "Add 1 box over ConnectX-7 (pair)", Plannable: true, Topology: config.TopologyPair,
		Summary: "This box and the new one become a pair joined by one QSFP cable (two rails, ~185 Gbps). Models too large for one box run across both with tensor parallelism."}
	switch {
	case !single:
		a.Notes = append(a.Notes, "only a single box can become a pair; this cluster is "+c.Spec.Fabric.Topology)
	case len(pend) < 1:
		a.Notes = append(a.Notes, "needs 1 joined box (run the join command on it first)")
	default:
		a.NewNodes = pend[:1]
		a.Nodes = append(append([]string{}, cur...), pend[0])
		a.TotalMemoryGiB = memory(a.Nodes...)
		step, why := cablePlan(in, cur[0], pend[0])
		if why != "" {
			a.Notes = append(a.Notes, why)
			break
		}
		a.Possible = true
		a.Cabling = []CableStep{step}
		free := config.MaxNodeMemoryFraction - usedFraction(c, cur[0])
		where := "pair " + cur[0] + "+" + pend[0]
		a.Models = fits(in.Catalog, 2, free, where)
		if big, ok := largest(a.Models); ok {
			a.Routing = []string{fmt.Sprintf("complex and reasoning queries → %s on the pair (vSR)", big.DisplayName),
				fmt.Sprintf("simple queries → %s (unchanged)", c.Spec.Routing.Defaults.Fast)}
		} else {
			a.Notes = append(a.Notes, fmt.Sprintf("%s's models already use %.0f%% of its memory, so no pair model fits next to them; shrink or move them first, or choose option c", cur[0], 100*usedFraction(c, cur[0])))
		}
	}

	// b) +1 box as a 1:1 redundant appliance.
	b := Option{ID: OptionHA, Letter: "b", Title: "Add 1 box over LAN (high availability, 1:1)", Plannable: false,
		Summary: "A second complete appliance with the same models takes over when this one fails. Users fail over over the LAN, usually through an external load balancer that follows each box's health endpoint."}
	b.Notes = append(b.Notes, "planned by the HA milestone (ADR-0002 §6); not available yet")
	if len(pend) >= 1 {
		b.Possible = true
		b.NewNodes = pend[:1]
		b.TotalMemoryGiB = memory(append(append([]string{}, cur...), pend[0])...)
	} else {
		b.Notes = append(b.Notes, "needs 1 joined box")
	}

	// c) +1 pair: two new boxes cabled to each other; this box stays single.
	cc := Option{ID: OptionPairPlusSingle, Letter: "c", Title: "Add a pair (2 boxes over ConnectX-7)", Plannable: true, Topology: config.TopologyPairPlusSingle,
		Summary: "Two new boxes are cabled to each other and reach this box over the LAN. A small model stays here; a large model runs on the pair; complex queries are routed to the pair automatically and simple ones stay local."}
	switch {
	case !single:
		cc.Notes = append(cc.Notes, "a pair is added next to a single box; this cluster is "+c.Spec.Fabric.Topology)
	case len(pend) < 2:
		cc.Notes = append(cc.Notes, fmt.Sprintf("needs 2 joined boxes (%d joined)", len(pend)))
	default:
		cc.NewNodes = pend[:2]
		cc.Nodes = append(append([]string{}, cur...), pend[0], pend[1])
		cc.TotalMemoryGiB = memory(cc.Nodes...)
		step, why := cablePlan(in, pend[0], pend[1])
		if why != "" {
			cc.Notes = append(cc.Notes, why)
			break
		}
		cc.Possible = true
		cc.Cabling = []CableStep{step}
		cc.Models = fits(in.Catalog, 2, config.MaxNodeMemoryFraction, "pair "+pend[0]+"+"+pend[1])
		if big, ok := largest(cc.Models); ok {
			cc.Routing = []string{fmt.Sprintf("complex and reasoning queries → %s on the pair (vSR)", big.DisplayName),
				fmt.Sprintf("simple queries → %s on %s (unchanged)", c.Spec.Routing.Defaults.Fast, cur[0])}
		}
		for _, n := range cc.NewNodes {
			hw, _ := hardwareOf(in, n)
			if big, ok := largest(cc.Models); ok && hw.DiskFreeGiB > 0 && hw.DiskFreeGiB < big.SizeGiB*1.1 {
				cc.Notes = append(cc.Notes, fmt.Sprintf("%s has %.0f GiB free; %s needs %.0f GiB of weights on each box of the pair", n, hw.DiskFreeGiB, big.DisplayName, big.SizeGiB))
			}
		}
	}
	for _, n := range append(append([]string{}, a.NewNodes...), cc.NewNodes...) {
		if hw, ok := hardwareOf(in, n); ok && hw.GPU != "" && !hw.GB10() {
			note := fmt.Sprintf("%s has %s, not a GB10", n, hw.GPU)
			a.Notes, cc.Notes = append(a.Notes, note), append(cc.Notes, note)
		}
	}

	switch {
	case cc.Possible && len(cc.Models) > 0:
		cc.Recommended = true
	case a.Possible && len(a.Models) > 0:
		a.Recommended = true
	}
	return []Option{a, b, cc}
}

// Choice is the admin's pick.
type Choice struct {
	Option string `json:"option"`
	// Model is the catalog model to deploy on the pair (default: the
	// largest that fits) and Name its served name (default: its id).
	Model string `json:"model,omitempty"`
	Name  string `json:"name,omitempty"`
	// NoModel adds the boxes and the fabric only.
	NoModel bool `json:"no_model,omitempty"`
}

// ruleName makes a valid rule name (a DNS label) from a served model name.
func ruleName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// SubnetBase is where rail subnets are allocated, /30 per rail.
var SubnetBase = netip.MustParsePrefix("10.200.0.0/16")

func allocSubnets(c *config.Cluster, n int) ([]string, error) {
	var used []netip.Prefix
	for _, l := range c.Spec.Fabric.Links {
		for _, s := range l.Subnets {
			if p, err := netip.ParsePrefix(s); err == nil {
				used = append(used, p.Masked())
			}
		}
	}
	var out []string
	addr := SubnetBase.Masked().Addr()
	for len(out) < n {
		p := netip.PrefixFrom(addr, 30)
		if !SubnetBase.Contains(addr) {
			return nil, fmt.Errorf("no free /30 left in %s", SubnetBase)
		}
		clash := false
		for _, u := range used {
			if u.Overlaps(p) {
				clash = true
				break
			}
		}
		if !clash {
			out = append(out, p.String())
			used = append(used, p)
		}
		a4 := addr.As4()
		v := uint32(a4[0])<<24 | uint32(a4[1])<<16 | uint32(a4[2])<<8 | uint32(a4[3])
		v += 4
		addr = netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	}
	return out, nil
}

func uniqueGroup(c *config.Cluster) string {
	taken := map[string]bool{}
	for _, g := range c.Spec.Groups {
		taken[g.Name] = true
	}
	for i := 1; ; i++ {
		if n := fmt.Sprintf("pair-%d", i); !taken[n] {
			return n
		}
	}
}

// Plan turns a possible, plannable option into an edited cluster.yaml,
// validated against the catalog. The pending boxes become inference nodes.
func Plan(data []byte, in Input, ch Choice) ([]byte, config.Report, error) {
	var opt *Option
	for _, o := range Recommend(in) {
		if o.ID == ch.Option {
			o := o
			opt = &o
		}
	}
	switch {
	case opt == nil:
		return nil, config.Report{}, fmt.Errorf("unknown option %q", ch.Option)
	case !opt.Plannable:
		return nil, config.Report{}, fmt.Errorf("option %s cannot be planned yet: %s", opt.ID, strings.Join(opt.Notes, "; "))
	case !opt.Possible:
		return nil, config.Report{}, fmt.Errorf("option %s is not possible: %s", opt.ID, strings.Join(opt.Notes, "; "))
	}
	c := in.Cluster
	e, err := config.NewEditor(data)
	if err != nil {
		return nil, config.Report{}, err
	}
	for _, name := range opt.NewNodes {
		var addr string
		for _, p := range in.Pending {
			if p.Name == name {
				addr = p.Address
			}
		}
		if err := e.Append(config.Node{Name: name, Address: addr, Roles: []string{config.RoleInference}}, "nodes"); err != nil {
			return nil, config.Report{}, err
		}
	}
	if err := e.Set(opt.Topology, "fabric", "topology"); err != nil {
		return nil, config.Report{}, err
	}
	if c.Spec.Fabric.MTU == 0 {
		if err := e.Set(9000, "fabric", "mtu"); err != nil {
			return nil, config.Report{}, err
		}
	}
	if c.Spec.Fabric.RoCE.GIDIndex == "" {
		if err := e.Set("auto", "fabric", "roce", "gidIndex"); err != nil {
			return nil, config.Report{}, err
		}
	}
	step := opt.Cabling[0]
	subnets, err := allocSubnets(c, 2) // one /30 per rail
	if err != nil {
		return nil, config.Report{}, err
	}
	if err := e.Append(config.Link{A: step.A, B: step.B, Subnets: subnets}, "fabric", "links"); err != nil {
		return nil, config.Report{}, err
	}
	group := uniqueGroup(c)
	if err := e.Append(config.Group{Name: group, Nodes: []string{step.A.Node, step.B.Node}}, "groups"); err != nil {
		return nil, config.Report{}, err
	}
	if !ch.NoModel {
		var fit ModelFit
		ok := false
		if ch.Model != "" {
			for _, f := range opt.Models {
				if f.CatalogRef == ch.Model {
					fit, ok = f, true
				}
			}
			if !ok {
				return nil, config.Report{}, fmt.Errorf("model %q does not fit option %s", ch.Model, opt.ID)
			}
		} else if fit, ok = largest(opt.Models); !ok {
			return nil, config.Report{}, fmt.Errorf("no catalog model fits option %s; plan it with no_model", opt.ID)
		}
		name := ch.Name
		if name == "" {
			name = fit.CatalogRef
		}
		d := config.ModelDeployment{Name: name, CatalogRef: fit.CatalogRef, Profile: fit.Profile,
			Placement: config.Placement{Group: group}, GPUMemoryUtilization: fit.GPUMemoryUtilization}
		if err := e.Append(d, "models"); err != nil {
			return nil, config.Report{}, err
		}
		th := 0.6
		rule := config.RoutingRule{Name: ruleName("complex-" + name), Target: name,
			Match: config.RuleMatch{Signals: []config.Signal{{Type: "reasoning", Threshold: &th}}}}
		if err := e.Append(rule, "routing", "rules"); err != nil {
			return nil, config.Report{}, err
		}
	}
	out, err := e.Bytes()
	if err != nil {
		return nil, config.Report{}, err
	}
	next, err := config.ParseCluster(out)
	if err != nil {
		return nil, config.Report{}, err
	}
	rep := config.Validate(next, in.Catalog, config.Options{})
	if !rep.OK() {
		return out, rep, fmt.Errorf("the plan does not validate: %v", rep.Errors())
	}
	return out, rep, nil
}

// ObservedCables works out the cables between boxes from their hardware
// reports: a rail that sees another box's rail MAC as a neighbour is cabled
// to that box's port.
func ObservedCables(hw map[string]join.Hardware) []Cable {
	type end struct {
		node string
		port int
	}
	byMAC := map[string]end{}
	for node, h := range hw {
		for _, p := range h.Ports {
			for _, nd := range p.Halves {
				if nd.MAC != "" {
					byMAC[strings.ToLower(nd.MAC)] = end{node, p.Index}
				}
			}
		}
	}
	seen := map[string]bool{}
	var out []Cable
	nodes := make([]string, 0, len(hw))
	for n := range hw {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		h := hw[node]
		for _, p := range h.Ports {
			for _, nd := range p.Halves {
				for _, nb := range h.Neighbors[nd.Name] {
					peer, ok := byMAC[nb.MAC]
					if !ok || peer.node == node {
						continue
					}
					a, b := end{node, p.Index}, peer
					if a.node > b.node {
						a, b = b, a
					}
					key := fmt.Sprintf("%s/%d-%s/%d", a.node, a.port, b.node, b.port)
					if seen[key] {
						continue
					}
					seen[key] = true
					out = append(out, Cable{A: config.LinkEnd{Node: a.node, Port: intp(a.port)}, B: config.LinkEnd{Node: b.node, Port: intp(b.port)}})
				}
			}
		}
	}
	return out
}
