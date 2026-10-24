package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/join"
)

// Deps is what the topology routes need from oafd.
type Deps struct {
	// Load returns cluster.yaml (bytes and decoded) and the catalog.
	Load func(ctx context.Context) ([]byte, *config.Cluster, *config.Catalog, error)
	// Pending lists the boxes that joined.
	Pending *join.PendingStore
	// Current is the hardware of the nodes in cluster.yaml (optional).
	Current func(ctx context.Context) map[string]join.Hardware
	// SavePlan stores a plan for POST /api/v1/plans/{id}/apply and returns
	// its id (oafd's plan store). Without it, plan_id is empty.
	SavePlan func(ctx context.Context, summary string, before, after []byte) (string, error)
}

// Routes are admin routes (oafd wraps them in its authentication).
func Routes(d Deps) map[string]http.Handler {
	return map[string]http.Handler{
		"POST /api/v1/topology/recommend": http.HandlerFunc(d.recommend),
		"POST /api/v1/topology/plan":      http.HandlerFunc(d.plan),
	}
}

// Cabling hints from the wizard.
const (
	CablingCX7     = "cx7"      // the new box is cabled to this box over ConnectX-7
	CablingLAN     = "lan"      // LAN only
	CablingCX7Pair = "cx7-pair" // the two new boxes are cabled to each other
	CablingUnknown = "unknown"
)

// RecommendRequest is the wizard's question.
type RecommendRequest struct {
	NewNodes int    `json:"new_nodes"`
	Cabling  string `json:"cabling"`
}

// EndView is one end of a cable.
type EndView struct {
	Node string `json:"node"`
	Port *int   `json:"port,omitempty"`
}

// CablingView is one cable to connect.
type CablingView struct {
	From EndView `json:"from"`
	To   EndView `json:"to"`
	Kind string  `json:"kind"` // cx7 | lan
	Done bool    `json:"done,omitempty"`
	Text string  `json:"text,omitempty"`
}

// ModelView is a model the option can serve.
type ModelView struct {
	ID        string `json:"id"`
	Placement string `json:"placement"`
	Note      string `json:"note,omitempty"`
}

// OptionView is one option for the wizard.
type OptionView struct {
	ID             string        `json:"id"` // a | b | c
	Title          string        `json:"title"`
	Summary        string        `json:"summary"`
	Topology       string        `json:"topology,omitempty"`
	NodesNeeded    int           `json:"nodes_needed"`
	Possible       bool          `json:"possible"`
	Reason         string        `json:"reason,omitempty"`
	TotalMemoryGiB float64       `json:"total_memory_gib,omitempty"`
	Models         []ModelView   `json:"models,omitempty"`
	Routing        []string      `json:"routing,omitempty"`
	HA             bool          `json:"ha,omitempty"`
	ComingNext     bool          `json:"coming_next,omitempty"`
	Recommended    bool          `json:"recommended,omitempty"`
	Cabling        []CablingView `json:"cabling,omitempty"`
	NewNodes       []string      `json:"new_nodes,omitempty"`
}

var needed = map[string]int{OptionPair: 1, OptionHA: 1, OptionPairPlusSingle: 2}

// View converts an option for the wizard.
func View(o Option, current []string) OptionView {
	v := OptionView{ID: o.Letter, Title: o.Title, Summary: o.Summary, Topology: o.Topology, NodesNeeded: needed[o.ID],
		Possible: o.Possible && o.Plannable, Reason: strings.Join(o.Notes, "; "), TotalMemoryGiB: o.TotalMemoryGiB,
		Routing: o.Routing, Recommended: o.Recommended, NewNodes: o.NewNodes}
	if o.ID == OptionHA {
		v.HA, v.ComingNext, v.Possible = true, true, false
	}
	for _, m := range o.Models {
		v.Models = append(v.Models, ModelView{ID: m.CatalogRef, Placement: m.Where,
			Note: fmt.Sprintf("%s, %s profile, %.0f GiB of weights", m.DisplayName, m.Profile, m.SizeGiB)})
	}
	for _, c := range o.Cabling {
		v.Cabling = append(v.Cabling, CablingView{From: EndView{c.A.Node, c.A.Port}, To: EndView{c.B.Node, c.B.Port}, Kind: "cx7", Done: c.Done, Text: c.Text})
	}
	// The LAN legs: every new box reaches this one over the LAN.
	if len(current) > 0 && (o.ID == OptionPairPlusSingle || o.ID == OptionHA) {
		for _, n := range o.NewNodes {
			v.Cabling = append(v.Cabling, CablingView{From: EndView{Node: current[0]}, To: EndView{Node: n}, Kind: "lan",
				Text: fmt.Sprintf("%s and %s on the same LAN", current[0], n)})
		}
	}
	return v
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (d Deps) input(ctx context.Context) ([]byte, Input, error) {
	data, c, cat, err := d.Load(ctx)
	if err != nil {
		return nil, Input{}, err
	}
	in := Input{Cluster: c, Catalog: cat}
	if d.Pending != nil {
		if in.Pending, err = d.Pending.List(); err != nil {
			return nil, Input{}, err
		}
	}
	if d.Current != nil {
		in.Current = d.Current(ctx)
	}
	hw := map[string]join.Hardware{}
	for n, h := range in.Current {
		hw[n] = h
	}
	for _, p := range in.Pending {
		hw[p.Name] = p.Hardware
	}
	in.Cabling = ObservedCables(hw)
	return data, in, nil
}

// Hypothetical fills in placeholder boxes ("new-1", ...) when fewer than n
// have joined, so the wizard can recommend before anything is plugged in.
func Hypothetical(in Input, n int) Input {
	for i := len(in.Pending); i < n; i++ {
		in.Pending = append(in.Pending, join.PendingNode{Name: fmt.Sprintf("new-%d", i+1)})
	}
	if n > 0 && len(in.Pending) > n {
		in.Pending = in.Pending[:n]
	}
	return in
}

func (d Deps) recommend(w http.ResponseWriter, r *http.Request) {
	var req RecommendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.NewNodes < 1 || req.NewNodes > 2 {
		fail(w, http.StatusBadRequest, "new_nodes must be 1 or 2: OAF grows in pairs of two (no switch)")
		return
	}
	_, in, err := d.input(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	in = Hypothetical(in, req.NewNodes)
	opts := Recommend(in)
	// The cabling the admin described decides what we recommend.
	want := ""
	switch req.Cabling {
	case CablingCX7:
		want = OptionPair
	case CablingLAN:
		want = OptionHA
	case CablingCX7Pair:
		want = OptionPairPlusSingle
	}
	if want != "" {
		for i := range opts {
			opts[i].Recommended = opts[i].ID == want && opts[i].Possible
		}
	}
	cur := nodeNames(in.Cluster)
	out := make([]OptionView, 0, len(opts))
	for _, o := range opts {
		out = append(out, View(o, cur))
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": out})
}

// PlanRequest is the admin's choice: an option and the joined boxes for it.
type PlanRequest struct {
	Option  string   `json:"option"` // a | c (or pair | pair-plus-single)
	Nodes   []string `json:"nodes"`
	Model   string   `json:"model,omitempty"`
	Name    string   `json:"name,omitempty"`
	NoModel bool     `json:"no_model,omitempty"`
	// Headless runs the new boxes without their desktop GUI (frees memory).
	Headless bool `json:"headless,omitempty"`
}

// PlanResponse is the plan for review. errors means it cannot be applied.
type PlanResponse struct {
	PlanID   string   `json:"plan_id"`
	Summary  string   `json:"summary,omitempty"`
	Diff     string   `json:"diff"`
	Warnings []string `json:"warnings,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

var letters = map[string]string{"a": OptionPair, "b": OptionHA, "c": OptionPairPlusSingle}

func (d Deps) plan(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "invalid request")
		return
	}
	opt := req.Option
	if id, ok := letters[opt]; ok {
		opt = id
	}
	data, in, err := d.input(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(req.Nodes) > 0 {
		byName := map[string]join.PendingNode{}
		for _, p := range in.Pending {
			byName[p.Name] = p
		}
		var chosen []join.PendingNode
		for _, n := range req.Nodes {
			p, ok := byName[n]
			if !ok {
				fail(w, http.StatusBadRequest, fmt.Sprintf("%s has not joined (run the join command on it first)", n))
				return
			}
			chosen = append(chosen, p)
		}
		in.Pending = chosen
	}
	out, rep, err := Plan(data, in, Choice{Option: opt, Model: req.Model, Name: req.Name, NoModel: req.NoModel, Headless: req.Headless})
	resp := PlanResponse{}
	for _, i := range rep.Warnings() {
		resp.Warnings = append(resp.Warnings, i.String())
	}
	for _, i := range rep.Errors() {
		resp.Errors = append(resp.Errors, i.String())
	}
	if out == nil {
		resp.Errors = append(resp.Errors, err.Error())
		writeJSON(w, http.StatusUnprocessableEntity, resp)
		return
	}
	resp.Diff = UnifiedDiff("cluster.yaml", data, out)
	resp.Summary = summary(in, opt, req)
	if err == nil && d.SavePlan != nil {
		if resp.PlanID, err = d.SavePlan(r.Context(), resp.Summary, data, out); err != nil {
			resp.Errors = append(resp.Errors, "saving the plan: "+err.Error())
		}
	}
	code := http.StatusOK
	if len(resp.Errors) > 0 {
		code = http.StatusUnprocessableEntity
	}
	writeJSON(w, code, resp)
}

func summary(in Input, opt string, req PlanRequest) string {
	var names []string
	for _, p := range in.Pending {
		names = append(names, p.Name)
	}
	switch opt {
	case OptionPair:
		return fmt.Sprintf("Make %s and %s a pair over ConnectX-7", nodeNames(in.Cluster)[0], strings.Join(names[:min(1, len(names))], ""))
	case OptionPairPlusSingle:
		return fmt.Sprintf("Add %s as a pair over ConnectX-7 next to %s", strings.Join(names[:min(2, len(names))], " and "), nodeNames(in.Cluster)[0])
	}
	return "Grow the cluster"
}
