package cli

import (
	"errors"
	"testing"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/reconcile"
)

func TestPortViews(t *testing.T) {
	ports := []*nodev1.FabricPort{
		{Index: 1, Halves: []*nodev1.FabricNetdev{{Name: "a1", OperState: "down"}}},
		{Index: 0, Halves: []*nodev1.FabricNetdev{
			{Name: "a0", OperState: "up", SpeedMbps: 100000, Addrs: []string{"10.0.0.1/30"}, RdmaDev: "rocep1s0f0"},
			{Name: "b0", OperState: "up", SpeedMbps: 100000},
		}},
	}
	v := portViews(ports)
	if len(v) != 2 || v[0].Port != 0 || v[0].Link != "up" || v[0].SpeedGbps != 200 || v[0].Rails[0].IP != "10.0.0.1/30" || v[1].Link != "down" {
		t.Fatalf("ports: %+v", v)
	}
}

func TestDeploymentState(t *testing.T) {
	c, err := config.LoadCluster("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := config.LoadCatalog("../../examples/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := c.Spec.Models[0]
	running := reconcile.ProjectState{Exists: true, Services: []*nodev1.Service{{Name: "vllm", State: "running", Health: "healthy"}}}
	starting := reconcile.ProjectState{Exists: true, Services: []*nodev1.Service{{Name: "vllm", State: "running", Health: "starting"}}}
	cases := []struct {
		sts           []reconcile.ProjectState
		state, health string
	}{
		{nil, "pending", ""},
		{[]reconcile.ProjectState{running}, "running", "healthy"},
		{[]reconcile.ProjectState{starting}, "running", "starting"},
		{[]reconcile.ProjectState{{Exists: false}}, "missing", ""},
		{[]reconcile.ProjectState{{Err: errors.New("dial")}}, "unreachable", "dial"},
	}
	for _, tc := range cases {
		v := deploymentState(c, cat, d, tc.sts)
		if v.State != tc.state || v.Health != tc.health || v.Task != "chat" || len(v.Nodes) == 0 {
			t.Errorf("%+v: got %s/%s", tc.sts, v.State, v.Health)
		}
	}
}
