package render

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"

	"openagentfabric.dev/oaf/internal/config"
)

// Attribution headers an app sets on a model call (lower case, as the
// gateway's CEL sees them). Advisory: they label traces, they grant nothing.
const (
	HeaderUser    = "x-oaf-user"
	HeaderSession = "x-oaf-session"
	// HeaderStep names the app step that made the call (e.g. "chat pass 2",
	// "case draft"), so a trace shows which part of an app used which model.
	HeaderStep = "x-oaf-step"
)

// AutoModel is the model name that vSR routes (the router project). With no
// active routing rule the gateway maps it straight to routing.defaults.fast.
const AutoModel = "auto"

// gateway renders agentgateway: one OpenAI-compatible listener that serves
// every enabled model by name, authenticated with the cluster's API keys and
// traced to Langfuse.
//
// Secrets stay out of config.yaml: agentgateway shell-expands $VARS in its
// config file, and compose passes them from .env. Client keys are stored only
// as SHA-256 hashes.
func (r *renderer) gateway(n config.Node) (Project, error) {
	p := Project{Node: n.Name, Name: ProjectGateway}
	v := r.Versions.Version
	env := newEnvFile()
	img, err := r.image("agentgateway")
	if err != nil {
		return p, err
	}

	cfg := map[string]any{}
	settings := map[string]any{
		"adminAddr":     "0.0.0.0:" + strconv.Itoa(config.PortGatewayAdmin),
		"statsAddr":     "0.0.0.0:" + strconv.Itoa(config.PortGatewayStats),
		"readinessAddr": "0.0.0.0:" + strconv.Itoa(config.PortGatewayReadiness),
	}
	cfg["config"] = settings

	containerEnv := map[string]string{}
	if obs, ok := r.observabilityNode(); ok && r.langfuseEnabled() {
		pk, err := r.secret(SecretLangfusePublicKey)
		if err != nil {
			return p, err
		}
		sk, err := r.secret(SecretLangfuseSecretKey)
		if err != nil {
			return p, err
		}
		auth := base64.StdEncoding.EncodeToString([]byte("pk-lf-" + pk + ":sk-lf-" + sk))
		containerEnv["LANGFUSE_OTLP_AUTH"] = env.ref("LANGFUSE_OTLP_AUTH", auth)
		settings["tracing"] = r.tracing(obs)
	}

	llm := map[string]any{"port": r.Cluster.GatewayPort()}
	if pol, err := r.apiKeyPolicy(); err != nil {
		return p, err
	} else if pol != nil {
		llm["policies"] = map[string]any{"apiKey": pol}
	}
	models, err := r.gatewayModels(n)
	if err != nil {
		return p, err
	}
	if len(models) > 0 {
		key, err := r.secret(SecretVLLMAPIKey)
		if err != nil {
			return p, err
		}
		containerEnv["VLLM_API_KEY"] = env.ref("VLLM_API_KEY", key)
	}
	// Guardrails on local deployments. "auto" carries those of every model it
	// can reach: the fast model, or behind the router the fast and fallback
	// models and every rule target (the union, so the strictest applies).
	byName := map[string][]string{}
	for _, d := range r.Cluster.Spec.Models {
		if d.IsEnabled() && len(d.Guardrails) > 0 {
			byName[d.Name] = d.Guardrails
		}
	}
	for _, m := range models {
		name, _ := m["name"].(string)
		policies := byName[name]
		if name == AutoModel {
			served, _ := m["params"].(map[string]any)["model"].(string)
			policies = byName[served]
			if served == AutoModel { // the router picks among these
				rt := r.routes()
				reach := []string{rt.Fast, rt.Fallback}
				for _, rule := range rt.Rules {
					reach = append(reach, rule.Target)
				}
				policies = unionPolicies(byName, reach)
			}
		}
		g, err := r.guardrailsFor(policies)
		if err != nil {
			return p, fmt.Errorf("model %s: %w", name, err)
		}
		if g != nil {
			m["guardrails"] = g
		}
	}
	external, err := r.providerModels(env, containerEnv)
	if err != nil {
		return p, err
	}
	models = append(models, external...)
	llm["models"] = models
	cfg["llm"] = llm

	s := newService(img)
	s.Command = []string{"--file", "/config/config.yaml"}
	s.Environment = containerEnv
	s.Ports = []string{
		portMap(r.Cluster.GatewayPort(), r.Cluster.GatewayPort()),
		fmt.Sprintf("127.0.0.1:%d:%d", config.PortGatewayAdmin, config.PortGatewayAdmin), // the admin UI has no auth
		portMap(config.PortGatewayStats, config.PortGatewayStats),
		portMap(config.PortGatewayReadiness, config.PortGatewayReadiness),
	}
	s.Volumes = []string{"./config.yaml:/config/config.yaml:ro"}

	cfgFile, err := yamlFile(v, "config.yaml", cfg)
	if err != nil {
		return p, err
	}
	p, err = finish(p, v, &composeFile{Services: map[string]*service{"agentgateway": s}}, env, cfgFile)
	return p, err
}

// tracing exports one OTLP span per LLM call to Langfuse, with the GenAI
// attributes Langfuse reads (as in agentgateway's own Langfuse example).
// config.tracing is deprecated in agentgateway 1.6 in favour of
// frontendPolicies.tracing; it still works at the pinned version.
func (r *renderer) tracing(obs config.Node) map[string]any {
	sample, capture := "1.0", false
	if lf := r.Cluster.Spec.Observability.Langfuse; lf != nil {
		if lf.SampleRate != nil {
			sample = strconv.FormatFloat(*lf.SampleRate, 'f', -1, 64)
		}
		if lf.CaptureContent != nil {
			capture = *lf.CaptureContent
		}
	}
	// An app joins the call to its own trace with a W3C traceparent header;
	// the gateway then exports the call as a child of the app's span.
	fields := map[string]string{
		// Named after the model, so traces with several models read at a glance.
		"span.name":                      `"llm " + llm.requestModel`,
		"gen_ai.system":                  "llm.provider",
		"gen_ai.usage.prompt_tokens":     "llm.inputTokens",
		"gen_ai.usage.completion_tokens": "llm.outputTokens",
		// Apps name the end user and conversation behind each call in these
		// headers; Langfuse files the trace under that user and session. A
		// request without them is still traced, with the fields left out.
		"user.id":    `request.headers["` + HeaderUser + `"]`,
		"session.id": `request.headers["` + HeaderSession + `"]`,
		"oaf.step":   `request.headers["` + HeaderStep + `"]`,
		// model: auto through vSR: the decision (rule name, or "default") and
		// the model it chose. In observe mode the decision is what would have
		// happened; the request went to the fast model.
		"oaf.route":        `response.headers["` + headerVSRDecision + `"]`,
		"oaf.routed_model": `response.headers["` + headerVSRModel + `"]`,
		"oaf.served_by":    `response.headers["` + headerServedBy + `"]`,
	}
	if capture {
		fields["gen_ai.prompt"] = "flattenRecursive(llm.prompt)"
		fields["gen_ai.completion"] = `flattenRecursive(llm.completion.map(c, {"role":"assistant", "content": c}))`
	}
	return map[string]any{
		// agentgateway posts OTLP/HTTP to this URL as given, without
		// appending /v1/traces; Langfuse's base path alone answers 404.
		"otlpEndpoint": "http://" + hostPort(obs.Address, config.PortLangfuse) + "/api/public/otel/v1/traces",
		"otlpProtocol": "http",
		// Header values are CEL expressions: a quoted string literal. The
		// variable is shell-expanded first; base64 never contains a quote.
		"headers":        map[string]string{"Authorization": `"Basic $LANGFUSE_OTLP_AUTH"`},
		"randomSampling": sample,
		"fields":         map[string]any{"add": fields},
	}
}

// apiKeyPolicy requires one of the cluster's API keys on every request. Keys
// are generated per install and referenced by secretRef; the config holds
// only their hashes.
func (r *renderer) apiKeyPolicy() (map[string]any, error) {
	keys := r.Cluster.Spec.Routing.Keys
	if len(keys) == 0 {
		return nil, nil
	}
	var out []map[string]any
	for _, k := range keys {
		val, err := r.secret(k.SecretRef)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(val))
		entry := map[string]any{
			"keyHash":  "sha256:" + hex.EncodeToString(sum[:]),
			"metadata": map[string]string{"name": k.Name},
		}
		if len(k.Models) > 0 {
			entry["allowedModels"] = k.Models
		}
		out = append(out, entry)
	}
	return map[string]any{
		"mode":     "strict",
		"location": map[string]any{"header": map[string]string{"name": "Authorization", "prefix": "Bearer "}},
		"keys":     out,
	}, nil
}

// gatewayModels serves each enabled deployment under its name, plus "auto".
func (r *renderer) gatewayModels(gw config.Node) ([]map[string]any, error) {
	var out []map[string]any
	base := map[string]string{}
	firstChat := ""
	for i, d := range r.Cluster.Spec.Models {
		if !d.IsEnabled() {
			continue
		}
		nodes := r.Cluster.PlacementNodes(d.Placement)
		n, _ := r.Cluster.Node(nodes[0])
		base[d.Name] = "http://" + hostPort(n.Address, r.Cluster.ModelPort(i)) + "/v1"
		// Embedding models are served by name for /v1/embeddings; "auto"
		// never routes to one.
		out = append(out, backend(d.Name, d.Name, base[d.Name]))
		if m, ok := r.Catalog.Lookup(d.CatalogRef); firstChat == "" && (!ok || !m.IsEmbedding()) {
			firstChat = d.Name
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	fast := r.Cluster.Spec.Routing.Defaults.Fast
	if fast == "" {
		fast = firstChat
	}
	if fast == "" {
		return out, nil // embedding models only: no chat route for "auto"
	}
	if _, ok := base[fast]; !ok {
		return nil, fmt.Errorf("routing.defaults.fast %q is not an enabled model", fast)
	}
	if r.routes().Enabled() {
		// vSR picks the model behind Envoy; the vLLM key still travels as
		// the Authorization header to whichever vLLM is chosen.
		out = append(out, backend(AutoModel, AutoModel, "http://"+hostPort(gw.Address, config.PortRouter)+"/v1"))
		return out, nil
	}
	out = append(out, backend(AutoModel, fast, base[fast]))
	return out, nil
}

func backend(name, served, baseURL string) map[string]any {
	return map[string]any{
		"name":     name,
		"provider": "openAI",
		"params": map[string]any{
			"model":   served,
			"baseUrl": baseURL,
			"apiKey":  "$VLLM_API_KEY",
		},
	}
}

// unionPolicies is the guardrail policies of models, each once, in order.
func unionPolicies(byName map[string][]string, models []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range models {
		for _, p := range byName[m] {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}
