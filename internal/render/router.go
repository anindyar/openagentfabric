package render

import (
	"fmt"
	"path"
	"strconv"

	"openagentfabric.dev/oaf/internal/config"
)

// The router project serves `model: auto` (ADR-0003). vSR is an Envoy
// external processor: Envoy buffers each request, asks vSR which model it
// should go to (vSR answers with the x-selected-model header and rewrites the
// body's model), and routes to that vLLM. agentgateway keeps auth, tracing
// and every explicit model name; only `auto` goes through here.
//
// The configs are generated from the enabled deployments and the active
// rules only, so vSR never routes to a model that is not there. Each model
// cluster is health-checked, and a route falls over to routing.defaults
// .fallback when its model is down (and to the fast model when vSR is). In
// observe mode vSR still decides (the decision is traced as oaf.route) but
// every decision targets the fast model.

const (
	vsrExtProcPort = 50051
	// Response headers vSR sets; agentgateway copies them into its trace.
	headerVSRDecision = "x-vsr-selected-decision"
	headerVSRModel    = "x-vsr-selected-model"
	// Set by Envoy: the model that actually served (differs from vSR's choice
	// when that model was down).
	headerServedBy = "x-oaf-served-by"
	// defaultDecision is the catch-all that sends everything else to fast.
	defaultDecision = "default"
)

// Built-in examples for the reasoning (complexity) signal: vSR embeds the
// prompt and compares it with these. Infrastructure and security work, the
// appliance's own domain.
var (
	complexityHard = []string{
		"Design a distributed system that guarantees exactly-once delivery across regions and explain the trade-offs.",
		"Prove that this algorithm terminates and analyse its time complexity.",
		"Investigate this multi-stage intrusion: correlate the logs, build a timeline and propose containment.",
		"Write a detailed migration plan for moving a stateful Kubernetes workload between clusters with zero downtime.",
		"Review this architecture for single points of failure and propose a highly available redesign.",
		"Debug why this service deadlocks under load; reason about the locking order step by step.",
	}
	complexityEasy = []string{
		"Say hello.",
		"What time is it in Tokyo?",
		"How many open findings do we have?",
		"Translate 'thank you' to French.",
		"List the systems tagged prod.",
		"What does this error message mean?",
	}
)

func (r *renderer) routes() config.Routes {
	return r.Cluster.ActiveRoutes(r.Catalog, r.Features)
}

// router renders vSR and its Envoy front end on the gateway node.
func (r *renderer) router(n config.Node, rt config.Routes) (Project, error) {
	p := Project{Node: n.Name, Name: ProjectRouter}
	v := r.Versions.Version
	vsrImg, err := r.image("vsr")
	if err != nil {
		return p, err
	}
	envoyImg, err := r.image("envoy")
	if err != nil {
		return p, err
	}
	observe := r.Cluster.Spec.Routing.Mode != "active"

	backends := r.chatBackends()
	vsr := newService(vsrImg)
	vsr.Environment = map[string]string{
		// Model files come from OAF (bundle artifacts), never from the hub at run time.
		"HF_HUB_OFFLINE":       "1",
		"TRANSFORMERS_OFFLINE": "1",
	}
	vsr.Volumes = []string{"./vsr.yaml:/app/config.yaml:ro"}
	if rt.NeedsEncoder {
		a, ok := r.Versions.Artifact(config.RouterEncoderArtifact)
		if !ok {
			return p, fmt.Errorf("reasoning signals need bundle artifact %q", config.RouterEncoderArtifact)
		}
		vsr.Volumes = append(vsr.Volumes, path.Join(modelsPath(n), a.Name)+":"+a.MountAs+":ro")
		vsr.MemLimit = strconv.Itoa(a.MemoryMiB) + "m"
		vsr.MemswapLimit = vsr.MemLimit
	}

	envoy := newService(envoyImg)
	envoy.Ports = []string{portMap(config.PortRouter, config.PortRouter)}
	envoy.Volumes = []string{"./envoy.yaml:/etc/envoy/envoy.yaml:ro"}
	envoy.DependsOn = map[string]dependency{"vsr": {Condition: "service_started"}}

	vsrCfg, err := yamlFile(v, "vsr.yaml", r.vsrConfig(rt, backends, observe))
	if err != nil {
		return p, err
	}
	envoyCfg, err := yamlFile(v, "envoy.yaml", envoyConfig(rt, backends, observe))
	if err != nil {
		return p, err
	}
	cf := &composeFile{Services: map[string]*service{"vsr": vsr, "envoy": envoy}}
	return finish(p, v, cf, newEnvFile(), vsrCfg, envoyCfg)
}

// chatBackend is an enabled chat deployment's API endpoint (its rank-0 node).
type chatBackend struct {
	name, host string
	port       int
}

func (r *renderer) chatBackends() []chatBackend {
	var out []chatBackend
	for i, d := range r.Cluster.Spec.Models {
		if !d.IsEnabled() {
			continue
		}
		if m, ok := r.Catalog.Lookup(d.CatalogRef); ok && m.IsEmbedding() {
			continue
		}
		nodes := r.Cluster.PlacementNodes(d.Placement)
		n, _ := r.Cluster.Node(nodes[0])
		out = append(out, chatBackend{name: d.Name, host: n.Address, port: r.Cluster.ModelPort(i)})
	}
	return out
}

// vsrConfig is vSR's canonical v0.3 config (the format vllm-sr v0.4.0 reads).
func (r *renderer) vsrConfig(rt config.Routes, backends []chatBackend, observe bool) map[string]any {
	var models, cards []map[string]any
	for _, b := range backends {
		models = append(models, map[string]any{
			"name": b.name,
			"backend_refs": []map[string]any{{
				"name": b.name, "provider": "vllm", "endpoint": hostPort(b.host, b.port), "protocol": "http", "weight": 100,
			}},
		})
		cards = append(cards, map[string]any{"name": b.name})
	}
	var keywords, contexts, complexity []map[string]any
	var decisions []map[string]any
	for i, rule := range rt.Rules {
		var conds []map[string]any
		for j, s := range rule.Signals {
			name := fmt.Sprintf("%s-%d", rule.Name, j)
			switch s.Type {
			case config.SignalKeyword:
				keywords = append(keywords, map[string]any{"name": name, "operator": "OR", "keywords": s.Keywords, "case_sensitive": s.CaseSensitive})
				conds = append(conds, map[string]any{"type": "keyword", "name": name})
			case config.SignalContext:
				contexts = append(contexts, map[string]any{"name": name, "min_tokens": strconv.Itoa(s.MinTokens)})
				conds = append(conds, map[string]any{"type": "context", "name": name})
			case config.SignalReasoning:
				th := 0.1
				if s.Threshold != nil {
					th = *s.Threshold
				}
				complexity = append(complexity, map[string]any{"name": name, "threshold": th,
					"hard": map[string]any{"candidates": complexityHard}, "easy": map[string]any{"candidates": complexityEasy}})
				conds = append(conds, map[string]any{"type": "complexity", "name": name + ":hard"})
			}
		}
		target := rule.Target
		if observe {
			target = rt.Fast
		}
		decisions = append(decisions, map[string]any{
			"name":        rule.Name,
			"description": "OAF rule " + rule.Name + " → " + rule.Target,
			"priority":    1000 - 10*i,
			"rules":       map[string]any{"operator": "OR", "conditions": conds},
			"modelRefs":   []map[string]any{{"model": target, "use_reasoning": false}},
		})
	}
	decisions = append(decisions, map[string]any{
		"name":        defaultDecision,
		"description": "Everything else goes to the fast model.",
		"priority":    1,
		"rules":       map[string]any{"operator": "AND", "conditions": []any{}},
		"modelRefs":   []map[string]any{{"model": rt.Fast, "use_reasoning": false}},
	})
	signals := map[string]any{}
	if len(keywords) > 0 {
		signals["keywords"] = keywords
	}
	if len(contexts) > 0 {
		signals["context"] = contexts
	}
	if len(complexity) > 0 {
		signals["complexity"] = complexity
	}
	return map[string]any{
		"version": "v0.3",
		// vSR's own launcher renders an Envoy from this; OAF renders Envoy
		// itself (envoy.yaml), so the listener only documents the port.
		"listeners": []map[string]any{{"name": "router", "address": "0.0.0.0", "port": config.PortRouter, "timeout": "1200s"}},
		"providers": map[string]any{"defaults": map[string]any{"model": rt.Fast}, "models": models},
		"routing": map[string]any{
			"modelCards": cards,
			"signals":    signals,
			"decisions":  decisions,
		},
	}
}

// Lua run by Envoy. Proven on a DGX Spark with two real models: simple and
// complex prompts, streaming, the chosen model down, and vSR down.
//
// routeLua runs after vSR (downstream): it keeps vSR's choice unless that
// model fails its health check, in which case it routes to the fallback; with
// vSR down (no choice) it routes to the fast model. It records the model that
// actually serves the request as x-oaf-served-by, which agentgateway traces.
const routeLua = `
local FAST, FALLBACK = %q, %q
local function healthy(h, model)
  local _, body = h:httpCall("envoy_admin", {[":method"] = "GET", [":path"] = "/clusters", [":authority"] = "admin"}, "", 1000)
  if body == nil then return true end
  local prefix = model .. "::"
  for line in body:gmatch("[^\n]+") do
    if line:sub(1, #prefix) == prefix and line:find("::health_flags::", 1, true) then
      return line:find("::health_flags::healthy", 1, true) ~= nil
    end
  end
  return true
end
function envoy_on_request(h)
  local target = h:headers():get("x-selected-model") or FAST
  if target ~= FALLBACK and not healthy(h, target) then target = FALLBACK end
  h:headers():replace("x-selected-model", target)
  h:streamInfo():dynamicMetadata():set("oaf", "served_by", target)
  h:clearRouteCache()
end
function envoy_on_response(h)
  local md = h:streamInfo():dynamicMetadata():get("oaf")
  if md ~= nil and md["served_by"] ~= nil then h:headers():replace("x-oaf-served-by", md["served_by"]) end
end
`

// modelLua runs per model cluster (upstream): the body names that cluster's
// model, so a request swapped to the fallback (or one vSR never saw, still
// "auto") is one the vLLM serves. vSR's own body rewrite is not visible to a
// downstream filter, so this has to be an upstream one.
const modelLua = `
function envoy_on_request(h)
  local body = h:body(true)
  if body == nil then return end
  local s = body:getBytes(0, body:length())
  local out, n = string.gsub(s, '"model"%%s*:%%s*"[^"]*"', '"model":"' .. %q .. '"', 1)
  if n > 0 then
    body:setBytes(out)
    h:headers():replace("content-length", tostring(#out))
  end
end
`

const envoyAdminPort = 9901 // loopback only: the route Lua reads cluster health from it

// envoyConfig routes on vSR's x-selected-model (after routeLua's health
// check) to a cluster per model.
func envoyConfig(rt config.Routes, backends []chatBackend, observe bool) map[string]any {
	timeout := "1200s"
	used := map[string]bool{rt.Fast: true, rt.Fallback: true}
	if !observe {
		for _, rule := range rt.Rules {
			used[rule.Target] = true
		}
	}
	var routes, clusters []map[string]any
	clusters = append(clusters, map[string]any{
		"name": "vsr", "type": "STRICT_DNS", "connect_timeout": "5s",
		"typed_extension_protocol_options": map[string]any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": map[string]any{
				"@type":                "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions",
				"explicit_http_config": map[string]any{"http2_protocol_options": map[string]any{}},
			},
		},
		"load_assignment": endpoints("vsr", "vsr", vsrExtProcPort),
	}, map[string]any{
		"name": "envoy_admin", "type": "STATIC", "connect_timeout": "1s",
		"load_assignment": endpoints("envoy_admin", "127.0.0.1", envoyAdminPort),
	})
	for _, b := range backends {
		if !used[b.name] {
			continue
		}
		routes = append(routes, map[string]any{
			"match": map[string]any{"prefix": "/", "headers": []map[string]any{{"name": "x-selected-model", "string_match": map[string]any{"exact": b.name}}}},
			"route": map[string]any{"cluster": b.name, "timeout": timeout, "auto_host_rewrite": true},
		})
		clusters = append(clusters, map[string]any{
			"name": b.name, "type": "STRICT_DNS", "connect_timeout": "5s",
			"load_assignment": endpoints(b.name, b.host, b.port),
			// vLLM's /health needs no key.
			"health_checks": []map[string]any{{
				"timeout": "2s", "interval": "5s", "unhealthy_threshold": 2, "healthy_threshold": 1,
				"http_health_check": map[string]any{"path": "/health"},
			}},
			"typed_extension_protocol_options": map[string]any{
				"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": map[string]any{
					"@type":                "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions",
					"explicit_http_config": map[string]any{"http_protocol_options": map[string]any{}},
					"http_filters": []map[string]any{
						luaFilter(fmt.Sprintf(modelLua, b.name)),
						{"name": "envoy.filters.http.upstream_codec", "typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.upstream_codec.v3.UpstreamCodec"}},
					},
				},
			},
		})
	}
	routes = append(routes, map[string]any{
		"match": map[string]any{"prefix": "/"},
		"route": map[string]any{"cluster": rt.Fast, "timeout": timeout, "auto_host_rewrite": true},
	})

	return map[string]any{
		"admin": map[string]any{"address": map[string]any{"socket_address": map[string]any{"address": "127.0.0.1", "port_value": envoyAdminPort}}},
		"static_resources": map[string]any{
			"listeners": []map[string]any{{
				"name":    "router",
				"address": map[string]any{"socket_address": map[string]any{"address": "0.0.0.0", "port_value": config.PortRouter}},
				"filter_chains": []map[string]any{{"filters": []map[string]any{{
					"name": "envoy.filters.network.http_connection_manager",
					"typed_config": map[string]any{
						"@type":               "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
						"stat_prefix":         "router",
						"stream_idle_timeout": timeout,
						"route_config": map[string]any{"name": "router", "virtual_hosts": []map[string]any{{
							"name": "models", "domains": []string{"*"}, "routes": routes,
						}}},
						"http_filters": []map[string]any{
							{
								"name": "envoy.filters.http.ext_proc",
								"typed_config": map[string]any{
									"@type":               "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor",
									"allow_mode_override": true,
									// vSR down: requests still flow, to the fast model.
									"failure_mode_allow": true,
									"grpc_service":       map[string]any{"envoy_grpc": map[string]any{"cluster_name": "vsr"}},
									// Response bodies are not sent, so streaming stays streaming;
									// vSR's decision headers still come back.
									"processing_mode": map[string]any{
										"request_header_mode": "SEND", "response_header_mode": "SEND",
										"request_body_mode": "BUFFERED", "response_body_mode": "NONE",
									},
									"message_timeout": "30s",
								},
							},
							luaFilter(fmt.Sprintf(routeLua, rt.Fast, rt.Fallback)),
							{"name": "envoy.filters.http.router", "typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}},
						},
					},
				}}}},
			}},
			"clusters": clusters,
		},
	}
}

func luaFilter(code string) map[string]any {
	return map[string]any{"name": "envoy.filters.http.lua", "typed_config": map[string]any{
		"@type":               "type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua",
		"default_source_code": map[string]any{"inline_string": code},
	}}
}

func endpoints(cluster, host string, port int) map[string]any {
	return map[string]any{
		"cluster_name": cluster,
		"endpoints": []map[string]any{{"lb_endpoints": []map[string]any{{
			"endpoint": map[string]any{"address": map[string]any{"socket_address": map[string]any{"address": host, "port_value": port}}},
		}}}},
	}
}
