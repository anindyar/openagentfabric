package config

import (
	"os"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/bundle"
)

func thirdPartyMD(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../THIRD_PARTY.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPinnedVersionsAreValid guards the embedded release pins: schema, digests,
// arm64, THIRD_PARTY.md rows and licence acceptance.
func TestPinnedVersionsAreValid(t *testing.T) {
	v, err := ParseVersions(bundle.VersionsYAML)
	if err != nil {
		t.Fatal(err)
	}
	if r := ValidateVersions(v, thirdPartyMD(t)); !r.OK() {
		t.Fatalf("versions.yaml: %v", r.Errors())
	}
	// The full stack from CLAUDE.md must be pinned.
	want := []string{"agentgateway", "vsr", "vllm", "langfuse-web", "langfuse-worker", "postgres",
		"clickhouse", "valkey", "seaweedfs", "prometheus", "node-exporter", "grafana"}
	have := map[string]bool{}
	for _, c := range v.Components {
		have[c.Name] = true
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("component %q is not pinned", n)
		}
	}
}

func TestVLLMNeedsAcceptance(t *testing.T) {
	v, err := ParseVersions(bundle.VersionsYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Components {
		got := c.License.NeedsAcceptance()
		if want := c.Name == "vllm" || c.Name == "vllm-spark-b12x" || c.Name == "vllm-glm53"; got != want {
			t.Errorf("%s: NeedsAcceptance = %v, want %v", c.Name, got, want)
		}
	}
}

func TestVersionsRejections(t *testing.T) {
	base := string(bundle.VersionsYAML)
	schemaCases := []struct{ name, old, new string }{
		{"tag without digest", "agentgateway:v1.6.0@sha256:9d3e6044ddcdc0878b1787f77bd401252b95e22684203fb5e874c4c42d2ed90c", "agentgateway:v1.6.0"},
		{"no arm64", "platforms: [linux/arm64, linux/amd64]\n    upstream: { repo: agentgateway", "platforms: [linux/amd64]\n    upstream: { repo: agentgateway"},
		{"bad licence hash", "sha256: 3b1ee5b1e14fda40515c18ec2f0796d632e65a20b2aca8f017c654bc26ca77bd", "sha256: abc"},
	}
	for _, tc := range schemaCases {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Replace(base, tc.old, tc.new, 1)
			if doc == base {
				t.Fatalf("test setup: %q not found", tc.old)
			}
			if _, err := ParseVersions([]byte(doc)); err == nil {
				t.Fatal("expected a schema error")
			}
		})
	}

	semantic := []struct {
		name, path, msg string
		mutate          func(*Versions)
	}{
		{"duplicate name", "components[1].name", "duplicate component", func(v *Versions) { v.Components[1].Name = v.Components[0].Name }},
		{"missing THIRD_PARTY row", "components[0].thirdParty", "no row named", func(v *Versions) { v.Components[0].ThirdParty = "Unlisted" }},
		{"acceptance without acceptUrl", "components[2].license.acceptUrl", "needs user acceptance", func(v *Versions) { v.Components[2].License.AcceptURL = "" }},
		{"non-allowlisted licence", "components[0].license.acceptUrl", "needs user acceptance", func(v *Versions) { v.Components[0].License.SPDX = "BUSL-1.1" }},
	}
	for _, tc := range semantic {
		t.Run(tc.name, func(t *testing.T) {
			v, err := ParseVersions(bundle.VersionsYAML)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(v)
			for _, i := range ValidateVersions(v, thirdPartyMD(t)).Errors() {
				if i.Path == tc.path && strings.Contains(i.Message, tc.msg) {
					return
				}
			}
			t.Fatalf("want error at %s containing %q", tc.path, tc.msg)
		})
	}
}
