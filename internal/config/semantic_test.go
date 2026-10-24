package config

import (
	"strings"
	"testing"
)

func intp(i int) *int { return &i }

// TestSemanticRules mutates a valid example and expects one specific error.
func TestSemanticRules(t *testing.T) {
	cases := []struct {
		name    string
		example string
		mutate  func(*Cluster, *Catalog)
		opts    Options
		path    string
		message string
	}{
		{"two control nodes", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Nodes[1].Roles = append(c.Spec.Nodes[1].Roles, RoleControl)
		}, Options{}, "spec.nodes", "exactly one node must have the control role"},
		{"no gateway", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Nodes[0].Roles = []string{RoleControl, RoleObservability, RoleInference}
		}, Options{}, "spec.nodes", "gateway role"},
		{"duplicate node name", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Nodes[1].Name = "spark-1"
		}, Options{}, "spec.nodes[1].name", "duplicate node name"},
		{"duplicate address", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Nodes[1].Address = c.Spec.Nodes[0].Address
		}, Options{}, "spec.nodes[1].address", "already used"},
		{"pair-plus-single needs three nodes", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Topology = TopologyPairPlusSingle
		}, Options{}, "spec.fabric.topology", "needs exactly 3 node(s)"},
		{"pair-plus-single with the single cabled", ppsExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Links = append(c.Spec.Fabric.Links, Link{A: LinkEnd{Node: "spark-1", Port: intp(1)}, B: LinkEnd{Node: "spark-2", Port: intp(0)}})
		}, Options{}, "spec.fabric.links", "exactly two nodes and none to the third"},
		{"pair-plus-single without its pair cable", ppsExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Links = nil
			c.Spec.Groups = nil
			c.Spec.Models = c.Spec.Models[:1]
			c.Spec.Routing.Rules = nil
		}, Options{}, "spec.fabric.links", "exactly two nodes and none to the third"},
		{"pair-plus-single group across the LAN", ppsExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Groups[0].Nodes = []string{"spark-1", "spark-2"}
		}, Options{}, "spec.groups[0].nodes", "spark-1 and spark-2 are not directly linked"},
		{"pair-plus-single three cables on the pair", ppsExample, func(c *Cluster, _ *Catalog) {
			l := Link{A: LinkEnd{Node: "spark-2"}, B: LinkEnd{Node: "spark-3"}}
			c.Spec.Fabric.Links = append(c.Spec.Fabric.Links, l, l)
		}, Options{}, "spec.fabric.links", "only 2 QSFP ports"},
		{"topology node count", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Topology = TopologyTriangle
		}, Options{}, "spec.fabric.topology", "needs exactly 3 node(s)"},
		{"link to unknown node", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Links[0].B.Node = "ghost"
		}, Options{}, "spec.fabric.links[0].b.node", "unknown node"},
		{"self link", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Links[0].B.Node = "spark-1"
		}, Options{}, "spec.fabric.links[0]", "to itself"},
		{"three links in a pair", pairExample, func(c *Cluster, _ *Catalog) {
			l := Link{A: LinkEnd{Node: "spark-1"}, B: LinkEnd{Node: "spark-2"}}
			c.Spec.Fabric.Links = append(c.Spec.Fabric.Links, l, l)
		}, Options{}, "spec.fabric.links", "only 2 QSFP ports"},
		{"port reused", triangleExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Links[1].A.Port = intp(0)
		}, Options{}, "spec.fabric.links[1].a.port", "already used"},
		{"triangle missing edge", triangleExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Links = c.Spec.Fabric.Links[:2]
		}, Options{}, "spec.fabric.links", "exactly one link between gx-1 and gx-3"},
		{"duplicate subnet", triangleExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Links[1].Subnets[1] = c.Spec.Fabric.Links[0].Subnets[0]
		}, Options{}, "spec.fabric.links[1].subnets[1]", "already used"},
		{"group not linked", triangleExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Fabric.Links = c.Spec.Fabric.Links[:2]
		}, Options{}, "spec.groups[0].nodes", "gx-1 and gx-3 are not directly linked"},
		{"group node without inference", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Nodes[1].Roles = []string{RoleGateway}
		}, Options{}, "spec.groups[0].nodes[1]", "inference role"},
		{"unknown catalogRef", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models[0].CatalogRef = "nope"
		}, Options{}, "spec.models[0].catalogRef", "no catalog entry"},
		{"unknown group", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models[1].Placement.Group = "nope"
		}, Options{}, "spec.models[1].placement.group", "unknown group"},
		{"placement on non-inference node", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Nodes[0].Roles = []string{RoleControl, RoleGateway, RoleObservability}
		}, Options{}, "spec.models[0].placement.node", "inference role"},
		{"profile node mismatch", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models[1].Profile = "single"
		}, Options{}, "spec.models[1].profile", "needs 1 node(s)"},
		{"no profile for placement size", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models[0].Placement = Placement{Group: "pair-a"}
		}, Options{}, "spec.models[0].placement", "no serving profile for 2 node(s)"},
		{"memory over budget", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models[0].GPUMemoryUtilization = 0.45 // + 0.50 on spark-1
		}, Options{}, "spec.models", `node "spark-1"`},
		{"port collides with gateway", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models[0].Port = DefaultGatewayPort
		}, Options{}, "spec.models[0].port", "already used by the gateway"},
		{"port collides with a stack service", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models[0].Port = PortNodeExporter
		}, Options{}, "spec.models[0].port", "already used by node_exporter"},
		{"explicit port collides with a default one", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models[1].Port = DefaultModelPortBase // model 0's default port, also on spark-1
		}, Options{}, "spec.models[1].port", `already used by model "fast"`},
		{"entitled model without entitlement", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models = append(c.Spec.Models, ModelDeployment{
				Name: "ddx", CatalogRef: "ddx", Placement: Placement{Node: "spark-2"}, GPUMemoryUtilization: 0.2,
			})
			c.Spec.LicenseAcceptances = []LicenseAcceptance{{Subject: "model/ddx"}}
		}, Options{}, "spec.models[2].catalogRef", `requires the "ddx" entitlement`},
		{"licence not accepted", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Models = append(c.Spec.Models, ModelDeployment{
				Name: "ddx", CatalogRef: "ddx", Placement: Placement{Node: "spark-2"}, GPUMemoryUtilization: 0.2,
			})
		}, Options{Features: map[string]bool{"ddx": true}}, "spec.models[2].catalogRef", "has not been accepted"},
		{"default fast not deployed", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Routing.Defaults.Fast = "nope"
		}, Options{}, "spec.routing.defaults.fast", "not an enabled deployment"},
		{"default fast disabled", pairExample, func(c *Cluster, _ *Catalog) {
			f := false
			c.Spec.Models[0].Enabled = &f
		}, Options{}, "spec.routing.defaults.fast", "not an enabled deployment"},
		{"ungated rule target missing", pairExample, func(c *Cluster, _ *Catalog) {
			c.Spec.Routing.Rules[0].Target = "nope"
		}, Options{}, "spec.routing.rules[0].target", "not an enabled deployment"},
		{"catalog tp mismatch", pairExample, func(_ *Cluster, cat *Catalog) {
			cat.Models[1].Profiles[1].TensorParallel = 1
		}, Options{}, "catalog.models[1].profiles[1]", "one GPU per GB10"},
		{"catalog acceptUrl missing", pairExample, func(_ *Cluster, cat *Catalog) {
			cat.Models[1].License.SPDX = "LicenseRef-llama"
		}, Options{}, "catalog.models[1].license.acceptUrl", "needs user acceptance"},
		{"catalog duplicate id", pairExample, func(_ *Cluster, cat *Catalog) {
			cat.Models[1].ID = cat.Models[0].ID
		}, Options{}, "catalog.models[1].id", "duplicate model id"},
		{"catalog entitled without entitlement", pairExample, func(_ *Cluster, cat *Catalog) {
			cat.Models[2].Entitlement = nil
		}, Options{}, "catalog.models[2]", "must be set together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cat := mustCluster(t, tc.example), mustCatalog(t)
			tc.mutate(c, cat)
			r := Validate(c, cat, tc.opts)
			for _, i := range r.Errors() {
				if i.Path == tc.path && strings.Contains(i.Message, tc.message) {
					return
				}
			}
			t.Fatalf("want error at %s containing %q; got errors %v", tc.path, tc.message, r.Errors())
		})
	}
}

func fourNodePairs(t *testing.T, links [][2]string) *Cluster {
	t.Helper()
	c := mustCluster(t, pairExample)
	c.Spec.Nodes = append(c.Spec.Nodes,
		Node{Name: "spark-3", Address: "192.168.1.13", Roles: []string{RoleInference}},
		Node{Name: "spark-4", Address: "192.168.1.14", Roles: []string{RoleInference}},
	)
	c.Spec.Fabric.Topology = TopologyPairs
	c.Spec.Fabric.Links = nil
	for _, l := range links {
		c.Spec.Fabric.Links = append(c.Spec.Fabric.Links, Link{A: LinkEnd{Node: l[0]}, B: LinkEnd{Node: l[1]}})
	}
	return c
}

func TestFourNodeRingIsRejected(t *testing.T) {
	c := fourNodePairs(t, [][2]string{
		{"spark-1", "spark-2"}, {"spark-2", "spark-3"}, {"spark-3", "spark-4"}, {"spark-4", "spark-1"},
	})
	r := Validate(c, mustCatalog(t), Options{})
	for _, i := range r.Errors() {
		if strings.Contains(i.Message, "4-node ring") {
			return
		}
	}
	t.Fatalf("ring not rejected: %v", r.Errors())
}

func TestTwoPairsAreAccepted(t *testing.T) {
	c := fourNodePairs(t, [][2]string{
		{"spark-1", "spark-2"}, {"spark-3", "spark-4"},
	})
	if r := Validate(c, mustCatalog(t), Options{}); !r.OK() {
		t.Fatalf("unexpected errors: %v", r.Errors())
	}
}

func TestGroupAcrossPairsIsRejected(t *testing.T) {
	c := fourNodePairs(t, [][2]string{
		{"spark-1", "spark-2"}, {"spark-1", "spark-2"}, {"spark-3", "spark-4"}, {"spark-3", "spark-4"},
	})
	c.Spec.Groups[0].Nodes = []string{"spark-1", "spark-3"}
	r := Validate(c, mustCatalog(t), Options{})
	for _, i := range r.Errors() {
		if strings.Contains(i.Message, "not directly linked") {
			return
		}
	}
	t.Fatalf("group across pairs not rejected: %v", r.Errors())
}

func TestWarnings(t *testing.T) {
	t.Run("ib on triangle", func(t *testing.T) {
		c := mustCluster(t, triangleExample)
		c.Spec.Fabric.NCCL.Transport = TransportIB
		assertWarning(t, Validate(c, mustCatalog(t), Options{}), "ib on a triangle")
	})
	t.Run("entitled rule target not deployed", func(t *testing.T) {
		r := Validate(mustCluster(t, pairExample), mustCatalog(t), Options{Features: map[string]bool{"ddx": true}})
		assertWarning(t, r, `"ddx" is not deployed`)
	})
}

func assertWarning(t *testing.T, r Report, substr string) {
	t.Helper()
	if !r.OK() {
		t.Fatalf("unexpected errors: %v", r.Errors())
	}
	for _, w := range r.Warnings() {
		if strings.Contains(w.Message, substr) {
			return
		}
	}
	t.Fatalf("no warning containing %q: %v", substr, r.Warnings())
}

func TestAcceptedLicenceUnblocksDeployment(t *testing.T) {
	c := mustCluster(t, pairExample)
	c.Spec.Models = append(c.Spec.Models, ModelDeployment{
		Name: "ddx", CatalogRef: "ddx", Placement: Placement{Node: "spark-2"}, GPUMemoryUtilization: 0.2,
	})
	c.Spec.LicenseAcceptances = []LicenseAcceptance{{Subject: AcceptanceSubject("ddx")}}
	r := Validate(c, mustCatalog(t), Options{Features: map[string]bool{"ddx": true}})
	if !r.OK() {
		t.Fatalf("unexpected errors: %v", r.Errors())
	}
	for _, w := range r.Warnings() {
		// The medical rule's domain signal needs vSR's domain classifier,
		// which no bundle pins yet: routing says so, licensing must not.
		if !strings.Contains(w.Message, "domain signals are not supported yet") && !strings.Contains(w.Message, "no supported signal left") &&
			!strings.Contains(w.Message, "reasoning signals load the vsr-encoder") {
			t.Fatalf("unexpected warning: %v", w)
		}
	}
}
