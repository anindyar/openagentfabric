package oafd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/keys"
)

func (s *Server) routes() {
	m := s.api
	m.HandleFunc("GET /api/v1/status", s.status)
	m.HandleFunc("GET /api/v1/config", s.getConfig)
	m.HandleFunc("GET /api/v1/catalog", s.catalog)

	m.HandleFunc("GET /api/v1/models", s.listModels)
	m.HandleFunc("POST /api/v1/models", s.addModel)
	m.HandleFunc("DELETE /api/v1/models/{name}", s.removeModel)
	m.HandleFunc("PUT /api/v1/models/{name}/guardrails", s.setModelGuardrails)
	m.HandleFunc("PUT /api/v1/models/{name}", s.updateModel)
	m.HandleFunc("PUT /api/v1/nodes/{name}/headless", s.setNodeHeadless)

	m.HandleFunc("GET /api/v1/providers", s.listProviders)
	m.HandleFunc("POST /api/v1/providers", s.putProvider)
	m.HandleFunc("PUT /api/v1/providers/{name}", s.putProvider)
	m.HandleFunc("DELETE /api/v1/providers/{name}", s.removeProvider)

	m.HandleFunc("GET /api/v1/guardrails", s.listGuardrails)
	m.HandleFunc("POST /api/v1/guardrails", s.putGuardrail)
	m.HandleFunc("PUT /api/v1/guardrails/{name}", s.putGuardrail)
	m.HandleFunc("DELETE /api/v1/guardrails/{name}", s.removeGuardrail)

	m.HandleFunc("GET /api/v1/routing", s.getRouting)
	m.HandleFunc("PUT /api/v1/routing", s.putRouting)

	m.HandleFunc("GET /api/v1/keys", s.listKeys)
	m.HandleFunc("POST /api/v1/keys", s.issueKey)
	m.HandleFunc("POST /api/v1/keys/{name}/rotate", s.rotateKey)

	m.HandleFunc("POST /api/v1/licenses/accept", s.acceptLicenses)
	m.HandleFunc("GET /api/v1/plans/{id}", s.getPlan)
	m.HandleFunc("POST /api/v1/plans/{id}/apply", s.applyPlan)
	m.HandleFunc("GET /api/v1/events", s.events)
}

// --- read ----------------------------------------------------------------------

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Status == nil {
		writeErr(w, http.StatusNotImplemented, "status is not available")
		return
	}
	v, err := s.cfg.Status(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if sv, ok := v.(StatusView); ok {
		s.mu.Lock()
		if p := s.plans[s.applying]; p != nil {
			sv.Applying = &ApplyingView{PlanID: p.ID, Summary: p.Summary, Since: p.applyStarted}
		}
		s.mu.Unlock()
		v = sv
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"cluster_yaml": string(data), "hash": hashOf(data)}
	if s.cfg.GatewayConfig != nil {
		if gw, err := s.cfg.GatewayConfig(r.Context()); err == nil {
			out["gateway_config"] = string(gw)
		} else {
			out["gateway_config_error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) loadCatalog() (*config.Catalog, error) {
	return config.LoadCatalog(s.cfg.CatalogPath)
}

func (s *Server) catalog(w http.ResponseWriter, _ *http.Request) {
	cat, err := s.loadCatalog()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The cluster decides which companion models are offered; without a
	// readable cluster.yaml every model is listed as before.
	pairDep, pairGroup := "", ""
	if data, err := os.ReadFile(s.cfg.ClusterPath); err == nil {
		if c, err := config.ParseCluster(data); err == nil {
			pairDep, pairGroup, _ = c.PairModel(cat, "think")
		}
	}
	out := []CatalogModelView{}
	for _, m := range cat.Models {
		v := CatalogModelView{ID: m.ID, Name: m.DisplayName, MemoryGiB: m.SizeGiB, Profiles: []ProfileView{},
			Classes: append([]string{}, m.Classes...), Offered: true}
		switch {
		case m.CompanionOf != "" && pairDep == "":
			v.Note = "Fast model for a single box; recommended beside a large (" + m.CompanionOf + ") model on a pair"
		case m.CompanionOf != "":
			v.Recommended = true
			v.Note = "Fast model for a single box while " + pairDep + " on " + pairGroup + " takes the heavy work"
		case pairDep != "" && m.HasClass("fast") && m.HasClass("think"):
			v.Note = "Middle ground for a box on its own; beside " + pairDep + " a companion model is faster"
		}
		v.Task = "chat"
		if m.IsEmbedding() {
			v.Task = "embedding"
		}
		if m.Source.HuggingFace != nil {
			v.HFRepo = m.Source.HuggingFace.Repo
		}
		v.License = m.License.SPDX
		if v.License == "" {
			v.License = m.License.Name
		}
		for _, p := range m.Profiles {
			v.Profiles = append(v.Profiles, ProfileView{Name: p.Name, Nodes: p.Nodes, GPUMemoryUtilization: p.GPUMemoryUtilization, MaxModelLen: p.MaxModelLen})
			if v.Quantization == "" {
				v.Quantization = p.Quantization
			}
			if p.MaxModelLen > v.ContextWindow {
				v.ContextWindow = p.MaxModelLen
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// --- models --------------------------------------------------------------------

func deploymentView(c *config.Cluster, cat *config.Catalog, d config.ModelDeployment) DeploymentView {
	v := DeploymentView{Name: d.Name, CatalogRef: d.CatalogRef, Profile: d.Profile, Placement: PlacementView{Node: d.Placement.Node, Group: d.Placement.Group},
		Nodes: c.PlacementNodes(d.Placement), Enabled: d.IsEnabled(), GPUMemoryUtilization: d.GPUMemoryUtilization,
		MaxModelLen: d.MaxModelLen, Guardrails: append([]string{}, d.Guardrails...), State: "configured"}
	if v.Nodes == nil {
		v.Nodes = []string{}
	}
	if !v.Enabled {
		v.State = "disabled"
	}
	if cat != nil {
		if m, ok := cat.Lookup(d.CatalogRef); ok {
			v.Task = "chat"
			if m.IsEmbedding() {
				v.Task = "embedding"
			}
		}
	}
	return v
}

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	_, c, err := s.readCluster()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cat, _ := s.loadCatalog()
	out := []DeploymentView{}
	for _, d := range c.Spec.Models {
		out = append(out, deploymentView(c, cat, d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

type addModelRequest struct {
	Name                 string        `json:"name"`
	CatalogRef           string        `json:"catalog_ref"`
	Placement            PlacementView `json:"placement"`
	Profile              string        `json:"profile,omitempty"`
	GPUMemoryUtilization float64       `json:"gpu_memory_utilization,omitempty"`
	MaxModelLen          int           `json:"max_model_len,omitempty"`
	Guardrails           []string      `json:"guardrails,omitempty"`
}

func (s *Server) addModel(w http.ResponseWriter, r *http.Request) {
	var req addModelRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	data, c, err := s.readCluster()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cat, err := s.loadCatalog()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	m, ok := cat.Lookup(req.CatalogRef)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("the catalog has no model %q", req.CatalogRef))
		return
	}
	for _, d := range c.Spec.Models {
		if d.Name == req.Name {
			writeErr(w, http.StatusConflict, fmt.Sprintf("a deployment called %q exists", req.Name))
			return
		}
	}
	d := config.ModelDeployment{Name: req.Name, CatalogRef: req.CatalogRef, Profile: req.Profile,
		Placement:            config.Placement{Node: req.Placement.Node, Group: req.Placement.Group},
		GPUMemoryUtilization: req.GPUMemoryUtilization, MaxModelLen: req.MaxModelLen}
	if d.Placement.Node == "" && d.Placement.Group == "" {
		for _, n := range c.Spec.Nodes {
			if n.HasRole(config.RoleInference) {
				d.Placement.Node = n.Name
				break
			}
		}
	}
	if p, err := config.Profile(d, m, len(c.PlacementNodes(d.Placement))); err == nil {
		if d.GPUMemoryUtilization == 0 {
			d.GPUMemoryUtilization = p.GPUMemoryUtilization
		}
		if d.MaxModelLen == 0 {
			d.MaxModelLen = p.MaxModelLen
		}
	}
	out, _, err := config.AddModel(data, d)
	if err == nil && len(req.Guardrails) > 0 {
		out, _, err = config.SetModelGuardrails(out, req.Name, req.Guardrails)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, code := s.offer(r, data, out, fmt.Sprintf("Deploy %s (%s)", req.Name, req.CatalogRef))
	writeJSON(w, code, s.view(p))
}

func (s *Server) removeModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := config.RemoveModel(data, name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	p, code := s.offer(r, data, out, "Remove model "+name)
	writeJSON(w, code, s.view(p))
}

func (s *Server) setModelGuardrails(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Guardrails []string `json:"guardrails"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	name := r.PathValue("name")
	data, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, _, err := config.SetModelGuardrails(data, name, req.Guardrails)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	p, code := s.offer(r, data, out, fmt.Sprintf("Guardrails on %s: %s", name, strings.Join(req.Guardrails, ", ")))
	writeJSON(w, code, s.view(p))
}

// updateModel changes a deployment's serving settings: PUT
// /api/v1/models/{name} {max_model_len, gpu_memory_utilization, enabled}.
// Absent fields stay; max_model_len 0 means the catalog profile's value.
func (s *Server) updateModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxModelLen          *int     `json:"max_model_len"`
		GPUMemoryUtilization *float64 `json:"gpu_memory_utilization"`
		Enabled              *bool    `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	name := r.PathValue("name")
	data, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, err := config.ParseCluster(data)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var d *config.ModelDeployment
	for i := range c.Spec.Models {
		if c.Spec.Models[i].Name == name {
			d = &c.Spec.Models[i]
		}
	}
	if d == nil {
		writeErr(w, http.StatusNotFound, "no model deployment "+name)
		return
	}
	var what []string
	if req.MaxModelLen != nil {
		d.MaxModelLen = *req.MaxModelLen
		if *req.MaxModelLen == 0 {
			what = append(what, "context: the profile's")
		} else {
			what = append(what, fmt.Sprintf("context %d", *req.MaxModelLen))
		}
	}
	if req.GPUMemoryUtilization != nil {
		d.GPUMemoryUtilization = *req.GPUMemoryUtilization
		what = append(what, fmt.Sprintf("memory %.2f", *req.GPUMemoryUtilization))
	}
	if req.Enabled != nil {
		d.Enabled = req.Enabled
		what = append(what, fmt.Sprintf("enabled %v", *req.Enabled))
	}
	if len(what) == 0 {
		writeErr(w, http.StatusBadRequest, "nothing to change: send max_model_len, gpu_memory_utilization or enabled")
		return
	}
	out, _, err := config.SetNamed(data, *d, name, "models")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, code := s.offer(r, data, out, fmt.Sprintf("Model %s: %s", name, strings.Join(what, ", ")))
	writeJSON(w, code, s.view(p))
}

// --- providers -------------------------------------------------------------------

func (s *Server) hasSecret(name string) bool {
	if name == "" {
		return false
	}
	_, ok, err := s.cfg.Secrets.Lookup(name)
	return err == nil && ok
}

func (s *Server) listProviders(w http.ResponseWriter, _ *http.Request) {
	_, c, err := s.readCluster()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []ProviderView{}
	for _, p := range c.Spec.Routing.Providers {
		v := providerView(p)
		v.HasKey = s.hasSecret(p.SecretRef)
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out, "no_guardrail_warning": config.NoGuardrailWarning})
}

// putProvider creates (POST) or replaces (PUT /{name}) a provider. A sent
// api_key is stored as the provider's secret (provider-<name> by default)
// before the plan, and never echoed.
func (s *Server) putProvider(w http.ResponseWriter, r *http.Request) {
	var v ProviderView
	if err := readJSON(r, &v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if name := r.PathValue("name"); name != "" {
		if v.Name == "" {
			v.Name = name
		}
		if v.Name != name {
			writeErr(w, http.StatusBadRequest, "a provider cannot be renamed; remove it and add a new one")
			return
		}
	}
	data, c, err := s.readCluster()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	exists := false
	for _, p := range c.Spec.Routing.Providers {
		if p.Name == v.Name {
			exists = true
			if v.SecretRef == "" {
				v.SecretRef = p.SecretRef
			}
		}
	}
	if r.Method == http.MethodPost && exists {
		writeErr(w, http.StatusConflict, fmt.Sprintf("provider %q exists; use PUT to change it", v.Name))
		return
	}
	if r.Method == http.MethodPut && !exists {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no provider %q", v.Name))
		return
	}
	if v.SecretRef == "" && (v.APIKey != "" || v.Type != config.ProviderOpenAICompatible) {
		v.SecretRef = "provider-" + v.Name
	}
	if v.APIKey != "" {
		if err := s.cfg.Secrets.Set(v.SecretRef, strings.TrimSpace(v.APIKey)); err != nil {
			writeErr(w, http.StatusBadRequest, "storing the API key: "+err.Error())
			return
		}
		s.cfg.Log.Info("provider key stored", "provider", v.Name, "secret", v.SecretRef, "app", r.Header.Get("X-OAF-App"))
	}
	out, _, err := config.SetProvider(data, v.toConfig())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	verb := "Add"
	if exists {
		verb = "Change"
	}
	p, code := s.offer(r, data, out, fmt.Sprintf("%s external provider %s (%s)", verb, v.Name, v.Type))
	writeJSON(w, code, s.view(p))
}

func (s *Server) removeProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := config.RemoveProvider(data, name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	p, code := s.offer(r, data, out, "Remove external provider "+name)
	writeJSON(w, code, s.view(p))
}

// --- guardrails --------------------------------------------------------------------

func (s *Server) listGuardrails(w http.ResponseWriter, _ *http.Request) {
	_, c, err := s.readCluster()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []GuardrailView{}
	for _, g := range c.Spec.Routing.Guardrails {
		v := guardrailView(g)
		at := AttachView{}
		for _, p := range c.Spec.Routing.Providers {
			if contains(p.Guardrails, g.Name) {
				at.Providers = append(at.Providers, p.Name)
			}
		}
		for _, d := range c.Spec.Models {
			if contains(d.Guardrails, g.Name) {
				at.Models = append(at.Models, d.Name)
			}
		}
		if len(at.Providers)+len(at.Models) > 0 {
			v.Attach = &at
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"guardrails": out,
		"builtins":   config.GuardrailBuiltins,
		"templates":  []GuardrailView{guardrailView(config.BasicGuardrail())},
	})
}

// putGuardrail creates (POST) or replaces (PUT /{name}) a policy, and
// optionally attaches it to providers and models in the same plan.
// {"template":"basic"} uses the recommended policy.
func (s *Server) putGuardrail(w http.ResponseWriter, r *http.Request) {
	var v GuardrailView
	if err := readJSON(r, &v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	var g config.GuardrailPolicy
	switch v.Template {
	case "":
		g = v.toConfig()
	case "basic":
		g = config.BasicGuardrail()
		if v.Name != "" {
			g.Name = v.Name
		}
	default:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown template %q (have: basic)", v.Template))
		return
	}
	if name := r.PathValue("name"); name != "" {
		if g.Name == "" {
			g.Name = name
		}
		if g.Name != name {
			writeErr(w, http.StatusBadRequest, "a guardrail policy cannot be renamed")
			return
		}
	}
	data, c, err := s.readCluster()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	exists := false
	for _, x := range c.Spec.Routing.Guardrails {
		exists = exists || x.Name == g.Name
	}
	if r.Method == http.MethodPost && exists && v.Template == "" {
		writeErr(w, http.StatusConflict, fmt.Sprintf("guardrail policy %q exists; use PUT to change it", g.Name))
		return
	}
	out, _, err := config.SetGuardrail(data, g)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var attached []string
	if v.Attach != nil {
		for _, name := range v.Attach.Providers {
			pr, ok := findProvider(c, name)
			if !ok {
				writeErr(w, http.StatusNotFound, "no provider "+name)
				return
			}
			if !contains(pr.Guardrails, g.Name) {
				pr.Guardrails = append(pr.Guardrails, g.Name)
				if out, _, err = config.SetProvider(out, pr); err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			attached = append(attached, "provider "+name)
		}
		for _, name := range v.Attach.Models {
			d, ok := findModel(c, name)
			if !ok {
				writeErr(w, http.StatusNotFound, "no model deployment "+name)
				return
			}
			if !contains(d.Guardrails, g.Name) {
				if out, _, err = config.SetModelGuardrails(out, name, append(append([]string{}, d.Guardrails...), g.Name)); err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			attached = append(attached, "model "+name)
		}
		if len(v.Attach.Keys) > 0 {
			writeErr(w, http.StatusBadRequest, "guardrails apply to providers and models; per-key limits are set on the key")
			return
		}
	}
	summary := "Guardrail policy " + g.Name
	if len(attached) > 0 {
		summary += " on " + strings.Join(attached, ", ")
	}
	p, code := s.offer(r, data, out, summary)
	writeJSON(w, code, s.view(p))
}

func (s *Server) removeGuardrail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, c, err := s.readCluster()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := config.RemoveGuardrail(data, name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// Detach it everywhere in the same plan, so the result still validates.
	for _, pr := range c.Spec.Routing.Providers {
		if contains(pr.Guardrails, name) {
			pr.Guardrails = without(pr.Guardrails, name)
			if out, _, err = config.SetProvider(out, pr); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	for _, d := range c.Spec.Models {
		if contains(d.Guardrails, name) {
			if out, _, err = config.SetModelGuardrails(out, d.Name, without(d.Guardrails, name)); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	p, code := s.offer(r, data, out, "Remove guardrail policy "+name)
	writeJSON(w, code, s.view(p))
}

// --- routing ------------------------------------------------------------------------

func (s *Server) getRouting(w http.ResponseWriter, _ *http.Request) {
	_, c, err := s.readCluster()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, routingView(c.Spec.Routing))
}

func (s *Server) putRouting(w http.ResponseWriter, r *http.Request) {
	var v RoutingView
	if err := readJSON(r, &v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	data, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, _, err := config.SetRouting(data, v.toUpdate())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, code := s.offer(r, data, out, "Routing: mode "+v.Mode)
	writeJSON(w, code, s.view(p))
}

// --- keys ----------------------------------------------------------------------------

func fingerprint(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])[:12]
}

func (s *Server) listKeys(w http.ResponseWriter, _ *http.Request) {
	_, c, err := s.readCluster()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []KeyView{}
	for _, k := range c.Spec.Routing.Keys {
		v := KeyView{Name: k.Name, Models: k.Models, RequestsPerMinute: k.RequestsPerMinute}
		if val, ok, err := s.cfg.Secrets.Lookup(k.SecretRef); err == nil && ok {
			v.Fingerprint = fingerprint(val)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// issueKey and rotateKey act at once (the key service applies the gateway
// itself), like `oaf keys issue|rotate`; the key is in this reply only.
func (s *Server) issueKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string   `json:"name"`
		Models []string `json:"models,omitempty"`
	}
	if err := readJSON(r, &req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "send {\"name\": ...}")
		return
	}
	if s.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "the key service is not available")
		return
	}
	creds, err := s.cfg.Keys.Issue(r.Context(), req.Name, req.Models)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.cfg.Log.Info("key issued", "key", req.Name, "app", r.Header.Get("X-OAF-App"))
	writeJSON(w, http.StatusOK, KeyView{Name: creds.Name, Key: creds.Key, Models: req.Models, Fingerprint: fingerprint(creds.Key)})
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "the key service is not available")
		return
	}
	name := r.PathValue("name")
	creds, err := s.cfg.Keys.Rotate(r.Context(), name)
	switch {
	case errors.Is(err, keys.ErrTooSoon):
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.cfg.Log.Info("key rotated", "key", name, "app", r.Header.Get("X-OAF-App"))
	writeJSON(w, http.StatusOK, KeyView{Name: creds.Name, Key: creds.Key, Fingerprint: fingerprint(creds.Key)})
}

// --- helpers ----------------------------------------------------------------------------

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func without(xs []string, s string) []string {
	var out []string
	for _, x := range xs {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func findProvider(c *config.Cluster, name string) (config.Provider, bool) {
	for _, p := range c.Spec.Routing.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return config.Provider{}, false
}

func findModel(c *config.Cluster, name string) (config.ModelDeployment, bool) {
	for _, d := range c.Spec.Models {
		if d.Name == name {
			return d, true
		}
	}
	return config.ModelDeployment{}, false
}

// setNodeHeadless plans switching a node's desktop GUI off (headless) or on.
func (s *Server) setNodeHeadless(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Headless *bool `json:"headless"`
	}
	if err := readJSON(r, &req); err != nil || req.Headless == nil {
		writeErr(w, http.StatusBadRequest, "invalid request: want {\"headless\": true|false}")
		return
	}
	name := r.PathValue("name")
	data, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, _, err := config.SetNodeHeadless(data, name, *req.Headless)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	summary := "Run " + name + " headless (stop its desktop GUI)"
	if !*req.Headless {
		summary = "Bring back the desktop GUI on " + name
	}
	p, code := s.offer(r, data, out, summary)
	writeJSON(w, code, s.view(p))
}
