package config

// permissiveSPDX is the licence allowlist from docs/licence-audit.md §3.7.
// Artefacts under any other licence (or none) need explicit user acceptance.
// Copyleft entries (GPL, AGPL) need no acceptance, but an offline bundle that
// includes them must carry the corresponding-source offer.
var permissiveSPDX = map[string]bool{
	"Apache-2.0":   true,
	"MIT":          true,
	"BSD-2-Clause": true,
	"BSD-3-Clause": true,
	"ISC":          true,
	"PostgreSQL":   true,
	"GPL-2.0-only": true, "GPL-2.0-or-later": true,
	"GPL-3.0-only": true, "GPL-3.0-or-later": true,
	"AGPL-3.0-only": true, "AGPL-3.0-or-later": true,
}

// NeedsAcceptance reports whether the user must accept the licence upstream
// before OAF may fetch or deploy the artefact.
func (l ModelLicense) NeedsAcceptance() bool {
	return l.Gated || !permissiveSPDX[l.SPDX]
}

// AcceptanceSubject is the licenseAcceptances subject for a catalog model.
func AcceptanceSubject(modelID string) string { return "model/" + modelID }

// NeedsAcceptance reports whether the user must accept terms before OAF pulls
// the image: its own licence is off the allowlist, or it ships third-party
// terms (such as NVIDIA CUDA).
func (l ComponentLicense) NeedsAcceptance() bool {
	return len(l.IncludedTerms) > 0 || !permissiveSPDX[l.SPDX]
}

// ImageAcceptanceSubject is the licenseAcceptances subject for a bundle component.
func ImageAcceptanceSubject(component string) string { return "image/" + component }
