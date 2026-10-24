package oafd

import (
	"strings"
	"time"

	"openagentfabric.dev/oaf/internal/config"
)

// JSON shapes of the API (snake_case), converted to and from the
// cluster.yaml types so the document keeps its own (camelCase) spelling.

// StatusView is GET /api/v1/status.
type StatusView struct {
	Cluster  string      `json:"cluster"`
	Topology string      `json:"topology"`
	Nodes    []NodeView  `json:"nodes"`
	Groups   []GroupView `json:"groups"`
	// LicensesNeeded: what cluster.yaml deploys but a person has not accepted
	// yet (e.g. after a change was written without its licence).
	LicensesNeeded []config.NeededAcceptance `json:"licenses_needed"`
	Deployments    []DeploymentView          `json:"deployments"`
	Components     []ComponentView           `json:"components"`
	// Applying is the plan being applied now, if any.
	Applying *ApplyingView `json:"applying,omitempty"`
}

// ApplyingView is the plan being applied.
type ApplyingView struct {
	PlanID  string    `json:"plan_id"`
	Summary string    `json:"summary"`
	Since   time.Time `json:"since"`
}

// GroupView is a placement group: the boxes a multi-node model runs across
// (a ConnectX-7 pair), from spec.groups.
type GroupView struct {
	Name  string   `json:"name"`
	Nodes []string `json:"nodes"`
}

// NodeView is one node. State: joined | pending | joining | offline.
type NodeView struct {
	Name      string          `json:"name"`
	Address   string          `json:"address"`
	Roles     []string        `json:"roles"`
	State     string          `json:"state"`
	Health    string          `json:"health,omitempty"`
	Hardware  *HardwareView   `json:"hardware,omitempty"`
	Ports     []PortView      `json:"ports,omitempty"`
	Preflight []PreflightView `json:"preflight,omitempty"`
	Version   string          `json:"version,omitempty"`
	JoinedAt  string          `json:"joined_at,omitempty"`
}

// HardwareView is what a node reports about itself.
type HardwareView struct {
	GPU           string  `json:"gpu,omitempty"`
	MemoryGiB     float64 `json:"memory_gib,omitempty"`
	MemoryFreeGiB float64 `json:"memory_free_gib,omitempty"`
	DiskFreeGiB   float64 `json:"disk_free_gib,omitempty"`
	Serial        string  `json:"serial,omitempty"`
	OS            string  `json:"os,omitempty"`
	Arch          string  `json:"arch,omitempty"`
}

// PortView is one QSFP port; Link is up | down.
type PortView struct {
	Port      int        `json:"port"`
	Link      string     `json:"link"`
	Peer      string     `json:"peer,omitempty"`
	SpeedGbps float64    `json:"speed_gbps,omitempty"`
	Rails     []RailView `json:"rails,omitempty"`
}

// RailView is one half of a port (one PCIe domain).
type RailView struct {
	Netdev  string  `json:"netdev"`
	IP      string  `json:"ip,omitempty"`
	BwGbps  float64 `json:"bw_gbps,omitempty"`
	Status  string  `json:"status,omitempty"`
	RDMADev string  `json:"rdma_dev,omitempty"`
}

// PreflightView is one preflight check: pass | warn | fail.
type PreflightView struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

// DeploymentView is a model deployment and how it runs.
type DeploymentView struct {
	Name                 string        `json:"name"`
	CatalogRef           string        `json:"catalog_ref"`
	Task                 string        `json:"task,omitempty"`
	Placement            PlacementView `json:"placement"`
	Profile              string        `json:"profile,omitempty"`
	Nodes                []string      `json:"nodes"`
	State                string        `json:"state"`
	Health               string        `json:"health,omitempty"`
	Enabled              bool          `json:"enabled"`
	GPUMemoryUtilization float64       `json:"gpu_memory_utilization,omitempty"`
	MaxModelLen          int           `json:"max_model_len,omitempty"`
	Guardrails           []string      `json:"guardrails"`
	// Phase is what the console shows (status only): queued | starting |
	// loading | ready | failed | removing | disabled. Progress (0-100) and
	// Detail follow a load; Error is a failure's cause; Since is when the
	// model's container last started; Restarts counts its restarts.
	Phase    string `json:"phase,omitempty"`
	Progress int    `json:"progress,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Error    string `json:"error,omitempty"`
	Since    string `json:"since,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
}

// PlacementView is a node or a group.
type PlacementView struct {
	Node  string `json:"node,omitempty"`
	Group string `json:"group,omitempty"`
}

// ComponentView is one stack container (project/service) on a node.
type ComponentView struct {
	Name   string `json:"name"`
	Node   string `json:"node"`
	State  string `json:"state"`
	Health string `json:"health,omitempty"`
}

// CatalogModelView is GET /api/v1/catalog models[].
type CatalogModelView struct {
	ID            string        `json:"id"`
	Name          string        `json:"name,omitempty"`
	Task          string        `json:"task,omitempty"`
	ParamsB       float64       `json:"params_b,omitempty"`
	ActiveParamsB float64       `json:"active_params_b,omitempty"`
	Quantization  string        `json:"quantization,omitempty"`
	MemoryGiB     float64       `json:"memory_gib,omitempty"`
	ContextWindow int           `json:"context_window,omitempty"`
	Profiles      []ProfileView `json:"profiles"`
	License       string        `json:"license,omitempty"`
	HFRepo        string        `json:"hf_repo,omitempty"`
	Classes       []string      `json:"classes,omitempty"`
	// Offered: the add-model form lists it (every catalog model is offered
	// today). Recommended marks companion models while a pair runs the model
	// they accompany. Note says why, for the add-model form.
	Offered     bool   `json:"offered"`
	Recommended bool   `json:"recommended,omitempty"`
	Note        string `json:"note,omitempty"`
}

// ProfileView is a serving profile.
type ProfileView struct {
	Name                 string  `json:"name"`
	Nodes                int     `json:"nodes"`
	GPUMemoryUtilization float64 `json:"gpu_memory_utilization"`
	MaxModelLen          int     `json:"max_model_len,omitempty"`
}

// ProviderView is an external provider (GET, POST, PUT /providers). APIKey
// is accepted on write only and stored as an OAF secret.
type ProviderView struct {
	Name        string              `json:"name"`
	Type        string              `json:"type"`
	Endpoint    string              `json:"endpoint,omitempty"`
	Azure       *AzureView          `json:"azure,omitempty"`
	Models      []ProviderModelView `json:"models"`
	SecretRef   string              `json:"secret_ref,omitempty"`
	Guardrails  []string            `json:"guardrails"`
	AllowEgress bool                `json:"allow_egress"`
	HasKey      bool                `json:"has_key"`
	APIKey      string              `json:"api_key,omitempty"`
}

// AzureView names an Azure OpenAI resource.
type AzureView struct {
	ResourceName string `json:"resource_name"`
	APIVersion   string `json:"api_version,omitempty"`
}

// ProviderModelView is a provider model.
type ProviderModelView struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream,omitempty"`
}

// GuardrailView is a guardrail policy as a flat list of rules.
type GuardrailView struct {
	Name              string      `json:"name"`
	Description       string      `json:"description,omitempty"`
	Message           string      `json:"message,omitempty"`
	Rules             []RuleView  `json:"rules"`
	RequestsPerMinute int         `json:"requests_per_minute,omitempty"`
	Attach            *AttachView `json:"attach,omitempty"`
	Template          string      `json:"template,omitempty"`
}

// RuleView is one rule: a built-in detector, a regex, or a webhook.
// On is request | response; Action is reject | mask | audit.
type RuleView struct {
	Builtin string `json:"builtin,omitempty"`
	Regex   string `json:"regex,omitempty"`
	Webhook string `json:"webhook,omitempty"`
	Name    string `json:"name,omitempty"`
	Action  string `json:"action,omitempty"`
	On      string `json:"on"`
}

// AttachView lists what a guardrail policy applies to.
type AttachView struct {
	Providers []string `json:"providers,omitempty"`
	Models    []string `json:"models,omitempty"`
	Keys      []string `json:"keys,omitempty"`
}

// RoutingView is GET/PUT /routing.
type RoutingView struct {
	Mode     string        `json:"mode"`
	Defaults DefaultsView  `json:"defaults"`
	Rules    []RuleRouting `json:"rules"`
}

// DefaultsView are the fast and fallback targets.
type DefaultsView struct {
	Fast     string `json:"fast,omitempty"`
	Fallback string `json:"fallback,omitempty"`
}

// RuleRouting is one routing rule.
type RuleRouting struct {
	Name     string    `json:"name"`
	Target   string    `json:"target"`
	Match    MatchView `json:"match"`
	Requires string    `json:"requires_entitlement,omitempty"`
}

// MatchView fires when any signal fires.
type MatchView struct {
	Signals []SignalView `json:"signals"`
}

// SignalView is a vSR signal.
type SignalView struct {
	Type          string   `json:"type"`
	Threshold     *float64 `json:"threshold,omitempty"`
	Keywords      []string `json:"keywords,omitempty"`
	Domains       []string `json:"domains,omitempty"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
	// MinTokens: a context signal fires for prompts at least this long.
	MinTokens int `json:"min_tokens,omitempty"`
}

// KeyView is an app key (never its value, except once on issue/rotate).
type KeyView struct {
	Name              string   `json:"name"`
	Models            []string `json:"models,omitempty"`
	RequestsPerMinute int      `json:"requests_per_minute,omitempty"`
	Fingerprint       string   `json:"fingerprint,omitempty"`
	Key               string   `json:"key,omitempty"`
}

// --- conversions ---------------------------------------------------------------

func providerView(p config.Provider) ProviderView {
	v := ProviderView{Name: p.Name, Type: p.Type, Endpoint: p.Endpoint, SecretRef: p.SecretRef,
		Guardrails: append([]string{}, p.Guardrails...), AllowEgress: p.AllowEgress, Models: []ProviderModelView{}}
	if p.Azure != nil {
		v.Azure = &AzureView{ResourceName: p.Azure.ResourceName, APIVersion: p.Azure.APIVersion}
	}
	for _, m := range p.Models {
		v.Models = append(v.Models, ProviderModelView{Name: m.Name, Upstream: m.UpstreamModel()})
	}
	return v
}

func (v ProviderView) toConfig() config.Provider {
	p := config.Provider{Name: v.Name, Type: v.Type, Endpoint: v.Endpoint, SecretRef: v.SecretRef,
		AllowEgress: v.AllowEgress, Guardrails: v.Guardrails}
	if v.Azure != nil {
		p.Azure = &config.AzureSettings{ResourceName: v.Azure.ResourceName, APIVersion: v.Azure.APIVersion}
	}
	for _, m := range v.Models {
		up := m.Upstream
		if up == m.Name {
			up = ""
		}
		p.Models = append(p.Models, config.ProviderModel{Name: m.Name, Upstream: up})
	}
	return p
}

// builtinAliases accept the spellings a UI may use for the pinned detectors.
var builtinAliases = map[string]string{
	"phone": "phoneNumber", "phone_number": "phoneNumber", "credit_card": "creditCard",
	"creditcard": "creditCard", "ca_sin": "caSin", "casin": "caSin",
}

func normalizeBuiltin(b string) string {
	if a, ok := builtinAliases[strings.ToLower(b)]; ok {
		return a
	}
	return b
}

func guardrailView(g config.GuardrailPolicy) GuardrailView {
	v := GuardrailView{Name: g.Name, Description: g.Description, Message: g.Message, Rules: []RuleView{}}
	add := func(on string, guards []config.Guard) {
		for _, gd := range guards {
			switch {
			case gd.Regex != nil:
				for _, b := range gd.Regex.Builtins {
					v.Rules = append(v.Rules, RuleView{Builtin: b, Action: gd.Regex.Action, On: on})
				}
				for _, p := range gd.Regex.Patterns {
					v.Rules = append(v.Rules, RuleView{Regex: p, Action: gd.Regex.Action, On: on})
				}
			case gd.Webhook != nil:
				v.Rules = append(v.Rules, RuleView{Webhook: gd.Webhook.Host, On: on})
			}
		}
	}
	add("request", g.Request)
	add("response", g.Response)
	return v
}

// toConfig groups rules by side and action into regex guards, in first-seen
// order; webhooks stay separate guards.
func (v GuardrailView) toConfig() config.GuardrailPolicy {
	g := config.GuardrailPolicy{Name: v.Name, Description: v.Description, Message: v.Message}
	type key struct{ on, action string }
	idx := map[key]int{}
	for _, r := range v.Rules {
		on := r.On
		if on == "" {
			on = "request"
		}
		guards := &g.Request
		if on == "response" {
			guards = &g.Response
		}
		if r.Webhook != "" {
			*guards = append(*guards, config.Guard{Webhook: &config.WebhookGuard{Host: r.Webhook}})
			continue
		}
		action := r.Action
		if action == "" {
			action = "reject"
		}
		k := key{on, action}
		i, ok := idx[k]
		if !ok {
			*guards = append(*guards, config.Guard{Regex: &config.RegexGuard{Action: action}})
			i = len(*guards) - 1
			idx[k] = i
		}
		rg := (*guards)[i].Regex
		if r.Builtin != "" {
			rg.Builtins = append(rg.Builtins, normalizeBuiltin(r.Builtin))
		}
		if r.Regex != "" {
			rg.Patterns = append(rg.Patterns, r.Regex)
		}
	}
	return g
}

func routingView(r config.Routing) RoutingView {
	mode := r.Mode
	if mode == "" {
		mode = "observe"
	}
	v := RoutingView{Mode: mode, Defaults: DefaultsView{Fast: r.Defaults.Fast, Fallback: r.Defaults.Fallback}, Rules: []RuleRouting{}}
	for _, rule := range r.Rules {
		rr := RuleRouting{Name: rule.Name, Target: rule.Target, Match: MatchView{Signals: []SignalView{}}}
		if rule.Requires != nil {
			rr.Requires = rule.Requires.Entitlement
		}
		for _, s := range rule.Match.Signals {
			rr.Match.Signals = append(rr.Match.Signals, SignalView{Type: s.Type, Threshold: s.Threshold, Keywords: s.Keywords,
				Domains: s.Domains, CaseSensitive: s.CaseSensitive, MinTokens: s.MinTokens})
		}
		v.Rules = append(v.Rules, rr)
	}
	return v
}

func (v RoutingView) toUpdate() config.RoutingUpdate {
	mode := v.Mode
	defaults := config.RoutingDefaults{Fast: v.Defaults.Fast, Fallback: v.Defaults.Fallback}
	rules := []config.RoutingRule{}
	for _, r := range v.Rules {
		rule := config.RoutingRule{Name: r.Name, Target: r.Target}
		if r.Requires != "" {
			rule.Requires = &config.RuleRequires{Entitlement: r.Requires}
		}
		for _, s := range r.Match.Signals {
			rule.Match.Signals = append(rule.Match.Signals, config.Signal{Type: s.Type, Threshold: s.Threshold,
				Keywords: s.Keywords, Domains: s.Domains, CaseSensitive: s.CaseSensitive, MinTokens: s.MinTokens})
		}
		rules = append(rules, rule)
	}
	return config.RoutingUpdate{Mode: &mode, Defaults: &defaults, Rules: &rules}
}
