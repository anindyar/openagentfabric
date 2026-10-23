package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
)

func TestKeysIssueAndRotate(t *testing.T) {
	docker := fakeCluster(t)
	dir := t.TempDir()
	data, err := os.ReadFile("../../examples/cluster.pair.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(cluster, data, 0o644); err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(dir, "secrets")
	args := func(cmd ...string) []string {
		return append(cmd, "-f", cluster, "--catalog", "../../examples/catalog.yaml", "--secrets-dir", secretsDir)
	}
	if out, err := runCLI(t, args("apply")...); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	before := docker.mutations()

	out, err := runCLI(t, args("keys", "issue", "vclocal", "--host", "host.docker.internal")...)
	if err != nil {
		t.Fatalf("issue: %v\n%s", err, out)
	}
	key := regexp.MustCompile(`(?m)^AGENT_GATEWAY_API_KEY=([0-9a-f]{64})$`).FindStringSubmatch(out)
	if key == nil {
		t.Fatalf("issue printed no key:\n%s", out)
	}
	for _, want := range []string{
		"AGENT_GATEWAY_URL=http://host.docker.internal:4000/v1",
		"AGENT_GATEWAY_ROTATE_URL=http://host.docker.internal:7702/v1/keys/rotate",
		"AGENT_GATEWAY_MODEL=auto",
		"AGENT_GATEWAY_CONTEXT_WINDOW=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("issue output lacks %q:\n%s", want, out)
		}
	}
	// Only the gateway is touched: one compose up, nothing removed.
	if got := docker.mutations() - before; got != 1 {
		t.Fatalf("issue ran %d docker mutations, want 1 (the gateway)\n%s", got, out)
	}
	c, err := config.LoadCluster(cluster)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c.Spec.Routing.Keys); c.Spec.Routing.Keys[n-1].Name != "vclocal" {
		t.Fatalf("cluster.yaml keys = %+v", c.Spec.Routing.Keys)
	}

	again, err := runCLI(t, args("keys", "issue", "vclocal", "--host", "host.docker.internal")...)
	if err != nil || !strings.Contains(again, key[1]) {
		t.Fatalf("re-issue must return the same key: %v\n%s", err, again)
	}

	rotated, err := runCLI(t, args("keys", "rotate", "vclocal")...)
	if err != nil {
		t.Fatalf("rotate: %v\n%s", err, rotated)
	}
	if strings.Contains(rotated, key[1]) || !regexp.MustCompile(`(?m)^[0-9a-f]{64}$`).MatchString(rotated) {
		t.Fatalf("rotate output:\n%s", rotated)
	}
	stored, _ := os.ReadFile(filepath.Join(secretsDir, "gateway-key-vclocal"))
	if strings.TrimSpace(string(stored)) == key[1] {
		t.Fatal("the stored key did not change")
	}

	list, err := runCLI(t, args("keys", "list")...)
	if err != nil || !strings.Contains(list, "vclocal") || strings.Contains(list, key[1]) {
		t.Fatalf("list must name keys but never print them: %v\n%s", err, list)
	}
}

func TestSmallestContextWindow(t *testing.T) {
	c, err := config.LoadCluster("../../examples/cluster.pair.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := config.LoadCatalog("../../examples/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w := smallestContextWindow(c, cat)
	if w <= 0 {
		t.Fatalf("window = %d, want the smallest enabled model's", w)
	}
	for _, d := range c.Spec.Models {
		if d.IsEnabled() && d.MaxModelLen > 0 && d.MaxModelLen < w {
			t.Fatalf("window %d is larger than %s's %d", w, d.Name, d.MaxModelLen)
		}
	}
}
