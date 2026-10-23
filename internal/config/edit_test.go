package config

import (
	"os"
	"strings"
	"testing"
)

func TestAddAPIKey(t *testing.T) {
	data, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, changed, err := AddAPIKey(data, APIKey{Name: "vclocal", SecretRef: "gateway-key-vclocal"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("adding a new key reported no change")
	}
	c, err := ParseCluster(out)
	if err != nil {
		t.Fatal(err)
	}
	keys := c.Spec.Routing.Keys
	if len(keys) != 2 || keys[0].Name != "default" || keys[1].Name != "vclocal" || keys[1].SecretRef != "gateway-key-vclocal" {
		t.Fatalf("keys = %+v", keys)
	}
	// Comments in the hand-written file survive the edit.
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") && !strings.Contains(string(out), strings.TrimSpace(line)) {
			t.Fatalf("comment lost: %q", line)
		}
	}

	again, changed, err := AddAPIKey(out, APIKey{Name: "vclocal", SecretRef: "gateway-key-vclocal"})
	if err != nil || changed || string(again) != string(out) {
		t.Fatalf("re-adding an existing key: changed=%v err=%v", changed, err)
	}
}

func TestAddAPIKeyCreatesRouting(t *testing.T) {
	data, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCluster(data)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.Routing.Keys == nil {
		t.Skip("example has no keys")
	}
	// Strip the keys block to model a cluster.yaml written without one.
	var kept []string
	skip := false
	for _, line := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case trim == "keys:":
			skip = true
			continue
		case skip && (strings.HasPrefix(trim, "- name:") || strings.HasPrefix(trim, "secretRef:")):
			continue
		default:
			skip = false
		}
		kept = append(kept, line)
	}
	out, changed, err := AddAPIKey([]byte(strings.Join(kept, "\n")), APIKey{Name: "app", SecretRef: "gateway-key-app", Models: []string{"auto"}})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	c, err = ParseCluster(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Spec.Routing.Keys) != 1 || c.Spec.Routing.Keys[0].Models[0] != "auto" {
		t.Fatalf("keys = %+v", c.Spec.Routing.Keys)
	}
}
