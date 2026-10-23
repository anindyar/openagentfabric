package config

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// MaxNodeMemoryFraction caps what the deployments on one node may take of its
// unified memory, leaving the rest for the OS and services.
const MaxNodeMemoryFraction = 0.90

// Each vLLM instance also uses memory outside --gpu-memory-utilization: the
// CUDA context, the API server and worker processes. Measured on a GB10
// (121.6 GiB visible): ~4.6 GiB for the chat model's processes, ~2.7 GiB on
// top of its budget for a small embedding model. The larger figure is charged
// to every deployment, so two models on one box are not over-committed.
const (
	GB10MemoryGiB        = 121.6
	VLLMInstanceOverhead = 4.6 / GB10MemoryGiB
)

// DefaultGatewayPort matches the schema default for routing.gateway.port.
const DefaultGatewayPort = 4000

// Options carries facts that are not in the documents themselves.
type Options struct {
	// Features granted by valid entitlements (e.g. "ddx").
	Features map[string]bool
}

// Report is the result of semantic validation.
type Report struct {
	Issues []Issue
}

// Errors returns the error-severity issues.
func (r Report) Errors() []Issue { return r.filter(SeverityError) }

// Warnings returns the warning-severity issues.
func (r Report) Warnings() []Issue { return r.filter(SeverityWarning) }

// OK reports whether there are no errors.
func (r Report) OK() bool { return len(r.Errors()) == 0 }

func (r Report) filter(s Severity) []Issue {
	var out []Issue
	for _, i := range r.Issues {
		if i.Severity == s {
			out = append(out, i)
		}
	}
	return out
}

type checker struct {
	c    *Cluster
	cat  *Catalog
	opts Options
	out  []Issue

	nodes  map[string]*Node
	groups map[string]*Group
	// adjacency: number of links between each unordered node pair.
	links map[[2]string]int
}

func (k *checker) errorf(path, format string, args ...any) {
	k.out = append(k.out, Issue{Path: path, Message: fmt.Sprintf(format, args...), Severity: SeverityError})
}

func (k *checker) warnf(path, format string, args ...any) {
	k.out = append(k.out, Issue{Path: path, Message: fmt.Sprintf(format, args...), Severity: SeverityWarning})
}

// ValidateCatalog checks catalog rules that JSON Schema cannot express.
func ValidateCatalog(cat *Catalog) Report {
	k := &checker{cat: cat}
	k.catalog()
	return Report{Issues: k.out}
}

// Validate checks a schema-valid cluster against its catalog (docs/PLAN.md §4.3).
// Catalog issues are included, so callers need only this one call.
func Validate(c *Cluster, cat *Catalog, opts Options) Report {
	k := &checker{c: c, cat: cat, opts: opts}
	k.catalog()
	k.nodesAndRoles()
	k.fabric()
	k.groupsCheck()
	k.models()
	k.routing()
	k.observability()
	return Report{Issues: k.out}
}

// observability: app scrape targets become Prometheus jobs, whose names
// must be unique.
func (k *checker) observability() {
	m := k.c.Spec.Observability.Metrics
	if m == nil {
		return
	}
	seen := map[string]bool{}
	for i, s := range m.Scrape {
		if seen[s.Name] {
			k.errorf(fmt.Sprintf("spec.observability.metrics.scrape[%d].name", i), "duplicate scrape target %q", s.Name)
		}
		seen[s.Name] = true
	}
}

// --- catalog ---------------------------------------------------------------

func (k *checker) catalog() {
	ids := map[string]bool{}
	for i, m := range k.cat.Models {
		p := fmt.Sprintf("catalog.models[%d]", i)
		if ids[m.ID] {
			k.errorf(p+".id", "duplicate model id %q", m.ID)
		}
		ids[m.ID] = true

		if (m.Source.Entitled != nil) != (m.Entitlement != nil) {
			k.errorf(p, "source.entitled and entitlement must be set together")
		}
		if m.License.NeedsAcceptance() && m.License.AcceptURL == "" {
			k.errorf(p+".license.acceptUrl", "required: licence %s needs user acceptance", licenceLabel(m.License))
		}

		profiles := map[string]bool{}
		for j, pr := range m.Profiles {
			pp := fmt.Sprintf("%s.profiles[%d]", p, j)
			if profiles[pr.Name] {
				k.errorf(pp+".name", "duplicate profile name %q", pr.Name)
			}
			profiles[pr.Name] = true
			// One GPU per GB10: the parallel degree must equal the node count.
			if pr.TensorParallel*pr.PP() != pr.Nodes {
				k.errorf(pp, "tensorParallel × pipelineParallel = %d, but nodes = %d (one GPU per GB10 node)",
					pr.TensorParallel*pr.PP(), pr.Nodes)
			}
		}
	}
}

func licenceLabel(l ModelLicense) string {
	switch {
	case l.SPDX != "" && l.Gated:
		return l.SPDX + " (gated)"
	case l.SPDX != "":
		return l.SPDX
	case l.Gated:
		return fmt.Sprintf("%q (gated)", l.Name)
	default:
		return fmt.Sprintf("%q", l.Name)
	}
}

// --- nodes and roles -----------------------------------------------------

func (k *checker) nodesAndRoles() {
	k.nodes = map[string]*Node{}
	addrs := map[string]string{}
	roleCount := map[string]int{}
	for i := range k.c.Spec.Nodes {
		n := &k.c.Spec.Nodes[i]
		p := fmt.Sprintf("spec.nodes[%d]", i)
		if _, dup := k.nodes[n.Name]; dup {
			k.errorf(p+".name", "duplicate node name %q", n.Name)
		}
		k.nodes[n.Name] = n
		if other, dup := addrs[n.Address]; dup {
			k.errorf(p+".address", "address %s is already used by node %q", n.Address, other)
		}
		addrs[n.Address] = n.Name
		for _, r := range n.Roles {
			roleCount[r]++
		}
	}
	if roleCount[RoleControl] != 1 {
		k.errorf("spec.nodes", "exactly one node must have the control role (found %d)", roleCount[RoleControl])
	}
	if roleCount[RoleObservability] != 1 {
		k.errorf("spec.nodes", "exactly one node must have the observability role in v1 (found %d)", roleCount[RoleObservability])
	}
	if roleCount[RoleGateway] < 1 {
		k.errorf("spec.nodes", "at least one node must have the gateway role")
	}
	if len(k.c.Spec.Models) > 0 && roleCount[RoleInference] < 1 {
		k.errorf("spec.nodes", "models are defined but no node has the inference role")
	}
}

// --- fabric --------------------------------------------------------------

var topologyNodes = map[string]int{
	TopologySingle: 1, TopologyPair: 2, TopologyTriangle: 3, TopologyPairs: 4,
}

func pairKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

func (k *checker) fabric() {
	f := k.c.Spec.Fabric
	want := topologyNodes[f.Topology]
	if got := len(k.c.Spec.Nodes); got != want {
		k.errorf("spec.fabric.topology", "topology %q needs exactly %d node(s), found %d", f.Topology, want, got)
		return // link checks below assume the right node count
	}

	k.links = map[[2]string]int{}
	cables := map[string]int{}     // links per node (a GB10 has 2 physical ports)
	portUse := map[string]string{} // node/port -> path of first use
	subnets := map[string]string{} // subnet -> path of first use
	ok := true
	for i, l := range f.Links {
		p := fmt.Sprintf("spec.fabric.links[%d]", i)
		for _, end := range []struct {
			e    LinkEnd
			side string
		}{{l.A, "a"}, {l.B, "b"}} {
			if _, known := k.nodes[end.e.Node]; !known {
				k.errorf(p+"."+end.side+".node", "unknown node %q", end.e.Node)
				ok = false
				continue
			}
			cables[end.e.Node]++
			if end.e.Port != nil {
				key := fmt.Sprintf("%s/%d", end.e.Node, *end.e.Port)
				if first, dup := portUse[key]; dup {
					k.errorf(p+"."+end.side+".port", "port %d on %s is already used by %s", *end.e.Port, end.e.Node, first)
				}
				portUse[key] = p
			}
		}
		if l.A.Node == l.B.Node {
			k.errorf(p, "a link cannot connect node %q to itself", l.A.Node)
			ok = false
			continue
		}
		for j, sn := range l.Subnets {
			if first, dup := subnets[sn]; dup {
				k.errorf(fmt.Sprintf("%s.subnets[%d]", p, j), "subnet %s is already used by %s", sn, first)
			}
			subnets[sn] = fmt.Sprintf("%s.subnets[%d]", p, j)
		}
		k.links[pairKey(l.A.Node, l.B.Node)]++
	}
	for _, n := range sortedKeys(cables) {
		if cables[n] > 2 {
			k.errorf("spec.fabric.links", "node %q has %d links, but a GB10 has only 2 QSFP ports", n, cables[n])
			ok = false
		}
	}
	if !ok {
		return
	}
	k.topologyShape()

	if f.Topology == TopologyTriangle && f.NCCL.Transport == TransportIB {
		k.warnf("spec.fabric.nccl.transport", "ib on a triangle is known to fail on GB10; use socket or auto")
	}
}

// topologyShape checks that the link graph is exactly the declared topology.
func (k *checker) topologyShape() {
	f := k.c.Spec.Fabric
	names := make([]string, 0, len(k.nodes))
	for n := range k.nodes {
		names = append(names, n)
	}
	sort.Strings(names)

	switch f.Topology {
	case TopologySingle:
		if len(f.Links) > 0 {
			k.errorf("spec.fabric.links", "topology single takes no links")
		}
	case TopologyPair:
		k.checkPair(names[0], names[1])
	case TopologyTriangle:
		for _, pk := range [][2]string{{names[0], names[1]}, {names[0], names[2]}, {names[1], names[2]}} {
			if k.links[pk] != 1 {
				k.errorf("spec.fabric.links", "a triangle needs exactly one link between %s and %s (found %d)", pk[0], pk[1], k.links[pk])
			}
		}
	case TopologyPairs:
		k.checkPairs(names)
	}
}

func (k *checker) checkPair(a, b string) {
	// One cable is the normal dual-rail pair; a second cable is allowed.
	if n := k.links[pairKey(a, b)]; n < 1 || n > 2 {
		k.errorf("spec.fabric.links", "pair %s–%s needs 1 or 2 links (found %d)", a, b, n)
	}
}

func (k *checker) checkPairs(names []string) {
	// Find connected components. "pairs" must be exactly two components of two nodes.
	parent := map[string]string{}
	for _, n := range names {
		parent[n] = n
	}
	var find func(string) string
	find = func(x string) string {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	for pk := range k.links {
		parent[find(pk[0])] = find(pk[1])
	}
	comps := map[string][]string{}
	for _, n := range names {
		r := find(n)
		comps[r] = append(comps[r], n)
	}
	if len(comps) == 1 && len(k.links) >= 4 {
		k.errorf("spec.fabric.links", "these links form a 4-node ring. NCCL tree channels need links between non-adjacent nodes, and RDMA cannot be forwarded, so a switchless ring breaks multi-node inference. Use two separate pairs, or add a QSFP switch")
		return
	}
	for _, root := range sortedKeys(comps) {
		members := comps[root]
		if len(members) != 2 {
			k.errorf("spec.fabric.links", "topology pairs needs two separate 2-node pairs; nodes %s are not connected as a pair", strings.Join(members, ", "))
			continue
		}
		k.checkPair(members[0], members[1])
	}
}

// --- groups --------------------------------------------------------------

func (k *checker) groupsCheck() {
	k.groups = map[string]*Group{}
	for i := range k.c.Spec.Groups {
		g := &k.c.Spec.Groups[i]
		p := fmt.Sprintf("spec.groups[%d]", i)
		if _, dup := k.groups[g.Name]; dup {
			k.errorf(p+".name", "duplicate group name %q", g.Name)
		}
		k.groups[g.Name] = g
		known := true
		for j, n := range g.Nodes {
			node, ok := k.nodes[n]
			if !ok {
				k.errorf(fmt.Sprintf("%s.nodes[%d]", p, j), "unknown node %q", n)
				known = false
				continue
			}
			if !node.HasRole(RoleInference) {
				k.errorf(fmt.Sprintf("%s.nodes[%d]", p, j), "node %q does not have the inference role", n)
			}
		}
		if !known || k.links == nil {
			continue
		}
		for a := 0; a < len(g.Nodes); a++ {
			for b := a + 1; b < len(g.Nodes); b++ {
				if k.links[pairKey(g.Nodes[a], g.Nodes[b])] == 0 {
					k.errorf(p+".nodes", "%s and %s are not directly linked; every node in a group must be linked to every other", g.Nodes[a], g.Nodes[b])
				}
			}
		}
	}
}

// --- models --------------------------------------------------------------

func (k *checker) models() {
	names := map[string]bool{}
	memory := map[string]float64{}
	instances := map[string]int{}
	ports := map[string]map[int]string{} // node -> port -> owner

	reserve := func(node string, port int, owner, path string) {
		if ports[node] == nil {
			ports[node] = map[int]string{}
		}
		if other, taken := ports[node][port]; taken {
			k.errorf(path, "port %d on node %q is already used by %s", port, node, other)
			return
		}
		ports[node][port] = owner
	}
	for _, n := range k.c.Spec.Nodes {
		svc := ServicePorts(n)
		for _, p := range sortedKeys(svc) {
			reserve(n.Name, p, svc[p], "spec.nodes")
		}
		if n.HasRole(RoleGateway) {
			reserve(n.Name, k.c.GatewayPort(), "the gateway", "spec.routing.gateway.port")
		}
	}

	accepted := map[string]bool{}
	for _, a := range k.c.Spec.LicenseAcceptances {
		accepted[a.Subject] = true
	}

	for i, d := range k.c.Spec.Models {
		p := fmt.Sprintf("spec.models[%d]", i)
		if names[d.Name] {
			k.errorf(p+".name", "duplicate served model name %q", d.Name)
		}
		names[d.Name] = true

		placed, ok := k.placementNodes(d.Placement, p+".placement")
		if !ok {
			continue
		}
		model, ok := k.cat.Lookup(d.CatalogRef)
		if !ok {
			k.errorf(p+".catalogRef", "no catalog entry %q", d.CatalogRef)
			continue
		}
		if _, ok := k.profileFor(d, model, len(placed), p); !ok {
			continue
		}
		if model.Entitlement != nil && !k.opts.Features[model.Entitlement.Feature] {
			k.errorf(p+".catalogRef", "model %q requires the %q entitlement, which is not present", model.ID, model.Entitlement.Feature)
		}
		if model.License.NeedsAcceptance() && !accepted[AcceptanceSubject(model.ID)] {
			k.errorf(p+".catalogRef", "licence %s for %q has not been accepted; accept it at %s (records %s in spec.licenseAcceptances)",
				licenceLabel(model.License), model.ID, model.License.AcceptURL, AcceptanceSubject(model.ID))
		}
		if !d.IsEnabled() {
			continue
		}
		for r, n := range placed {
			memory[n] += d.GPUMemoryUtilization
			instances[n]++
			if r == 0 {
				reserve(n, k.c.ModelPort(i), fmt.Sprintf("model %q", d.Name), p+".port")
				if len(placed) > 1 {
					reserve(n, MasterPort(i), fmt.Sprintf("model %q (rendezvous)", d.Name), p)
				}
			}
		}
	}
	for _, n := range sortedKeys(memory) {
		if memory[n] > MaxNodeMemoryFraction+1e-9 {
			k.errorf("spec.models", "node %q: gpuMemoryUtilization of its deployments sums to %.2f, over the %.2f limit", n, memory[n], MaxNodeMemoryFraction)
			continue
		}
		if total := memory[n] + float64(instances[n])*VLLMInstanceOverhead; total > MaxNodeMemoryFraction+1e-9 {
			k.warnf("spec.models", "node %q: with about %.1f GiB of vLLM overhead per model its deployments need %.2f of unified memory, over the %.2f limit; little is left for the OS and other apps",
				n, VLLMInstanceOverhead*GB10MemoryGiB, total, MaxNodeMemoryFraction)
		}
	}
}

// placementNodes resolves a placement to node names.
func (k *checker) placementNodes(pl Placement, path string) ([]string, bool) {
	if pl.Node != "" {
		n, ok := k.nodes[pl.Node]
		if !ok {
			k.errorf(path+".node", "unknown node %q", pl.Node)
			return nil, false
		}
		if !n.HasRole(RoleInference) {
			k.errorf(path+".node", "node %q does not have the inference role", pl.Node)
			return nil, false
		}
		return []string{pl.Node}, true
	}
	g, ok := k.groups[pl.Group]
	if !ok {
		k.errorf(path+".group", "unknown group %q", pl.Group)
		return nil, false
	}
	return g.Nodes, true
}

// profileFor picks the named profile, or the first whose node count matches.
func (k *checker) profileFor(d ModelDeployment, m *CatalogModel, nodes int, path string) (*ServingProf, bool) {
	p, err := Profile(d, m, nodes)
	if err != nil {
		field := ".placement"
		if d.Profile != "" {
			field = ".profile"
		}
		k.errorf(path+field, "%s", err)
		return nil, false
	}
	return p, true
}

// --- routing -------------------------------------------------------------

func (k *checker) routing() {
	r := k.c.Spec.Routing
	served := map[string]bool{}
	embedding := map[string]bool{} // embedding models are served, but never a chat route
	chatModels := 0
	for _, d := range k.c.Spec.Models {
		if !d.IsEnabled() {
			continue
		}
		served[d.Name] = true
		if m, ok := k.cat.Lookup(d.CatalogRef); ok && m.IsEmbedding() {
			embedding[d.Name] = true
		} else {
			chatModels++
		}
	}
	chatTarget := func(path, name string) {
		switch {
		case !served[name]:
			k.errorf(path, "%q is not an enabled deployment", name)
		case embedding[name]:
			k.errorf(path, "%q is an embedding model; it cannot answer chat requests", name)
		}
	}

	if chatModels > 0 && r.Defaults.Fast == "" {
		k.errorf("spec.routing.defaults.fast", "required when models are deployed")
	}
	if r.Defaults.Fast != "" {
		chatTarget("spec.routing.defaults.fast", r.Defaults.Fast)
	}
	if r.Defaults.Fallback != "" {
		chatTarget("spec.routing.defaults.fallback", r.Defaults.Fallback)
	}

	ruleNames := map[string]bool{}
	for i, rule := range r.Rules {
		p := fmt.Sprintf("spec.routing.rules[%d]", i)
		if ruleNames[rule.Name] {
			k.errorf(p+".name", "duplicate rule name %q", rule.Name)
		}
		ruleNames[rule.Name] = true
		gated := rule.Requires != nil && rule.Requires.Entitlement != ""
		switch {
		case gated && !k.opts.Features[rule.Requires.Entitlement]:
			k.warnf(p, "rule %q is inactive: entitlement %q is not present; matching requests go to the default model", rule.Name, rule.Requires.Entitlement)
		case !served[rule.Target] && gated:
			k.warnf(p+".target", "rule %q is inactive: %q is not deployed", rule.Name, rule.Target)
		default:
			chatTarget(p+".target", rule.Target)
		}
	}

	for i, key := range r.Keys {
		for j, m := range key.Models {
			if m != "auto" && !served[m] {
				k.warnf(fmt.Sprintf("spec.routing.keys[%d].models[%d]", i, j), "%q is not an enabled deployment", m)
			}
		}
	}
}

func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
