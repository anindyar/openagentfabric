package config

import (
	"fmt"
	"strings"
)

// ValidateVersions checks versions.yaml rules that JSON Schema cannot express.
// thirdPartyMD is the content of THIRD_PARTY.md: every component must have a
// row there (docs/licence-audit.md §4.2).
func ValidateVersions(v *Versions, thirdPartyMD string) Report {
	rows := thirdPartyRows(thirdPartyMD)
	var out []Issue
	add := func(path, format string, args ...any) {
		out = append(out, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	names := map[string]bool{}
	for i, c := range v.Components {
		p := fmt.Sprintf("components[%d]", i)
		if names[c.Name] {
			add(p+".name", "duplicate component %q", c.Name)
		}
		names[c.Name] = true
		if !rows[c.ThirdParty] {
			add(p+".thirdParty", "no row named %q in THIRD_PARTY.md", c.ThirdParty)
		}
		if c.License.NeedsAcceptance() && c.License.AcceptURL == "" {
			add(p+".license.acceptUrl", "required: %s needs user acceptance", c.Name)
		}
	}
	return Report{Issues: out}
}

// thirdPartyRows returns the first-column names of the markdown tables in
// THIRD_PARTY.md, with formatting (bold, strikethrough, links) removed.
func thirdPartyRows(md string) map[string]bool {
	rows := map[string]bool{}
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		name := strings.NewReplacer("**", "", "~~", "", "`", "").Replace(strings.TrimSpace(cells[1]))
		rows[name] = true
	}
	return rows
}
