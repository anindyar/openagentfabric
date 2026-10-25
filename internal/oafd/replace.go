package oafd

import (
	"context"
	"fmt"
	"net/http"

	"openagentfabric.dev/oaf/internal/config"
)

// replaceModelRequest is POST /api/v1/models/{name}/replace.
type replaceModelRequest struct {
	CatalogRef string `json:"catalog_ref"`
	// Name is the replacement's served name; empty keeps the deployment's.
	Name string `json:"name,omitempty"`
	// Placement moves it; empty keeps the deployment's node or pair.
	Placement            PlacementView `json:"placement,omitempty"`
	GPUMemoryUtilization float64       `json:"gpu_memory_utilization,omitempty"`
	MaxModelLen          int           `json:"max_model_len,omitempty"`
}

// replaceModel swaps the model behind a deployment in one change. Only the
// end state has to fit, so a box's model can be swapped for a bigger one.
// Keeping the name, routing, app keys and clients keep working, and the old
// model serves until the new weights are downloaded (the agent starts the
// new one then). With a new name the old deployment goes, and routing and
// app keys move to the new one. The download starts with the plan.
func (s *Server) replaceModel(w http.ResponseWriter, r *http.Request) {
	var req replaceModelRequest
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
	name := r.PathValue("name")
	var old *config.ModelDeployment
	for i := range c.Spec.Models {
		if c.Spec.Models[i].Name == name {
			old = &c.Spec.Models[i]
		}
	}
	if old == nil {
		writeErr(w, http.StatusNotFound, "no model deployment "+name)
		return
	}
	m, ok := cat.Lookup(req.CatalogRef)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("the catalog has no model %q", req.CatalogRef))
		return
	}
	// The replacement keeps what belongs to the deployment (name, placement,
	// guardrails, enabled) and drops what belonged to the old model.
	d := *old
	d.CatalogRef, d.Profile, d.Args, d.Env, d.Port = req.CatalogRef, "", nil, nil, 0
	d.GPUMemoryUtilization, d.MaxModelLen = req.GPUMemoryUtilization, req.MaxModelLen
	if req.Placement.Node != "" || req.Placement.Group != "" {
		d.Placement = config.Placement{Node: req.Placement.Node, Group: req.Placement.Group}
	}
	if p, err := config.Profile(d, m, len(c.PlacementNodes(d.Placement))); err == nil {
		if d.GPUMemoryUtilization == 0 {
			d.GPUMemoryUtilization = p.GPUMemoryUtilization
		}
		if d.MaxModelLen == 0 {
			d.MaxModelLen = p.MaxModelLen
		}
	}

	var out []byte
	summary := fmt.Sprintf("Replace %s: %s → %s", name, old.CatalogRef, req.CatalogRef)
	if req.Name == "" || req.Name == name {
		out, _, err = config.SetNamed(data, d, name, "models")
	} else {
		for _, x := range c.Spec.Models {
			if x.Name == req.Name {
				writeErr(w, http.StatusConflict, fmt.Sprintf("a deployment called %q exists", req.Name))
				return
			}
		}
		d.Name = req.Name
		summary += ", served as " + req.Name
		out, err = config.RemoveModel(data, name)
		if err == nil {
			out, _, err = config.AddModel(out, d)
		}
		if err == nil {
			out, err = config.RenameModelRefs(out, name, req.Name)
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, code := s.offer(r, data, out, summary)
	if p.Status == PlanPending && s.cfg.Prefetch != nil {
		go s.cfg.Prefetch(context.WithoutCancel(r.Context()), c, d)
	}
	writeJSON(w, code, s.view(p))
}
