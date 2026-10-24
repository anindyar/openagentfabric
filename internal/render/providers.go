package render

import (
	"fmt"
	"strings"

	"openagentfabric.dev/oaf/internal/config"
)

// External providers and guardrails, rendered into agentgateway v1.6's
// simplified LLM config (llm.models[]): each provider model is one entry
// with the upstream's provider type and params, and guardrail policies
// become the entry's guardrails.request / guardrails.response prompt guards.
// API keys never appear in config.yaml: like the vLLM key, each is a $VAR
// the gateway shell-expands from the compose env file.

// agentgatewayProvider maps OAF provider types to agentgateway's provider
// enum (v1.6.0 llm.models[].provider).
var agentgatewayProvider = map[string]string{
	config.ProviderOpenAI:           "openAI",
	config.ProviderOpenAICompatible: "openAI",
	config.ProviderAnthropic:        "anthropic",
	config.ProviderGemini:           "gemini",
	config.ProviderAzureOpenAI:      "azure",
}

// providerKeyEnv is the env var holding a provider's API key.
func providerKeyEnv(name string) string {
	return "PROVIDER_KEY_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// secretLookup is implemented by secrets.Store: it reads a secret without
// generating a missing one.
type secretLookup interface {
	Lookup(name string) (string, bool, error)
}

// providedSecret reads a secret the user supplies (an external provider's
// API key). Unlike generated secrets it is never invented: a missing one is
// an error that says how to set it. Sources that cannot look up (a dry
// run's preview, tests) fall back to their own Value.
func (r *renderer) providedSecret(name, owner string) (string, error) {
	if l, ok := r.Secrets.(secretLookup); ok {
		v, found, err := l.Lookup(name)
		if err != nil {
			return "", fmt.Errorf("secret %s: %w", name, err)
		}
		if !found {
			return "", fmt.Errorf("%s needs its API key in secret %s, which is not set (it is never generated): set it through the management API or `oaf secrets set %s`", owner, name, name)
		}
		return v, nil
	}
	return r.secret(name)
}

// providerModels renders every external provider's models, and adds each
// API key to the gateway's env.
func (r *renderer) providerModels(env *envFile, containerEnv map[string]string) ([]map[string]any, error) {
	var out []map[string]any
	for _, p := range r.Cluster.Spec.Routing.Providers {
		prov, ok := agentgatewayProvider[p.Type]
		if !ok {
			return nil, fmt.Errorf("provider %s: unsupported type %q", p.Name, p.Type)
		}
		keyRef := ""
		if p.SecretRef != "" {
			key, err := r.providedSecret(p.SecretRef, "provider "+p.Name)
			if err != nil {
				return nil, err
			}
			v := providerKeyEnv(p.Name)
			containerEnv[v] = env.ref(v, key)
			keyRef = "$" + v
		}
		guards, err := r.guardrailsFor(p.Guardrails)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", p.Name, err)
		}
		for _, m := range p.Models {
			params := map[string]any{"model": m.UpstreamModel()}
			if keyRef != "" {
				params["apiKey"] = keyRef
			}
			if p.Endpoint != "" {
				params["baseUrl"] = p.Endpoint
			}
			if p.Azure != nil {
				params["azureResourceName"] = p.Azure.ResourceName
				params["azureResourceType"] = "openAI" // agentgateway requires it; azure-openai is the openAI type
				if p.Azure.APIVersion != "" {
					params["azureApiVersion"] = p.Azure.APIVersion
				}
			}
			entry := map[string]any{"name": m.Name, "provider": prov, "params": params}
			if guards != nil {
				entry["guardrails"] = guards
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

// guardrailsFor merges the named policies into one agentgateway guardrails
// object, or nil when there are none. Each regex guard carries its policy's
// rejection message.
func (r *renderer) guardrailsFor(names []string) (map[string]any, error) {
	if len(names) == 0 {
		return nil, nil
	}
	byName := map[string]config.GuardrailPolicy{}
	for _, g := range r.Cluster.Spec.Routing.Guardrails {
		byName[g.Name] = g
	}
	var req, resp []map[string]any
	for _, n := range names {
		g, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("no guardrail policy %q", n)
		}
		for _, gd := range g.Request {
			req = append(req, renderGuard(gd, g.Message))
		}
		for _, gd := range g.Response {
			resp = append(resp, renderGuard(gd, g.Message))
		}
	}
	out := map[string]any{}
	if len(req) > 0 {
		out["request"] = req
	}
	if len(resp) > 0 {
		out["response"] = resp
	}
	return out, nil
}

func renderGuard(g config.Guard, message string) map[string]any {
	out := map[string]any{}
	switch {
	case g.Regex != nil:
		var rules []map[string]any
		for _, b := range g.Regex.Builtins {
			rules = append(rules, map[string]any{"builtin": b})
		}
		for _, p := range g.Regex.Patterns {
			rules = append(rules, map[string]any{"pattern": p})
		}
		out["regex"] = map[string]any{"action": g.Regex.Action, "rules": rules}
		if g.Regex.Action == "reject" {
			if message == "" {
				message = "Blocked by the appliance's guardrails."
			}
			out["rejection"] = map[string]any{"status": 403, "body": message}
		}
	case g.Webhook != nil:
		out["webhook"] = map[string]any{"target": map[string]any{"host": g.Webhook.Host}}
	}
	return out
}
