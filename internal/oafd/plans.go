package oafd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"openagentfabric.dev/oaf/internal/config"
)

// Plan states.
const (
	PlanPending  = "planned"
	PlanApplying = "applying"
	PlanApplied  = "applied"
	PlanFailed   = "failed"
	// Internal states, shown as planned (with errors) and failed (expired).
	PlanInvalid = "invalid"
	PlanExpired = "expired"
)

// Issue is a validation finding as JSON.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Plan is a proposed cluster.yaml change: what it does, its diff and its
// validation findings. Applying it writes the new document and reconciles.
type Plan struct {
	ID        string    `json:"plan_id"`
	Summary   string    `json:"summary"`
	App       string    `json:"app,omitempty"`
	Status    string    `json:"status"`
	Diff      string    `json:"diff"`
	Warnings  []Issue   `json:"warnings"`
	Errors    []Issue   `json:"errors"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Steps     []Step    `json:"steps,omitempty"`
	// NeedsAcceptance: licences a person must read and accept before this
	// change can apply (POST /api/v1/licenses/accept). The plan is invalid
	// until then.
	NeedsAcceptance []config.NeededAcceptance `json:"needs_acceptance,omitempty"`
	Error           string                    `json:"error,omitempty"`
	Applied         time.Time                 `json:"applied_at,omitzero"`

	Log []string `json:"-"`

	baseHash string
	doc      []byte
	// rebase marks a record-only plan (licence acceptances): it changes
	// nothing that runs, so it is rebuilt on the current cluster.yaml when
	// that changed, and applies while another plan is being applied.
	rebase func(cur []byte) ([]byte, error)
}

// Step is one line of an apply's progress.
type Step struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // running | done | failed
	Message string `json:"message,omitempty"`
}

// Event is a progress line for GET /api/v1/events.
type Event struct {
	Type    string `json:"type"`
	Plan    string `json:"plan_id,omitempty"`
	Status  string `json:"status,omitempty"`
	Message string `json:"message"`
	At      string `json:"at"`
}

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func issues(in []config.Issue) []Issue {
	out := []Issue{}
	for _, i := range in {
		out = append(out, Issue{Path: i.Path, Message: i.Message})
	}
	return out
}

// offer validates doc (the full new cluster.yaml) against the catalog and the
// secrets it needs, and records it as a plan against base.
func (s *Server) offer(r *http.Request, base, doc []byte, summary string) (*Plan, int) {
	now := s.cfg.Now()
	p := &Plan{ID: newID(), Summary: summary, App: r.Header.Get("X-OAF-App"), CreatedAt: now,
		ExpiresAt: now.Add(s.cfg.PlanTTL), Warnings: []Issue{}, Errors: []Issue{}, baseHash: hashOf(base), doc: doc}
	p.Diff = unifiedDiff(string(base), string(doc), "cluster.yaml", "cluster.yaml (planned)")
	c, err := config.ParseCluster(doc)
	if err != nil {
		p.Errors = append(p.Errors, Issue{Path: "", Message: err.Error()})
	} else {
		cat, err := config.LoadCatalog(s.cfg.CatalogPath)
		if err != nil {
			p.Errors = append(p.Errors, Issue{Message: "catalog: " + err.Error()})
		} else {
			rep := config.Validate(c, cat, config.Options{})
			p.Errors = append(p.Errors, issues(rep.Errors())...)
			p.Warnings = append(p.Warnings, issues(rep.Warnings())...)
			// Found now, not at apply: an engine image or model with terms to
			// accept would otherwise fail after the change was approved.
			if s.cfg.Versions != nil {
				p.NeedsAcceptance = config.NeededAcceptances(c, cat, s.cfg.Versions)
				for _, n := range p.NeedsAcceptance {
					p.Errors = append(p.Errors, Issue{Path: "spec.licenseAcceptances",
						Message: fmt.Sprintf("%s needs %s accepted first (%s): accept it in the console or POST /api/v1/licenses/accept", n.For, n.Title, n.URL)})
				}
			}
		}
		// A provider key is never generated: it must exist before apply.
		for i, pr := range c.Spec.Routing.Providers {
			if pr.SecretRef == "" {
				continue
			}
			if _, ok, err := s.cfg.Secrets.Lookup(pr.SecretRef); err != nil || !ok {
				p.Errors = append(p.Errors, Issue{Path: fmt.Sprintf("spec.routing.providers[%d].secretRef", i),
					Message: fmt.Sprintf("provider %q has no API key yet: send api_key with the provider", pr.Name)})
			}
		}
	}
	p.Status = PlanPending
	code := http.StatusCreated
	if len(p.Errors) > 0 {
		p.Status, code = PlanInvalid, http.StatusUnprocessableEntity
	}
	if p.Diff == "" && len(p.Errors) == 0 {
		p.Summary += " (no change)"
	}
	s.mu.Lock()
	s.gcLocked(now)
	s.plans[p.ID] = p
	s.mu.Unlock()
	s.persist()
	return p, code
}

func (s *Server) gcLocked(now time.Time) {
	for id, p := range s.plans {
		if (p.Status == PlanPending || p.Status == PlanInvalid) && now.After(p.ExpiresAt) {
			p.Status = PlanExpired
			p.doc = nil
		}
		if now.Sub(p.CreatedAt) > 24*time.Hour && p.Status != PlanApplying {
			delete(s.plans, id)
		}
	}
}

func (s *Server) view(p *Plan) Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := *p
	v.Log = nil
	switch p.Status {
	case PlanInvalid:
		v.Status = PlanPending
	case PlanExpired:
		v.Status = PlanFailed
		if v.Error == "" {
			v.Error = "the plan expired or cluster.yaml changed since it was made; make a new plan"
		}
	}
	for i, line := range p.Log {
		st := "done"
		if i == len(p.Log)-1 {
			switch p.Status {
			case PlanApplying:
				st = "running"
			case PlanFailed:
				st = "failed"
			}
		}
		v.Steps = append(v.Steps, Step{Name: line, Status: st})
	}
	return v
}

func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	s.gcLocked(s.cfg.Now())
	p := s.plans[id]
	s.mu.Unlock()
	if p == nil {
		if s.knownPlan(id) {
			writeJSON(w, http.StatusOK, Plan{ID: id, Status: PlanFailed, Error: "the plan expired (oafd restarted)", Warnings: []Issue{}, Errors: []Issue{}})
			return
		}
		writeErr(w, http.StatusNotFound, "no such plan")
		return
	}
	writeJSON(w, http.StatusOK, s.view(p))
}

// applyPlan writes the plan's cluster.yaml and reconciles in the background.
func (s *Server) applyPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	s.gcLocked(s.cfg.Now())
	p := s.plans[id]
	switch {
	case p == nil:
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such plan (plans expire after an hour)")
		return
	case p.Status == PlanInvalid:
		s.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the plan has errors and cannot be applied", "errors": p.Errors})
		return
	case p.Status != PlanPending:
		st := p.Status
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "the plan is "+st)
		return
	case s.applying != "" && p.rebase == nil:
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "another plan is being applied: "+s.applying)
		return
	}
	cur, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		s.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hashOf(cur) != p.baseHash && p.rebase != nil {
		doc, err := p.rebase(cur)
		if err != nil {
			s.mu.Unlock()
			writeErr(w, http.StatusConflict, "cluster.yaml changed and the plan could not be rebuilt on it: "+err.Error())
			return
		}
		p.doc, p.baseHash = doc, hashOf(cur)
	}
	if hashOf(cur) != p.baseHash {
		p.Status = PlanExpired
		p.doc = nil
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "cluster.yaml changed since this plan was made; make a new plan")
		return
	}
	// The change must render (images, licences, secrets, fabric) before it
	// is written: a failed apply used to leave cluster.yaml holding it.
	if s.cfg.Check != nil {
		if err := s.cfg.Check(r.Context(), p.doc); err != nil {
			p.Status, p.Error = PlanFailed, "not applied, cluster.yaml unchanged: "+err.Error()
			s.mu.Unlock()
			s.persist()
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": p.Error, "plan_id": p.ID})
			return
		}
	}
	// A record-only plan reconciles only when something already deployed was
	// waiting on it, and never while another apply runs (that one, or the
	// next, deploys it).
	waiting := p.rebase == nil || s.waitingOnLicences(cur)
	if err := config.WriteFileAtomic(s.cfg.ClusterPath, p.doc); err != nil {
		s.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "writing cluster.yaml: "+err.Error())
		return
	}
	if p.rebase != nil && (!waiting || s.applying != "") {
		p.Status, p.Applied, p.doc = PlanApplied, s.cfg.Now(), nil
		if waiting {
			p.Log = append(p.Log, "recorded; what waited on these licences deploys when "+s.applying+" finishes or with the next apply")
		}
		s.mu.Unlock()
		s.persist()
		s.publish(Event{Plan: p.ID, Status: PlanApplied, Message: "applied: " + p.Summary})
		s.cfg.Log.Info("plan recorded", "plan", p.ID, "summary", p.Summary, "app", r.Header.Get("X-OAF-App"))
		writeJSON(w, http.StatusOK, s.view(p))
		return
	}
	p.Status = PlanApplying
	s.applying = p.ID
	s.mu.Unlock()
	s.publish(Event{Plan: p.ID, Status: PlanApplying, Message: "applying: " + p.Summary})
	s.cfg.Log.Info("applying plan", "plan", p.ID, "summary", p.Summary, "app", r.Header.Get("X-OAF-App"))

	go s.runApply(p)
	writeJSON(w, http.StatusAccepted, s.view(p))
}

func (s *Server) runApply(p *Plan) {
	lw := &lineWriter{fn: func(line string) {
		s.mu.Lock()
		p.Log = append(p.Log, line)
		if len(p.Log) > 2000 {
			p.Log = p.Log[len(p.Log)-2000:]
		}
		s.mu.Unlock()
		s.publish(Event{Type: lineType(line), Plan: p.ID, Status: PlanApplying, Message: line})
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	err := s.cfg.Apply(ctx, lw)
	lw.flush()
	s.mu.Lock()
	s.applying = ""
	p.Applied = s.cfg.Now()
	if err != nil {
		p.Status = PlanFailed
		p.Error = err.Error() + " (cluster.yaml holds the change; fix it with a new plan or re-apply)"
	} else {
		p.Status = PlanApplied
	}
	p.doc = nil
	st := p.Status
	s.mu.Unlock()
	s.persist()
	msg := "applied: " + p.Summary
	if st == PlanFailed {
		msg = p.Error
	}
	s.publish(Event{Plan: p.ID, Status: st, Message: msg})
	s.cfg.Log.Info("plan finished", "plan", p.ID, "status", st)
}

// waitingOnLicences reports whether the cluster in cur deploys something whose
// licence nobody accepted yet (accepting it must then reconcile).
func (s *Server) waitingOnLicences(cur []byte) bool {
	c, err := config.ParseCluster(cur)
	if err != nil || s.cfg.Versions == nil {
		return true
	}
	cat, err := config.LoadCatalog(s.cfg.CatalogPath)
	if err != nil {
		return true
	}
	return len(config.NeededAcceptances(c, cat, s.cfg.Versions)) > 0
}

// lineWriter splits writes into lines.
type lineWriter struct {
	fn  func(string)
	buf bytes.Buffer
}

func (l *lineWriter) Write(b []byte) (int, error) {
	l.buf.Write(b)
	for {
		line, err := l.buf.ReadString('\n')
		if err != nil {
			l.buf.WriteString(line)
			break
		}
		if t := strings.TrimRight(line, "\r\n"); t != "" {
			l.fn(t)
		}
	}
	return len(b), nil
}

func (l *lineWriter) flush() {
	if t := strings.TrimSpace(l.buf.String()); t != "" {
		l.fn(t)
	}
	l.buf.Reset()
}

// --- events ------------------------------------------------------------------

func (s *Server) publish(e Event) {
	if e.Type == "" {
		e.Type = "plan"
	}
	e.At = s.cfg.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- e:
		default: // a slow reader misses lines rather than blocking applies
		}
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch := make(chan Event, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": oafd events\n\n")
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// --- persistence ---------------------------------------------------------------

type planRecord struct {
	ID     string    `json:"id"`
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

func (s *Server) planFile() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.StateDir, "oafd-plans.json")
}

// persist records plan ids and states (never documents), so that after a
// restart a known plan reads as expired rather than unknown.
func (s *Server) persist() {
	f := s.planFile()
	if f == "" {
		return
	}
	s.mu.Lock()
	recs := []planRecord{}
	for _, p := range s.plans {
		recs = append(recs, planRecord{ID: p.ID, Status: p.Status, At: p.CreatedAt})
	}
	s.mu.Unlock()
	b, _ := json.Marshal(recs)
	_ = os.MkdirAll(filepath.Dir(f), 0o700)
	_ = os.WriteFile(f, b, 0o600)
}

func (s *Server) knownPlan(id string) bool {
	f := s.planFile()
	if f == "" {
		return false
	}
	b, err := os.ReadFile(f)
	if errors.Is(err, os.ErrNotExist) || err != nil {
		return false
	}
	var recs []planRecord
	if json.Unmarshal(b, &recs) != nil {
		return false
	}
	for _, r := range recs {
		if r.ID == id {
			return true
		}
	}
	return false
}

// lineType classifies an apply output line for /events: fabric bring-up
// ("<verb> <node>/fabric ...") and link checks ("link  ...") get their own
// types, so the console's wizard can follow them.
func lineType(line string) string {
	switch {
	case strings.HasPrefix(line, "link "):
		return "link"
	case strings.Contains(line, "/fabric "), strings.HasPrefix(line, "warning: link "):
		return "fabric"
	}
	return "plan"
}
