package config

import (
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

const hospitalExample = "../../examples/cluster.hospital.yaml"

var ddx = Options{Features: map[string]bool{"ddx": true}}

func TestHospitalExample(t *testing.T) {
	c := mustCluster(t, hospitalExample)
	if a := c.Spec.Models[1].Adapters; len(a) != 1 || a[0].Name != "spetaar-ddx" || a[0].Rank != 16 || a[0].Entitlement != "ddx" {
		t.Fatalf("adapters decoded wrong: %+v", a)
	}
	// Not entitled: valid, with the adapter and its rule reported inactive.
	r := Validate(c, mustCatalog(t), Options{})
	assertWarning(t, r, `adapter "spetaar-ddx" is not served: entitlement "ddx" is not present`)
	assertWarning(t, r, `rule "differential" is inactive`)
	rt := c.ActiveRoutes(mustCatalog(t), nil)
	if len(rt.Rules) != 1 || rt.Rules[0].Target != "medgemma" {
		t.Fatalf("unentitled routes = %+v", rt.Rules)
	}
	// Entitled: the rule targets the adapter like a deployment.
	if r := Validate(c, mustCatalog(t), ddx); !r.OK() {
		t.Fatalf("unexpected errors: %v", r.Errors())
	}
	rt = c.ActiveRoutes(mustCatalog(t), ddx.Features)
	if len(rt.Rules) != 2 || rt.Rules[0].Target != "spetaar-ddx" || rt.Fast != "ornith" {
		t.Fatalf("entitled routes = %+v", rt)
	}
}

func TestServedAdapters(t *testing.T) {
	off := false
	d := ModelDeployment{Name: "base", Adapters: []Adapter{{Name: "a"}, {Name: "b", Entitlement: "x"}}}
	if got := d.ServedAdapters(nil); len(got) != 1 || got[0].Name != "a" {
		t.Errorf("without x: %+v", got)
	}
	if got := d.ServedAdapters(map[string]bool{"x": true}); len(got) != 2 {
		t.Errorf("with x: %+v", got)
	}
	d.Enabled = &off
	if got := d.ServedAdapters(map[string]bool{"x": true}); len(got) != 0 {
		t.Errorf("disabled deployment serves %+v", got)
	}
	d.Guardrails = []string{"g"}
	if g := d.AdapterGuardrails(d.Adapters[0]); len(g) != 1 || g[0] != "g" {
		t.Errorf("inherited guardrails = %v", g)
	}
	if g := d.AdapterGuardrails(Adapter{Guardrails: []string{}}); len(g) != 0 {
		t.Errorf("an adapter's own (empty) guardrails must win: %v", g)
	}
}

func TestAdapterRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Cluster)
		opts    Options
		path    string
		message string
	}{
		{"adapter named like a deployment", func(c *Cluster) {
			c.Spec.Models[1].Adapters[0].Name = "medgemma"
		}, ddx, "spec.models[1].adapters[0].name", "already used by model deployment medgemma"},
		{"adapter named twice", func(c *Cluster) {
			c.Spec.Models[2].Adapters = []Adapter{{Name: "spetaar-ddx", Path: "/a", Rank: 8}}
		}, ddx, "spec.models[2].adapters[0].name", "already used by adapter spetaar-ddx on gpt-oss-20b"},
		{"adapter named like a provider model", func(c *Cluster) {
			c.Spec.Routing.Providers = []Provider{{Name: "oai", Type: ProviderOpenAI, SecretRef: "k", AllowEgress: true,
				Guardrails: nil, Models: []ProviderModel{{Name: "spetaar-ddx"}}}}
		}, ddx, "spec.routing.providers[0].models[0].name", "already used by adapter spetaar-ddx"},
		{"relative path", func(c *Cluster) {
			c.Spec.Models[1].Adapters[0].Path = "/var/lib/../oaf/x"
		}, ddx, "spec.models[1].adapters[0].path", "absolute, clean"},
		{"adapter on an embedding model", func(c *Cluster) {
			c.Spec.Models[3].Adapters = []Adapter{{Name: "emb-lora", Path: "/a", Rank: 8}}
		}, ddx, "spec.models[3].adapters", "embedding model"},
		{"lora-modules by hand", func(c *Cluster) {
			c.Spec.Models[1].Args = []string{"--lora-modules", "x=/y"}
		}, ddx, "spec.models[1].args", "rendered from adapters"},
		{"unknown adapter guardrail", func(c *Cluster) {
			c.Spec.Models[1].Adapters[0].Guardrails = []string{"nope"}
		}, ddx, "spec.models[1].adapters[0].guardrails[0]", "no guardrail policy"},
		{"fast is an unentitled adapter", func(c *Cluster) {
			c.Spec.Routing.Defaults.Fast = "spetaar-ddx"
		}, Options{}, "spec.routing.defaults.fast", `adapter "spetaar-ddx" is not served`},
		{"adapter of a disabled deployment", func(c *Cluster) {
			off := false
			c.Spec.Models[1].Enabled = &off
			c.Spec.Routing.Rules[0].Requires = nil
		}, ddx, "spec.routing.rules[0].target", "not an enabled deployment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustCluster(t, hospitalExample)
			tc.mutate(c)
			r := Validate(c, mustCatalog(t), tc.opts)
			for _, e := range r.Errors() {
				if e.Path == tc.path && strings.Contains(e.Message, tc.message) {
					return
				}
			}
			t.Fatalf("want error at %s containing %q; got %v", tc.path, tc.message, r.Errors())
		})
	}
}

func TestAdapterAsFastAndKeyModel(t *testing.T) {
	c := mustCluster(t, hospitalExample)
	c.Spec.Routing.Defaults.Fallback = "spetaar-ddx"
	r := Validate(c, mustCatalog(t), ddx)
	if !r.OK() {
		t.Fatalf("an entitled adapter is a valid fallback: %v", r.Errors())
	}
	// A key may name the adapter before its entitlement arrives.
	for _, w := range Validate(mustCluster(t, hospitalExample), mustCatalog(t), Options{}).Warnings() {
		if strings.HasPrefix(w.Path, "spec.routing.keys") {
			t.Fatalf("key warning: %v", w)
		}
	}
	// An ungated rule to an unentitled adapter is inactive, not an error.
	c = mustCluster(t, hospitalExample)
	c.Spec.Routing.Rules[0].Requires = nil
	assertWarning(t, Validate(c, mustCatalog(t), Options{}), `adapter "spetaar-ddx" needs entitlement "ddx"`)
}

func TestCheckAdapterFiles(t *testing.T) {
	c := mustCluster(t, hospitalExample)
	cat := mustCatalog(t)
	dir := "var/lib/oaf/adapters/spetaar-ddx/"
	cfg := func(r int, base string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(`{"peft_type":"LORA","r":` + strconv.Itoa(r) + `,"lora_alpha":32,"base_model_name_or_path":"` + base + `","target_modules":["q_proj","k_proj","v_proj","o_proj"]}`)}
	}
	cases := []struct {
		name string
		fs   fstest.MapFS
		sev  Severity
		want string // "" = no issue
	}{
		{"absent here", fstest.MapFS{}, SeverityWarning, "is not on this host"},
		{"good", fstest.MapFS{dir + "adapter_config.json": cfg(16, "openai/gpt-oss-20b"), dir + "adapter_model.safetensors": {}}, 0, ""},
		{"local base path", fstest.MapFS{dir + "adapter_config.json": cfg(16, "/models/gpt-oss-20b"), dir + "adapter_model.safetensors": {}}, 0, ""},
		{"rank too small", fstest.MapFS{dir + "adapter_config.json": cfg(32, "openai/gpt-oss-20b"), dir + "adapter_model.safetensors": {}}, SeverityError, "set rank: 32"},
		{"rank larger than needed", fstest.MapFS{dir + "adapter_config.json": cfg(8, "openai/gpt-oss-20b"), dir + "adapter_model.safetensors": {}}, SeverityWarning, "r=8, but rank is 16"},
		{"no weights", fstest.MapFS{dir + "adapter_config.json": cfg(16, "openai/gpt-oss-20b")}, SeverityError, "no adapter_model.safetensors"},
		{"not an adapter", fstest.MapFS{dir + "README.md": {}}, SeverityError, "no adapter_config.json"},
		{"other base", fstest.MapFS{dir + "adapter_config.json": cfg(16, "Qwen/Qwen3-8B"), dir + "adapter_model.safetensors": {}}, SeverityWarning, "trained on Qwen/Qwen3-8B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckAdapterFiles(c, cat, tc.fs)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected issues: %v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Severity != tc.sev || !strings.Contains(got[0].Message, tc.want) {
				t.Fatalf("want one issue (severity %d) containing %q; got %v", tc.sev, tc.want, got)
			}
		})
	}
}
