package render

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"openagentfabric.dev/oaf/internal/config"
)

// providersInput is the pair example plus one provider of each type and the
// basic guardrail on two of them and on the fast local model.
func providersInput(t *testing.T) Input {
	t.Helper()
	in := pairInput(t)
	basic := config.BasicGuardrail()
	in.Cluster.Spec.Routing.Guardrails = []config.GuardrailPolicy{basic, {
		Name:    "hook",
		Request: []config.Guard{{Webhook: &config.WebhookGuard{Host: "guard.example.internal:9000"}}},
	}}
	in.Cluster.Spec.Routing.Providers = []config.Provider{
		{Name: "openai", Type: config.ProviderOpenAI, SecretRef: "provider-openai", AllowEgress: true, Guardrails: []string{"basic"},
			Models: []config.ProviderModel{{Name: "gpt-4o", Upstream: "gpt-4o-2024-08-06"}}},
		{Name: "claude", Type: config.ProviderAnthropic, SecretRef: "provider-claude", AllowEgress: true, Guardrails: []string{"basic", "hook"},
			Models: []config.ProviderModel{{Name: "claude-sonnet"}}},
		{Name: "gemini", Type: config.ProviderGemini, SecretRef: "provider-gemini", AllowEgress: true,
			Models: []config.ProviderModel{{Name: "gemini-pro"}}},
		{Name: "azure", Type: config.ProviderAzureOpenAI, SecretRef: "provider-azure", AllowEgress: true,
			Azure:  &config.AzureSettings{ResourceName: "acme-openai", APIVersion: "2024-10-21"},
			Models: []config.ProviderModel{{Name: "azure-gpt", Upstream: "gpt-4o"}}},
		{Name: "lab", Type: config.ProviderOpenAICompatible, Endpoint: "http://llm.example.internal:8010/v1", AllowEgress: true,
			Models: []config.ProviderModel{{Name: "lab-model"}}},
	}
	for i := range in.Cluster.Spec.Models {
		if in.Cluster.Spec.Models[i].Name == in.Cluster.Spec.Routing.Defaults.Fast {
			in.Cluster.Spec.Models[i].Guardrails = []string{"basic"}
		}
	}
	return in
}

func TestProvidersRender(t *testing.T) {
	in := providersInput(t)
	if r := config.Validate(in.Cluster, in.Catalog, config.Options{}); !r.OK() {
		t.Fatalf("example does not validate: %v", r.Errors())
	}
	var gw *Project
	ps := mustRender(t, in)
	for i := range ps {
		if ps[i].Name == ProjectGateway {
			gw = &ps[i]
		}
	}
	if gw == nil {
		t.Fatal("no gateway project")
	}
	var cfg, env []byte
	for _, f := range gw.Files {
		switch f.Path {
		case "config.yaml":
			cfg = f.Content
		case ".env":
			env = f.Content
		}
	}

	golden := filepath.Join("testdata", "providers", "config.yaml")
	if *update {
		_ = os.MkdirAll(filepath.Dir(golden), 0o755)
		if err := os.WriteFile(golden, cfg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cfg, want) {
		t.Fatalf("gateway config.yaml differs from %s (go test ./internal/render -update)\n%s", golden, cfg)
	}

	// Keys live in .env, never in config.yaml.
	for _, ref := range []string{"provider-openai", "provider-claude", "provider-gemini", "provider-azure"} {
		v := fakeSecret(ref)
		if bytes.Contains(cfg, []byte(v)) {
			t.Errorf("secret %s is in config.yaml", ref)
		}
		if !bytes.Contains(env, []byte(v)) {
			t.Errorf("secret %s is not in .env", ref)
		}
	}

	var doc struct {
		LLM struct {
			Models []struct {
				Name       string         `yaml:"name"`
				Provider   string         `yaml:"provider"`
				Params     map[string]any `yaml:"params"`
				Guardrails map[string]any `yaml:"guardrails"`
			} `yaml:"models"`
		} `yaml:"llm"`
	}
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	guarded := map[string]bool{}
	for _, m := range doc.LLM.Models {
		got[m.Name] = m.Provider
		guarded[m.Name] = m.Guardrails != nil
	}
	for name, prov := range map[string]string{"gpt-4o": "openAI", "claude-sonnet": "anthropic", "gemini-pro": "gemini", "azure-gpt": "azure", "lab-model": "openAI"} {
		if got[name] != prov {
			t.Errorf("%s: provider %q, want %q", name, got[name], prov)
		}
	}
	fast := in.Cluster.Spec.Routing.Defaults.Fast
	for _, name := range []string{"gpt-4o", "claude-sonnet", fast, "auto"} {
		if !guarded[name] {
			t.Errorf("%s has no guardrails", name)
		}
	}
	for _, name := range []string{"gemini-pro", "lab-model"} {
		if guarded[name] {
			t.Errorf("%s has guardrails it was not given", name)
		}
	}
}

// A provider whose key was never set fails the render with instructions,
// instead of the key being generated.
func TestProviderKeyNeverGenerated(t *testing.T) {
	in := providersInput(t)
	store := lookupOnly{}
	in.Secrets = store
	_, err := Render(in)
	if err == nil || !strings.Contains(err.Error(), "never generated") {
		t.Fatalf("render with a missing provider key: %v", err)
	}
}

// lookupOnly behaves like a secrets.Store with no provider keys set.
type lookupOnly struct{}

func (lookupOnly) Value(name string) (string, error) { return fakeSecret(name), nil }
func (lookupOnly) Lookup(name string) (string, bool, error) {
	if strings.HasPrefix(name, "provider-") {
		return "", false, nil
	}
	return fakeSecret(name), true, nil
}
