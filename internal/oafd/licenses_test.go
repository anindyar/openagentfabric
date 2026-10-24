package oafd

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/bundle"
	"openagentfabric.dev/oaf/internal/config"
)

// A change whose deployments need licences is refused at plan time with the
// licences listed; accepting them is a plan that records who and when; an
// apply that would not render leaves cluster.yaml as it was.
func TestLicencesAcceptedBeforeApply(t *testing.T) {
	f := newFixture(t)
	raw, _ := os.ReadFile(f.cluster)
	// The cluster deploys models whose licences nobody has accepted.
	cut := strings.Index(string(raw), "  licenseAcceptances:")
	if cut < 0 {
		t.Fatal("example has no licenseAcceptances block")
	}
	if err := os.WriteFile(f.cluster, raw[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := config.ParseVersions(bundle.VersionsYAML)
	if err != nil {
		t.Fatal(err)
	}
	var checkErr error
	s := New(Config{ClusterPath: f.cluster, CatalogPath: "../../examples/catalog.yaml", Secrets: f.store, StateDir: t.TempDir(),
		Versions: v,
		Check:    func(context.Context, []byte) error { return checkErr },
		Apply:    func(context.Context, io.Writer) error { f.applied.Add(1); return nil },
	})
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)

	code, plan := f.do(t, "PUT", "/api/v1/routing", map[string]any{"mode": "observe"}, token)
	needs, _ := plan["needs_acceptance"].([]any)
	if code != 422 || len(needs) == 0 {
		t.Fatalf("plan without licences: %d %v", code, plan)
	}
	var subjects []string
	for _, n := range needs {
		m := n.(map[string]any)
		if m["url"] == "" || m["title"] == "" {
			t.Fatalf("needed licence lacks url/title: %v", m)
		}
		subjects = append(subjects, m["subject"].(string))
	}

	if c, _ := f.do(t, "POST", "/api/v1/licenses/accept", map[string]any{"subjects": subjects}, token); c != 400 {
		t.Fatalf("acceptance without a person: %d", c)
	}
	if c, _ := f.do(t, "POST", "/api/v1/licenses/accept", map[string]any{"subjects": []string{"image/agentgateway"}, "accepted_by": "ana@example.test"}, token); c != 400 {
		t.Fatalf("accepting a licence nobody asks for: %d", c)
	}
	code, acc := f.do(t, "POST", "/api/v1/licenses/accept", map[string]any{"subjects": subjects, "accepted_by": "ana@example.test"}, token)
	if code != 201 || acc["needs_acceptance"] != nil {
		t.Fatalf("accept plan: %d %v", code, acc)
	}

	// The change does not render: refused, nothing written.
	before, _ := os.ReadFile(f.cluster)
	checkErr = errors.New("image not found")
	if c, body := f.do(t, "POST", "/api/v1/plans/"+acc["plan_id"].(string)+"/apply", nil, token); c != 422 || !strings.Contains(body["error"].(string), "cluster.yaml unchanged") {
		t.Fatalf("apply of an unrenderable change: %d %v", c, body)
	}
	if after, _ := os.ReadFile(f.cluster); string(after) != string(before) {
		t.Fatal("a refused apply changed cluster.yaml")
	}

	// It renders: applied, with who and when recorded.
	checkErr = nil
	code, acc = f.do(t, "POST", "/api/v1/licenses/accept", map[string]any{"subjects": subjects, "accepted_by": "ana@example.test"}, token)
	if c, _ := f.do(t, "POST", "/api/v1/plans/"+acc["plan_id"].(string)+"/apply", nil, token); c/100 != 2 {
		t.Fatalf("apply: %d", c)
	}
	f.waitPlan(t, acc["plan_id"].(string))
	c, err := config.LoadCluster(f.cluster)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]config.LicenseAcceptance{}
	for _, a := range c.Spec.LicenseAcceptances {
		got[a.Subject] = a
	}
	for _, s := range subjects {
		if a := got[s]; a.AcceptedBy != "ana@example.test" || a.URL == "" || a.AcceptedAt == "" {
			t.Fatalf("%s recorded as %+v", s, a)
		}
	}
	if code, plan = f.do(t, "PUT", "/api/v1/routing", map[string]any{"mode": "observe"}, token); plan["needs_acceptance"] != nil {
		t.Fatalf("licences still needed after acceptance: %d %v", code, plan)
	}
}

// Accepting a licence only records who and when: it applies while another
// plan is being applied, on top of the change that plan wrote, and does not
// reconcile when nothing deployed waited on it.
func TestLicenceAcceptedDuringAnotherApply(t *testing.T) {
	f := newFixture(t)
	v, err := config.ParseVersions(bundle.VersionsYAML)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	s := New(Config{ClusterPath: f.cluster, CatalogPath: "../../examples/catalog.yaml", Secrets: f.store, StateDir: t.TempDir(),
		Versions: v,
		Apply:    func(context.Context, io.Writer) error { f.applied.Add(1); <-release; return nil },
	})
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	defer close(release)

	code, acc := f.do(t, "POST", "/api/v1/licenses/accept", map[string]any{"subjects": []string{"image/vllm-glm53"}, "accepted_by": "ana@example.test"}, token)
	if code != 201 {
		t.Fatalf("accept plan: %d %v", code, acc)
	}
	code, other := f.do(t, "PUT", "/api/v1/routing", map[string]any{"mode": "active", "defaults": map[string]any{"fast": "qwen-flash"}}, token)
	if code != 201 {
		t.Fatalf("routing plan: %d %v", code, other)
	}
	if c, _ := f.do(t, "POST", "/api/v1/plans/"+other["plan_id"].(string)+"/apply", nil, token); c != 202 {
		t.Fatalf("routing apply: %d", c)
	}
	code, done := f.do(t, "POST", "/api/v1/plans/"+acc["plan_id"].(string)+"/apply", nil, token)
	if code != 200 || done["status"] != PlanApplied {
		t.Fatalf("acceptance during another apply: %d %v", code, done)
	}
	c, err := config.LoadCluster(f.cluster)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range c.Spec.LicenseAcceptances {
		found = found || (a.Subject == "image/vllm-glm53" && a.AcceptedBy == "ana@example.test")
	}
	if !found || c.Spec.Routing.Mode != "active" {
		t.Fatalf("cluster.yaml lost a change: accepted %v, mode %q", found, c.Spec.Routing.Mode)
	}
	if n := f.applied.Load(); n != 1 {
		t.Fatalf("reconciles: %d, want only the routing plan's", n)
	}
}
