package oafd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/secrets"
)

const token = "test-management-token"

type fixture struct {
	srv     *httptest.Server
	cluster string
	store   secrets.Store
	applied atomic.Int32
	fail    atomic.Bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{cluster: filepath.Join(dir, "cluster.yaml"), store: secrets.Store{Dir: filepath.Join(dir, "secrets")}}
	if err := os.WriteFile(f.cluster, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Set(AdminSecretPrefix+"garrison", token); err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		ClusterPath: f.cluster, CatalogPath: "../../examples/catalog.yaml", Secrets: f.store, StateDir: dir,
		Apply: func(_ context.Context, w io.Writer) error {
			fmt.Fprintln(w, "update spark-1/gateway")
			f.applied.Add(1)
			if f.fail.Load() {
				return fmt.Errorf("node unreachable")
			}
			return nil
		},
		Status: func(context.Context) (any, error) { return StatusView{Cluster: "test", Nodes: []NodeView{}}, nil },
	})
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) do(t *testing.T, method, path string, body any, tok string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *fixture) waitPlan(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, p := f.do(t, "GET", "/api/v1/plans/"+id, nil, token)
		if p["status"] != PlanApplying || time.Now().After(deadline) {
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAuth(t *testing.T) {
	f := newFixture(t)
	if c, _ := f.do(t, "GET", "/healthz", nil, ""); c != 200 {
		t.Fatalf("healthz: %d", c)
	}
	for _, tok := range []string{"", "wrong"} {
		if c, _ := f.do(t, "GET", "/api/v1/status", nil, tok); c != 401 {
			t.Fatalf("token %q: %d", tok, c)
		}
	}
	if c, body := f.do(t, "GET", "/api/v1/status", nil, token); c != 200 || body["cluster"] != "test" {
		t.Fatalf("status: %d %v", c, body)
	}
}

// A provider without guardrails plans with the exact warning; its key is
// stored, never echoed; applying writes cluster.yaml and reconciles; a second
// plan made against the old document is refused with 409.
func TestProviderPlanApplyAndConflict(t *testing.T) {
	f := newFixture(t)
	c, plan := f.do(t, "POST", "/api/v1/providers", map[string]any{
		"name": "openai", "type": "openai", "allow_egress": true, "api_key": "sk-test-key-value",
		"models": []map[string]string{{"name": "gpt-4o"}},
	}, token)
	if c != 201 {
		t.Fatalf("plan: %d %v", c, plan)
	}
	raw, _ := json.Marshal(plan)
	if strings.Contains(string(raw), "sk-test-key-value") {
		t.Fatal("the API key is echoed")
	}
	if !strings.Contains(string(raw), config.NoGuardrailWarning) {
		t.Fatalf("no guardrail warning in %s", raw)
	}
	if !strings.Contains(plan["diff"].(string), "+      - name: gpt-4o") && !strings.Contains(plan["diff"].(string), "gpt-4o") {
		t.Fatalf("diff: %s", plan["diff"])
	}
	if v, ok, _ := f.store.Lookup("provider-openai"); !ok || v != "sk-test-key-value" {
		t.Fatal("key not stored as provider-openai")
	}
	before, _ := os.ReadFile(f.cluster)
	if strings.Contains(string(before), "gpt-4o") {
		t.Fatal("planning changed cluster.yaml")
	}

	// A second plan, made now, goes stale once the first is applied.
	_, stale := f.do(t, "PUT", "/api/v1/routing", map[string]any{"mode": "active", "defaults": map[string]string{"fast": "qwen-flash"}, "rules": []any{}}, token)

	id := plan["plan_id"].(string)
	if c, body := f.do(t, "POST", "/api/v1/plans/"+id+"/apply", nil, token); c != 202 {
		t.Fatalf("apply: %d %v", c, body)
	}
	done := f.waitPlan(t, id)
	if done["status"] != PlanApplied || f.applied.Load() != 1 {
		t.Fatalf("after apply: %v (applies %d)", done, f.applied.Load())
	}
	if steps, _ := done["steps"].([]any); len(steps) == 0 {
		t.Fatalf("no steps: %v", done)
	}
	after, _ := os.ReadFile(f.cluster)
	if !strings.Contains(string(after), "gpt-4o") || !strings.Contains(string(after), "#") {
		t.Fatal("cluster.yaml not written (or comments lost)")
	}
	if c, _ := f.do(t, "POST", "/api/v1/plans/"+id+"/apply", nil, token); c != 409 {
		t.Fatalf("re-applying an applied plan: %d", c)
	}
	if c, body := f.do(t, "POST", "/api/v1/plans/"+stale["plan_id"].(string)+"/apply", nil, token); c != 409 {
		t.Fatalf("stale plan: %d %v", c, body)
	}

	_, list := f.do(t, "GET", "/api/v1/providers", nil, token)
	ps := list["providers"].([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["has_key"] != true {
		t.Fatalf("providers: %v", list)
	}
}

// Invalid changes are planned with errors and cannot be applied.
func TestInvalidPlan(t *testing.T) {
	f := newFixture(t)
	c, plan := f.do(t, "POST", "/api/v1/providers", map[string]any{
		"name": "claude", "type": "anthropic", "allow_egress": false, "models": []map[string]string{{"name": "claude-x"}},
	}, token)
	if c != 422 || plan["status"] != PlanPending {
		t.Fatalf("plan: %d %v", c, plan)
	}
	raw, _ := json.Marshal(plan["errors"])
	if !strings.Contains(string(raw), "allowEgress") || !strings.Contains(string(raw), "no API key yet") {
		t.Fatalf("errors: %s", raw)
	}
	if c, _ := f.do(t, "POST", "/api/v1/plans/"+plan["plan_id"].(string)+"/apply", nil, token); c != 422 {
		t.Fatalf("applying an invalid plan: %d", c)
	}
	if f.applied.Load() != 0 {
		t.Fatal("an invalid plan was applied")
	}
}

// The basic guardrail is added and attached to a provider in one plan, and
// removing it detaches it in one plan.
func TestGuardrailTemplateAndAttach(t *testing.T) {
	f := newFixture(t)
	_, p := f.do(t, "POST", "/api/v1/providers", map[string]any{
		"name": "lab", "type": "openai-compatible", "endpoint": "http://llm.example.internal:8010/v1", "allow_egress": true,
		"models": []map[string]string{{"name": "lab-model"}},
	}, token)
	f.do(t, "POST", "/api/v1/plans/"+p["plan_id"].(string)+"/apply", nil, token)
	f.waitPlan(t, p["plan_id"].(string))

	c, plan := f.do(t, "POST", "/api/v1/guardrails", map[string]any{"template": "basic", "attach": map[string]any{"providers": []string{"lab"}}}, token)
	if c != 201 {
		t.Fatalf("plan: %d %v", c, plan)
	}
	raw, _ := json.Marshal(plan)
	if strings.Contains(string(raw), config.NoGuardrailWarning) {
		t.Fatal("still warned after attaching basic")
	}
	f.do(t, "POST", "/api/v1/plans/"+plan["plan_id"].(string)+"/apply", nil, token)
	f.waitPlan(t, plan["plan_id"].(string))

	_, list := f.do(t, "GET", "/api/v1/guardrails", nil, token)
	gs := list["guardrails"].([]any)
	if len(gs) != 1 {
		t.Fatalf("guardrails: %v", list)
	}
	g := gs[0].(map[string]any)
	if at := g["attach"].(map[string]any); at["providers"].([]any)[0] != "lab" {
		t.Fatalf("attach: %v", g)
	}
	if b := list["builtins"].([]any); len(b) != len(config.GuardrailBuiltins) {
		t.Fatalf("builtins: %v", b)
	}

	// A UI-shaped policy with aliases converts to the pinned detector names.
	c, plan = f.do(t, "POST", "/api/v1/guardrails", map[string]any{"name": "pii", "rules": []map[string]string{
		{"builtin": "credit_card", "action": "mask", "on": "response"}, {"builtin": "phone", "action": "mask", "on": "response"},
		{"regex": "secret-[0-9]+", "action": "reject", "on": "request"},
	}}, token)
	if c != 201 {
		t.Fatalf("pii: %d %v", c, plan)
	}
	if d := plan["diff"].(string); !strings.Contains(d, "creditCard") || !strings.Contains(d, "phoneNumber") {
		t.Fatalf("aliases not normalized: %s", d)
	}

	c, plan = f.do(t, "DELETE", "/api/v1/guardrails/basic", nil, token)
	if c != 201 {
		t.Fatalf("remove: %d %v", c, plan)
	}
	raw, _ = json.Marshal(plan)
	if !strings.Contains(string(raw), config.NoGuardrailWarning) {
		t.Fatal("removing the only policy should bring the warning back")
	}
}

func TestModelsAndRouting(t *testing.T) {
	f := newFixture(t)
	_, list := f.do(t, "GET", "/api/v1/models", nil, token)
	if ms := list["models"].([]any); len(ms) == 0 || ms[0].(map[string]any)["catalog_ref"] == "" {
		t.Fatalf("models: %v", list)
	}
	if c, plan := f.do(t, "DELETE", "/api/v1/models/bge-m3", nil, token); c != 201 || !strings.Contains(plan["diff"].(string), "-    - name: bge-m3") {
		t.Fatalf("remove model: %d %v", c, plan)
	}
	if c, _ := f.do(t, "DELETE", "/api/v1/models/nope", nil, token); c != 404 {
		t.Fatalf("remove missing model: %d", c)
	}
	if c, plan := f.do(t, "POST", "/api/v1/models", map[string]any{"name": "qwen-flash", "catalog_ref": "bge-m3"}, token); c != 409 {
		t.Fatalf("duplicate model: %d %v", c, plan)
	}
	_, r := f.do(t, "GET", "/api/v1/routing", nil, token)
	if r["mode"] == "" || r["defaults"].(map[string]any)["fast"] != "qwen-flash" {
		t.Fatalf("routing: %v", r)
	}
	_, cat := f.do(t, "GET", "/api/v1/catalog", nil, token)
	if len(cat["models"].([]any)) == 0 {
		t.Fatalf("catalog: %v", cat)
	}
	_, cfg := f.do(t, "GET", "/api/v1/config", nil, token)
	if !strings.Contains(cfg["cluster_yaml"].(string), "spec:") {
		t.Fatalf("config: %v", cfg)
	}
}

func TestEvents(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest("GET", f.srv.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	f.fail.Store(true)
	_, plan := f.do(t, "DELETE", "/api/v1/models/bge-m3", nil, token)
	f.do(t, "POST", "/api/v1/plans/"+plan["plan_id"].(string)+"/apply", nil, token)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.HasPrefix(l, "data: ") && strings.Contains(l, `"status":"failed"`) {
				var e Event
				if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &e); err != nil || e.Type != "plan" || e.At == "" {
					t.Fatalf("event: %s", l)
				}
				done := f.waitPlan(t, plan["plan_id"].(string))
				if done["status"] != PlanFailed || !strings.Contains(done["error"].(string), "node unreachable") {
					t.Fatalf("failed plan: %v", done)
				}
				return
			}
		case <-deadline:
			t.Fatal("no failed event")
		}
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"
	b := "a\nb\nc\nX\ne\nf\ng\nh\ni\nj\nk\n"
	d := unifiedDiff(a, b, "old", "new")
	for _, want := range []string{"--- old", "+++ new", "-d", "+X", "+k", " c"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff lacks %q:\n%s", want, d)
		}
	}
	if unifiedDiff(a, a, "x", "y") != "" {
		t.Fatal("no-change diff is not empty")
	}
}

// A node's desktop mode is a plan like any other change.
func TestNodeHeadlessPlan(t *testing.T) {
	f := newFixture(t)
	c, plan := f.do(t, "PUT", "/api/v1/nodes/spark-1/headless", map[string]any{"headless": true}, token)
	if c != 201 || !strings.Contains(plan["diff"].(string), "headless: true") {
		t.Fatalf("headless plan: %d %v", c, plan)
	}
	if !strings.Contains(plan["summary"].(string), "headless") {
		t.Fatalf("summary: %v", plan["summary"])
	}
	if c, _ := f.do(t, "PUT", "/api/v1/nodes/nope/headless", map[string]any{"headless": true}, token); c != 404 {
		t.Fatalf("unknown node: %d", c)
	}
	if c, _ := f.do(t, "PUT", "/api/v1/nodes/spark-1/headless", map[string]any{}, token); c != 400 {
		t.Fatalf("missing headless: %d", c)
	}
	if c, _ := f.do(t, "PUT", "/api/v1/nodes/spark-1/headless", map[string]any{"headless": true}, ""); c != 401 {
		t.Fatalf("no token: %d", c)
	}
}
