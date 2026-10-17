package config

// permissiveSPDX is the licence allowlist from docs/licence-audit.md §3.7.
// Artefacts under any other licence (or none) need explicit user acceptance.
var permissiveSPDX = map[string]bool{
	"Apache-2.0":   true,
	"MIT":          true,
	"BSD-2-Clause": true,
	"BSD-3-Clause": true,
	"ISC":          true,
	"PostgreSQL":   true,
	"GPL-2.0-only": true, "GPL-2.0-or-later": true,
	"GPL-3.0-only": true, "GPL-3.0-or-later": true,
}

// NeedsAcceptance reports whether the user must accept the licence upstream
// before OAF may fetch or deploy the artefact.
func (l ModelLicense) NeedsAcceptance() bool {
	return l.Gated || !permissiveSPDX[l.SPDX]
}

// AcceptanceSubject is the licenseAcceptances subject for a catalog model.
func AcceptanceSubject(modelID string) string { return "model/" + modelID }
