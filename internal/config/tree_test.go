package config

import (
	"os"
	"strings"
	"testing"
)

// Several edits keep the file's comments and produce a document that parses.
func TestEditorKeepsComments(t *testing.T) {
	data, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEditor(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Append(Node{Name: "spark-2", Address: "192.0.2.12", Roles: []string{RoleInference}}, "nodes"); err != nil {
		t.Fatal(err)
	}
	if err := e.Append(Node{Name: "spark-3", Address: "192.0.2.13", Roles: []string{RoleInference}}, "nodes"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(TopologyPairPlusSingle, "fabric", "topology"); err != nil {
		t.Fatal(err)
	}
	if err := e.Append(Link{A: LinkEnd{Node: "spark-2", Port: intp(1)}, B: LinkEnd{Node: "spark-3", Port: intp(1)},
		Subnets: []string{"10.200.0.0/30", "10.200.0.4/30"}}, "fabric", "links"); err != nil {
		t.Fatal(err)
	}
	if err := e.Append(Group{Name: "pair-1", Nodes: []string{"spark-2", "spark-3"}}, "groups"); err != nil {
		t.Fatal(err)
	}
	out, err := e.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Memory on one GB10") {
		t.Fatal("comments were lost")
	}
	c, err := ParseCluster(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Spec.Nodes) != 3 || c.Spec.Fabric.Topology != TopologyPairPlusSingle || len(c.Spec.Fabric.Links) != 1 || *c.Spec.Fabric.Links[0].B.Port != 1 || c.Spec.Groups[0].Name != "pair-1" {
		t.Fatalf("edits not applied: %+v", c.Spec)
	}
	if r := Validate(c, mustCatalog(t), Options{}); !r.OK() {
		t.Fatalf("edited cluster does not validate: %v", r.Errors())
	}
	if _, err := e.Bytes(); err != nil { // encoding twice is fine
		t.Fatal(err)
	}
	if err := e.Set("bogus", "fabric", "topology"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Bytes(); err == nil {
		t.Fatal("an invalid edit must not encode")
	}
}
