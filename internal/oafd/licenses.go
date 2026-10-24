package oafd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"openagentfabric.dev/oaf/internal/config"
)

// acceptLicenses records that a person read and accepted licences (engine
// images with NVIDIA terms, gated models): POST /api/v1/licenses/accept
// {subjects, accepted_by}. It returns a record-only plan adding them to
// spec.licenseAcceptances: it applies even while another plan is applied,
// and reconciles only to deploy what was already waiting on them.
func (s *Server) acceptLicenses(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subjects   []string `json:"subjects"`
		AcceptedBy string   `json:"accepted_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Subjects) == 0 {
		writeErr(w, http.StatusBadRequest, "subjects are required")
		return
	}
	by := strings.TrimSpace(req.AcceptedBy)
	if by == "" {
		writeErr(w, http.StatusBadRequest, "accepted_by (who accepted) is required")
		return
	}
	cat, err := config.LoadCatalog(s.cfg.CatalogPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "catalog: "+err.Error())
		return
	}
	urls := map[string]string{}
	for _, sub := range req.Subjects {
		url, ok := config.AcceptableURL(sub, cat, s.cfg.Versions)
		if !ok {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q is not a licence this bundle or catalog asks to accept", sub))
			return
		}
		urls[sub] = url
	}
	base, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := s.cfg.Now().UTC().Format(time.RFC3339)
	accept := func(cur []byte) ([]byte, []string, error) {
		return withAcceptances(cur, req.Subjects, urls, by, now)
	}
	doc, added, err := accept(base)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	summary := "accept licences: " + strings.Join(added, ", ") + " (by " + by + ")"
	if len(added) == 0 {
		summary = "licences already accepted"
	}
	p, code := s.offer(r, base, doc, summary)
	p.rebase = func(cur []byte) ([]byte, error) {
		d, _, err := accept(cur)
		return d, err
	}
	writeJSON(w, code, p)
}

// withAcceptances appends the subjects not yet accepted in cur to
// spec.licenseAcceptances.
func withAcceptances(cur []byte, subjects []string, urls map[string]string, by, at string) ([]byte, []string, error) {
	c, err := config.ParseCluster(cur)
	if err != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	for _, a := range c.Spec.LicenseAcceptances {
		have[a.Subject] = true
	}
	ed, err := config.NewEditor(cur)
	if err != nil {
		return nil, nil, err
	}
	var added []string
	for _, sub := range subjects {
		if have[sub] {
			continue
		}
		have[sub] = true
		if err := ed.Append(config.LicenseAcceptance{Subject: sub, URL: urls[sub], AcceptedBy: by, AcceptedAt: at}, "licenseAcceptances"); err != nil {
			return nil, nil, err
		}
		added = append(added, sub)
	}
	doc, err := ed.Bytes()
	return doc, added, err
}
