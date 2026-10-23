package config

import "fmt"

// Host ports of the OAF stack. Model deployments get their own ports; see
// Cluster.ModelPort.
const (
	PortLangfuse         = 3000
	PortGrafana          = 3001
	PortPrometheus       = 9090
	PortLoki             = 3100
	PortNodeExporter     = 9100
	PortGatewayAdmin     = 15000 // bound to 127.0.0.1 only
	PortGatewayStats     = 15020
	PortGatewayReadiness = 15021
	PortOAFD             = 8443
	PortNodeAgent        = 7701
	PortKeyService       = 7702 // oaf keys serve, on the control node

	// DefaultModelPortBase + index in spec.models is a deployment's port when it
	// sets none.
	DefaultModelPortBase = 8000
	// MasterPortBase + index in spec.models is the torch.distributed rendezvous
	// port of a multi-node deployment (on its rank-0 node).
	MasterPortBase = 29500
)

// GatewayPort is the agentgateway LLM listener port.
func (c *Cluster) GatewayPort() int {
	if g := c.Spec.Routing.Gateway; g != nil && g.Port != 0 {
		return g.Port
	}
	return DefaultGatewayPort
}

// ModelPort is the OpenAI API port of spec.models[i].
func (c *Cluster) ModelPort(i int) int {
	if p := c.Spec.Models[i].Port; p != 0 {
		return p
	}
	return DefaultModelPortBase + i
}

// MasterPort is the rendezvous port of spec.models[i] when it spans nodes.
func MasterPort(i int) int { return MasterPortBase + i }

// ServicePorts are the stack's host ports on node n, by owner, from its roles.
func ServicePorts(n Node) map[int]string {
	p := map[int]string{
		PortNodeAgent:    "oaf-node",
		PortNodeExporter: "node_exporter",
	}
	if n.HasRole(RoleControl) {
		p[PortOAFD] = "oafd"
		p[PortKeyService] = "oaf key service"
	}
	if n.HasRole(RoleGateway) {
		p[PortGatewayAdmin] = "agentgateway admin"
		p[PortGatewayStats] = "agentgateway stats"
		p[PortGatewayReadiness] = "agentgateway readiness"
	}
	if n.HasRole(RoleObservability) {
		p[PortLangfuse] = "Langfuse"
		p[PortGrafana] = "Grafana"
		p[PortPrometheus] = "Prometheus"
		p[PortLoki] = "Loki"
	}
	return p
}

// PlacementNodes resolves a placement to node names, in group order. It
// returns nil for an unknown node or group (semantic validation reports those).
func (c *Cluster) PlacementNodes(pl Placement) []string {
	if pl.Node != "" {
		for _, n := range c.Spec.Nodes {
			if n.Name == pl.Node {
				return []string{n.Name}
			}
		}
		return nil
	}
	for _, g := range c.Spec.Groups {
		if g.Name == pl.Group {
			return g.Nodes
		}
	}
	return nil
}

// Node returns the node called name.
func (c *Cluster) Node(name string) (Node, bool) {
	for _, n := range c.Spec.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// Profile returns the serving profile deployment d uses on nodes nodes:
// the named one, else the first whose node count matches.
func Profile(d ModelDeployment, m *CatalogModel, nodes int) (*ServingProf, error) {
	for i := range m.Profiles {
		p := &m.Profiles[i]
		if d.Profile != "" && p.Name == d.Profile || d.Profile == "" && p.Nodes == nodes {
			if p.Nodes != nodes {
				return nil, fmt.Errorf("profile %q needs %d node(s), but the placement has %d", p.Name, p.Nodes, nodes)
			}
			return p, nil
		}
	}
	if d.Profile != "" {
		return nil, fmt.Errorf("model %q has no profile %q", m.ID, d.Profile)
	}
	return nil, fmt.Errorf("model %q has no serving profile for %d node(s)", m.ID, nodes)
}
