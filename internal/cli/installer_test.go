package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
)

// The hospital appliance's installer turns a fresh single box (what
// spark-bootstrap.sh writes) into the hospital profile with CLI commands
// only; every step is idempotent and cluster.yaml keeps its comments.
func TestInstallerFlow(t *testing.T) {
	data, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(cluster, data, 0o644); err != nil {
		t.Fatal(err)
	}
	paths := []string{"-f", cluster, "--catalog", "../../examples/catalog.yaml"}
	run := func(want string, args ...string) string {
		t.Helper()
		out, err := runCLI(t, append(args, paths...)...)
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("oaf %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	fail := func(want string, args ...string) {
		t.Helper()
		out, err := runCLI(t, append(args, paths...)...)
		if err == nil || !strings.Contains(out+err.Error(), want) {
			t.Fatalf("oaf %s should fail with %q: %v\n%s", strings.Join(args, " "), want, err, out)
		}
	}

	// MedGemma beside ornith at 0.40 is over budget, and its licence is not accepted yet.
	fail("has not been accepted", "models", "add", "medgemma", "--catalog-ref", "medgemma-27b-it")
	run(`updated "ornith"`, "models", "set", "ornith", "--util", "0.25", "--max-model-len", "65536")
	run("unchanged", "models", "set", "ornith", "--util", "0.25")
	run(`added "gpt-oss-20b"`, "models", "add", "gpt-oss-20b", "--catalog-ref", "gpt-oss-20b")
	run("image/vllm", "licenses", "needed") // the engine of gpt-oss-20b; MedGemma is not deployed yet
	run("accepted model/medgemma-27b-it, image/vllm", "licenses", "accept", "model/medgemma-27b-it", "image/vllm", "--by", "admin@example.com")
	run("already accepted", "licenses", "accept", "image/vllm", "--by", "admin@example.com")
	fail("not a licence", "licenses", "accept", "model/bge-m3", "--by", "admin@example.com")
	run(`added "medgemma"`, "models", "add", "medgemma", "--catalog-ref", "medgemma-27b-it")
	run("no licences needed", "licenses", "needed")

	add := []string{"models", "adapter", "add", "gpt-oss-20b", "spetaar-ddx", "--path", "/var/lib/oaf/adapters/spetaar-ddx", "--rank", "16", "--entitlement", "ddx"}
	out := run(`added adapter "spetaar-ddx" on gpt-oss-20b`, add...)
	if !strings.Contains(out, "is not on this host") || !strings.Contains(out, `entitlement "ddx" is not present`) {
		t.Errorf("adapter warnings missing:\n%s", out)
	}
	run("already serves adapter", add...)
	base := add[:5]
	run(`updated adapter "spetaar-ddx" on gpt-oss-20b: rank 16 → 32`,
		append(base, "--path", "/var/lib/oaf/adapters/spetaar-ddx", "--rank", "32", "--entitlement", "ddx")...)
	before, _ := os.ReadFile(cluster)
	fail(`no guardrail policy "basic"`, append(base, "--path", "/var/lib/oaf/adapters/spetaar-ddx", "--rank", "16", "--guardrails", "basic")...)
	fail("absolute", append(base, "--path", "adapters/x", "--rank", "16")...)
	fail(`no model deployment "nope"`, "models", "adapter", "add", "nope", "x", "--path", "/x", "--rank", "8")
	if after, _ := os.ReadFile(cluster); string(after) != string(before) {
		t.Fatal("a refused change was written")
	}

	diff := []string{"routing", "rule", "set", "differential", "--target", "spetaar-ddx", "--keywords", "differential diagnosis, ddx", "--requires-entitlement", "ddx"}
	run(`added rule "differential" → spetaar-ddx`, diff...)
	run(`already has rule "differential"`, diff...)
	run(`added rule "medical" → medgemma`, "routing", "rule", "set", "medical", "--target", "medgemma", "--keywords", "patient,symptoms", "--before", "differential")
	run(`updated rule "differential"`, append(diff, "--before", "medical")...)
	fail("not an enabled deployment", "routing", "rule", "set", "x", "--target", "nope", "--keywords", "a")
	fail("give --keywords", "routing", "rule", "set", "y", "--target", "medgemma")
	run("routing mode active", "routing", "mode", "active")
	run("already active", "routing", "mode", "active")
	fail("observe or active", "routing", "mode", "on")
	run("unchanged", "routing", "defaults", "--fallback", "ornith")
	fail(`adapter "spetaar-ddx" is not served`, "routing", "defaults", "--fast", "spetaar-ddx")

	raw, _ := os.ReadFile(cluster)
	if !strings.Contains(string(raw), "# Embeddings for the apps on the box") || !strings.Contains(string(raw), "# Memory on one GB10") {
		t.Errorf("comments lost:\n%s", raw)
	}
	c, err := config.ParseCluster(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Spec.Routing.Rules; len(r) != 2 || r[0].Name != "differential" || r[1].Name != "medical" ||
		r[0].Requires == nil || strings.Join(r[0].Match.Signals[0].Keywords, "|") != "differential diagnosis|ddx" {
		t.Fatalf("rules = %+v", r)
	}
	cat, _ := config.LoadCatalog("../../examples/catalog.yaml")
	if r := config.Validate(c, cat, config.Options{Features: map[string]bool{"ddx": true}}); !r.OK() {
		t.Fatalf("result does not validate with ddx: %v", r.Errors())
	}
	if rt := c.ActiveRoutes(cat, map[string]bool{"ddx": true}); len(rt.Rules) != 2 || rt.Rules[0].Target != "spetaar-ddx" {
		t.Fatalf("routes = %+v", rt)
	}

	run(`removed rule "medical"`, "routing", "rule", "remove", "medical")
	fail(`no routing.rules "medical"`, "routing", "rule", "remove", "medical")
	run(`removed adapter "spetaar-ddx"`, "models", "adapter", "remove", "gpt-oss-20b", "spetaar-ddx")
	fail(`has no adapter "spetaar-ddx"`, "models", "adapter", "remove", "gpt-oss-20b", "spetaar-ddx")
	if raw, _ := os.ReadFile(cluster); strings.Contains(string(raw), "adapters:") {
		t.Errorf("empty adapters list left behind")
	}
}
