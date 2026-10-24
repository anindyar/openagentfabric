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
// {subjects, accepted_by}. It returns a plan adding them to
// spec.licenseAcceptances; applying it also deploys what was waiting on them.
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
	base, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cur, err := config.ParseCluster(base)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	have := map[string]bool{}
	for _, a := range cur.Spec.LicenseAcceptances {
		have[a.Subject] = true
	}
	ed, err := config.NewEditor(base)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := s.cfg.Now().UTC().Format(time.RFC3339)
	var added []string
	for _, sub := range req.Subjects {
		url, ok := config.AcceptableURL(sub, cat, s.cfg.Versions)
		if !ok {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q is not a licence this bundle or catalog asks to accept", sub))
			return
		}
		if have[sub] {
			continue
		}
		if err := ed.Append(config.LicenseAcceptance{Subject: sub, URL: url, AcceptedBy: by, AcceptedAt: now}, "licenseAcceptances"); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		added = append(added, sub)
	}
	doc, err := ed.Bytes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	summary := "accept licences: " + strings.Join(added, ", ") + " (by " + by + ")"
	if len(added) == 0 {
		summary = "licences already accepted"
	}
	p, code := s.offer(r, base, doc, summary)
	writeJSON(w, code, p)
}
