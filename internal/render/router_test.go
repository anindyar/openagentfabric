package render

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"openagentfabric.dev/oaf/internal/config"
)

func routerFiles(t *testing.T, in Input) (vsr, envoy, gw, compose string, ok bool) {
	t.Helper()
	for _, p := range mustRender(t, in) {
		if p.Name == ProjectRouter {
			ok = true
			vsr = string(fileOf(t, in, p.Node, p.Name, "vsr.yaml"))
			envoy = string(fileOf(t, in, p.Node, p.Name, "envoy.yaml"))
			compose = string(fileOf(t, in, p.Node, p.Name, "compose.yaml"))
		}
		if p.Name == ProjectGateway {
			gw = string(fileOf(t, in, p.Node, p.Name, "config.yaml"))
		}
	}
	return
}

// Observe: vSR decides, every decision targets fast, Envoy only knows the
// fast model; agentgateway sends auto to the router and traces the decision.
func TestRouterObserve(t *testing.T) {
	vsr, envoy, gw, compose, ok := routerFiles(t, pairInput(t))
	if !ok {
		t.Fatal("no router project for a cluster with an active rule")
	}
	var cfg struct {
		Routing struct {
			Decisions []struct {
				Name      string `yaml:"name"`
				ModelRefs []struct {
					Model string `yaml:"model"`
				} `yaml:"modelRefs"`
			} `yaml:"decisions"`
		} `yaml:"routing"`
	}
	if err := yaml.Unmarshal([]byte(vsr), &cfg); err != nil {
		t.Fatal(err)
	}
	for _, d := range cfg.Routing.Decisions {
		if d.ModelRefs[0].Model != "fast" {
			t.Fatalf("observe: decision %s targets %s", d.Name, d.ModelRefs[0].Model)
		}
	}
	if strings.Contains(envoy, "exact: think") {
		t.Fatal("observe: Envoy must not route to think")
	}
	for _, want := range []string{"baseUrl: http://192.168.1.11:8899/v1", `oaf.route: response.headers["x-vsr-selected-decision"]`, `oaf.served_by: response.headers["x-oaf-served-by"]`} {
		if !strings.Contains(gw, want) {
			t.Errorf("gateway config lacks %q", want)
		}
	}
	// The pair example's think rule has a reasoning signal: the encoder.
	for _, want := range []string{"/var/lib/oaf/models/vsr-encoder:/app/models/Vela-1.0-Encoder-307M-Embedding:ro", "mem_limit: 1792m", "HF_HUB_OFFLINE"} {
		if !strings.Contains(compose, want) {
			t.Errorf("router compose lacks %q", want)
		}
	}
}

// Active: think gets a route, a health-checked cluster, and an upstream Lua
// that names it in the body; the route Lua knows fast and fallback.
func TestRouterActive(t *testing.T) {
	in := pairInput(t)
	in.Cluster.Spec.Routing.Mode = "active"
	vsr, envoy, _, _, _ := routerFiles(t, in)
	if !strings.Contains(vsr, "- model: think") {
		t.Fatal("active: the think decision must target think")
	}
	for _, want := range []string{"exact: think", "path: /health", `"model":"' .. "think" .. '"`, `local FAST, FALLBACK = "fast", "fast"`, "failure_mode_allow: true", "response_body_mode: NONE"} {
		if !strings.Contains(envoy, want) {
			t.Errorf("envoy config lacks %q", want)
		}
	}
	if strings.Contains(envoy, "envoy.clusters.aggregate") {
		t.Error("fallback is done by the route Lua, not aggregate clusters (they skip upstream filters)")
	}
}

// Keyword/context only: no encoder, small memory; no rules: no router at all.
func TestRouterWithoutEncoderOrRules(t *testing.T) {
	in := pairInput(t)
	in.Cluster.Spec.Routing.Rules = []config.RoutingRule{{Name: "think", Target: "think",
		Match: config.RuleMatch{Signals: []config.Signal{{Type: config.SignalKeyword, Keywords: []string{"prove"}}, {Type: config.SignalContext, MinTokens: 2000}}}}}
	_, _, _, compose, ok := routerFiles(t, in)
	if !ok || strings.Contains(compose, "vsr-encoder") || !strings.Contains(compose, "mem_limit: 128m") {
		t.Fatalf("keyword-only router:\n%s", compose)
	}

	in.Cluster.Spec.Routing.Rules = nil
	_, _, gw, _, ok := routerFiles(t, in)
	if ok {
		t.Fatal("router rendered with no rules")
	}
	if !strings.Contains(gw, "baseUrl: http://192.168.1.11:8000/v1\n        model: fast") {
		t.Fatalf("auto must go straight to fast without a router:\n%s", gw)
	}
}
