package config

import (
	"sort"
	"strings"
)

// NeededAcceptance is a licence a cluster's enabled deployments need accepted
// before OAF may pull or run them: an engine image that ships third-party
// terms (NVIDIA CUDA) or a model whose licence is gated or off the allowlist.
// Apps show these to a person, who reads them and accepts.
type NeededAcceptance struct {
	Subject string `json:"subject"` // image/<component> or model/<catalog id>
	Kind    string `json:"kind"`    // image | model
	Title   string `json:"title"`   // the terms, e.g. "NVIDIA CUDA Toolkit EULA"
	For     string `json:"for"`     // what needs it: the component or model, and the deployments
	URL     string `json:"url"`     // where to read the terms
}

// NeededAcceptances lists the acceptances the enabled deployments of c need
// and cluster.yaml does not record yet, sorted by subject.
func NeededAcceptances(c *Cluster, cat *Catalog, v *Versions) []NeededAcceptance {
	accepted := map[string]bool{}
	for _, a := range c.Spec.LicenseAcceptances {
		accepted[a.Subject] = true
	}
	need := map[string]*NeededAcceptance{}
	users := map[string][]string{}
	add := func(n NeededAcceptance, deployment string) {
		if accepted[n.Subject] {
			return
		}
		if need[n.Subject] == nil {
			need[n.Subject] = &n
		}
		users[n.Subject] = append(users[n.Subject], deployment)
	}
	for _, d := range c.Spec.Models {
		if !d.IsEnabled() {
			continue
		}
		m, ok := cat.Lookup(d.CatalogRef)
		if !ok {
			continue
		}
		if m.License.NeedsAcceptance() {
			url := m.License.AcceptURL
			if url == "" {
				url = m.License.URL
			}
			title := m.License.Name
			if title == "" {
				title = m.License.SPDX + " licence"
			}
			add(NeededAcceptance{Subject: AcceptanceSubject(m.ID), Kind: "model", Title: title, For: m.ID, URL: url}, d.Name)
		}
		if v == nil {
			continue
		}
		prof, err := Profile(d, m, len(c.PlacementNodes(d.Placement)))
		if err != nil {
			continue
		}
		comp := prof.EngineComponent()
		for _, vc := range v.Components {
			if vc.Name != comp || !vc.License.NeedsAcceptance() {
				continue
			}
			var titles []string
			for _, t := range vc.License.IncludedTerms {
				titles = append(titles, t.Name)
			}
			if len(titles) == 0 {
				titles = []string{vc.License.SPDX + " licence"}
			}
			add(NeededAcceptance{Subject: ImageAcceptanceSubject(comp), Kind: "image", Title: strings.Join(titles, ", "),
				For: comp + " engine image", URL: vc.License.AcceptURL}, d.Name)
		}
	}
	out := make([]NeededAcceptance, 0, len(need))
	for s, n := range need {
		n.For += " (for " + strings.Join(users[s], ", ") + ")"
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

// AcceptableURL is the terms URL of a subject OAF knows (a bundle component
// or catalog model that needs acceptance), or false.
func AcceptableURL(subject string, cat *Catalog, v *Versions) (string, bool) {
	if comp, ok := strings.CutPrefix(subject, "image/"); ok && v != nil {
		for _, vc := range v.Components {
			if vc.Name == comp && vc.License.NeedsAcceptance() {
				return vc.License.AcceptURL, true
			}
		}
	}
	if id, ok := strings.CutPrefix(subject, "model/"); ok && cat != nil {
		if m, found := cat.Lookup(id); found && m.License.NeedsAcceptance() {
			if m.License.AcceptURL != "" {
				return m.License.AcceptURL, true
			}
			return m.License.URL, true
		}
	}
	return "", false
}
