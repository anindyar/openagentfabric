package render

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"openagentfabric.dev/oaf/internal/config"
)

// hospitalInput is examples/cluster.hospital.yaml with the ddx entitlement
// granted, so the spetaar-ddx adapter is served.
func hospitalInput(t *testing.T) Input {
	t.Helper()
	in := singleInput(t)
	c, err := config.LoadCluster("../../examples/cluster.hospital.yaml")
	if err != nil {
		t.Fatal(err)
	}
	in.Cluster, in.Features = c, map[string]bool{"ddx": true}
	return in
}

func TestGoldenHospital(t *testing.T) { checkGolden(t, "hospital", mustRender(t, hospitalInput(t))) }

func projectFile(t *testing.T, ps []Project, project, file string) string {
	t.Helper()
	for _, p := range ps {
		if p.Name != project {
			continue
		}
		for _, f := range p.Files {
			if f.Path == file {
				return string(f.Content)
			}
		}
	}
	return ""
}

func vllmCommand(t *testing.T, ps []Project, project string) ([]string, []string) {
	t.Helper()
	var cf composeFile
	if err := yaml.Unmarshal([]byte(projectFile(t, ps, project, "compose.yaml")), &cf); err != nil {
		t.Fatal(err)
	}
	s := cf.Services["vllm"]
	if s == nil {
		t.Fatalf("%s has no vllm service", project)
	}
	return s.Command, s.Volumes
}

// The adapter is served by its base's vLLM, by name through the gateway, and
// is a vSR model that rules can pick.
func TestAdapterServed(t *testing.T) {
	ps := mustRender(t, hospitalInput(t))
	cmd, vols := vllmCommand(t, ps, "model-gpt-oss-20b")
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "--enable-lora --max-lora-rank 16 --lora-modules spetaar-ddx=/adapters/spetaar-ddx") {
		t.Errorf("LoRA args missing: %s", joined)
	}
	if strings.Contains(joined, "--max-loras") {
		t.Errorf("one adapter needs no --max-loras: %s", joined)
	}
	if !slices.Contains(vols, "/var/lib/oaf/adapters/spetaar-ddx:/adapters/spetaar-ddx:ro") {
		t.Errorf("adapter not mounted read-only: %v", vols)
	}
	if cmd, _ := vllmCommand(t, ps, "model-medgemma"); slices.Contains(cmd, "--enable-lora") {
		t.Errorf("LoRA on a deployment without adapters")
	}
	gw := projectFile(t, ps, "gateway", "config.yaml")
	if !strings.Contains(gw, "name: spetaar-ddx") || !strings.Contains(gw, "model: spetaar-ddx") {
		t.Errorf("gateway does not serve spetaar-ddx:\n%s", gw)
	}
	vsr := projectFile(t, ps, "router", "vsr.yaml")
	if !strings.Contains(vsr, "name: spetaar-ddx") || !strings.Contains(vsr, "model: spetaar-ddx") {
		t.Errorf("vSR config does not know spetaar-ddx:\n%s", vsr)
	}
	if envoy := projectFile(t, ps, "router", "envoy.yaml"); !strings.Contains(envoy, "exact: spetaar-ddx") {
		t.Errorf("envoy has no route to spetaar-ddx")
	}
}

// Without the entitlement nothing of the adapter is rendered, and its rule is
// left out; the base model still serves.
func TestAdapterNotEntitled(t *testing.T) {
	in := hospitalInput(t)
	in.Features = nil
	ps := mustRender(t, in)
	cmd, vols := vllmCommand(t, ps, "model-gpt-oss-20b")
	if slices.Contains(cmd, "--enable-lora") || len(vols) != 1 {
		t.Errorf("unentitled adapter rendered: %v %v", cmd, vols)
	}
	for _, f := range []string{projectFile(t, ps, "gateway", "config.yaml"), projectFile(t, ps, "router", "vsr.yaml"), projectFile(t, ps, "router", "envoy.yaml")} {
		// (The app key may still list it: it is allowed once served.)
		if strings.Contains(f, "name: spetaar-ddx") || strings.Contains(f, "model: spetaar-ddx") || strings.Contains(f, "exact: spetaar-ddx") {
			t.Errorf("unentitled adapter in a config:\n%s", f)
		}
	}
	if gw := projectFile(t, ps, "gateway", "config.yaml"); !strings.Contains(gw, "name: gpt-oss-20b") {
		t.Errorf("base model not served")
	}
}

// An adapter's guardrails replace its deployment's; unset, it has them.
func TestAdapterGuardrails(t *testing.T) {
	in := hospitalInput(t)
	in.Cluster.Spec.Routing.Guardrails = []config.GuardrailPolicy{config.BasicGuardrail()}
	in.Cluster.Spec.Models[1].Guardrails = []string{"basic"}
	gw := projectFile(t, mustRender(t, in), "gateway", "config.yaml")
	var cfg struct {
		LLM struct {
			Models []map[string]any `yaml:"models"`
		} `yaml:"llm"`
	}
	if err := yaml.Unmarshal([]byte(gw), &cfg); err != nil {
		t.Fatal(err)
	}
	guarded := map[string]bool{}
	for _, m := range cfg.LLM.Models {
		guarded[m["name"].(string)] = m["guardrails"] != nil
	}
	if !guarded["gpt-oss-20b"] || !guarded["spetaar-ddx"] || guarded["medgemma"] {
		t.Errorf("inherited guardrails: %v", guarded)
	}
	in.Cluster.Spec.Models[1].Adapters[0].Guardrails = []string{}
	gw = projectFile(t, mustRender(t, in), "gateway", "config.yaml")
	if err := yaml.Unmarshal([]byte(gw), &cfg); err != nil {
		t.Fatal(err)
	}
	for _, m := range cfg.LLM.Models {
		if m["name"] == "spetaar-ddx" && m["guardrails"] != nil {
			t.Errorf("an adapter's own empty list must override the deployment's")
		}
	}
}

func TestLoraArgs(t *testing.T) {
	two := []config.Adapter{{Name: "a", Rank: 12}, {Name: "team/b.v2", Rank: 4}}
	got := strings.Join(loraArgs(two, nil), " ")
	if got != "--enable-lora --max-lora-rank 16 --max-loras 2 --lora-modules a=/adapters/a team/b.v2="+adapterMount("team/b.v2") {
		t.Errorf("two adapters: %s", got)
	}
	if !strings.HasPrefix(adapterMount("team/b.v2"), "/adapters/team-b-v2-") {
		t.Errorf("mount of a name with / and . = %s", adapterMount("team/b.v2"))
	}
	// What the profile or deployment already sets is not repeated.
	got = strings.Join(loraArgs(two[:1], []string{"--enable-lora", "--max-lora-rank=64"}), " ")
	if got != "--lora-modules a=/adapters/a" {
		t.Errorf("with profile flags: %s", got)
	}
	if loraArgs(nil, nil) != nil {
		t.Error("no adapters, no args")
	}
	if got := strings.Join(loraArgs([]config.Adapter{{Name: "x", Rank: 300}}, nil), " "); !strings.Contains(got, "--max-lora-rank 320") {
		t.Errorf("rank 300 rounds up to 320: %s", got)
	}
}
