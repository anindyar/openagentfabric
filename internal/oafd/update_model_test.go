package oafd

import (
	"strings"
	"testing"
)

// A deployment's context and memory share change by plan; 0 restores the
// profile's context.
func TestUpdateModel(t *testing.T) {
	f := newFixture(t)
	code, plan := f.do(t, "PUT", "/api/v1/models/does-not-exist", map[string]any{"max_model_len": 4096}, token)
	if code != 404 {
		t.Fatalf("unknown model: %d %v", code, plan)
	}
	_, list := f.do(t, "GET", "/api/v1/models", nil, token)
	models, _ := list["models"].([]any)
	if len(models) == 0 {
		t.Fatal("fixture has no models")
	}
	name := models[0].(map[string]any)["name"].(string)
	if c, _ := f.do(t, "PUT", "/api/v1/models/"+name, map[string]any{}, token); c != 400 {
		t.Fatalf("empty change: %d", c)
	}
	code, plan = f.do(t, "PUT", "/api/v1/models/"+name, map[string]any{"max_model_len": 4096}, token)
	if code/100 != 2 || !strings.Contains(plan["diff"].(string), "maxModelLen: 4096") {
		t.Fatalf("plan: %d %v", code, plan)
	}
}
