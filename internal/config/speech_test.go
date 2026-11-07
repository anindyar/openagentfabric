package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestSpeechCatalog(t *testing.T) {
	cat := mustCatalog(t)
	stt, _ := cat.Lookup("whisper-large-v3-turbo")
	tts, _ := cat.Lookup("kokoro-82m")
	bge, _ := cat.Lookup("bge-m3")
	gpt, _ := cat.Lookup("gpt-oss-20b")
	if stt.Task() != TaskSpeechToText || tts.Task() != TaskTextToSpeech || bge.Task() != TaskEmbedding || gpt.Task() != TaskChat {
		t.Fatalf("tasks: %s %s %s %s", stt.Task(), tts.Task(), bge.Task(), gpt.Task())
	}
	if stt.IsChat() || tts.IsChat() || !gpt.IsChat() {
		t.Fatal("IsChat")
	}
	if p := tts.Profiles[0]; p.IsVLLM() || p.UpstreamModel("tts") != "kokoro" || tts.Source.Bundled == nil {
		t.Fatalf("kokoro profile: %+v", p)
	}
	if p := stt.Profiles[0]; !p.IsVLLM() || p.UpstreamModel("stt") != "stt" {
		t.Fatalf("whisper profile: %+v", p)
	}
}

func TestSpeechRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Cluster, *Catalog)
		path    string
		message string
	}{
		{"speech model as fast", func(c *Cluster, _ *Catalog) {
			c.Spec.Routing.Defaults.Fast = "stt"
		}, "spec.routing.defaults.fast", `"stt" is a speech-to-text model; it cannot answer chat requests`},
		{"speech model as a rule target", func(c *Cluster, _ *Catalog) {
			c.Spec.Routing.Rules[1].Target = "kokoro"
		}, "spec.routing.rules[1].target", `"kokoro" is a text-to-speech model`},
		{"adapter on a speech model", func(c *Cluster, _ *Catalog) {
			c.Spec.Models[4].Adapters = []Adapter{{Name: "x", Path: "/x", Rank: 8}}
		}, "spec.models[4].adapters", "speech-to-text model; LoRA adapters"},
		{"bundled weights in another engine", func(_ *Cluster, cat *Catalog) {
			m, _ := cat.Lookup("kokoro-82m")
			m.Source.Bundled.Component = "vllm"
		}, "catalog.models[", "the weights are bundled in vllm, but this profile runs kokoro-fastapi"},
		{"text-to-speech under another name", func(c *Cluster, _ *Catalog) {
			for i := range c.Spec.Models {
				if c.Spec.Models[i].Name == "kokoro" {
					c.Spec.Models[i].Name = "tts"
				}
			}
		}, "spec.models[", `kokoro-fastapi serves the model name "kokoro" and the gateway passes text-to-speech requests through unchanged; name this deployment "kokoro"`},
		{"server engine on a pair", func(_ *Cluster, cat *Catalog) {
			m, _ := cat.Lookup("kokoro-82m")
			m.Profiles = append(m.Profiles, ServingProf{Name: "pair", Engine: "kokoro-fastapi", Nodes: 2, TensorParallel: 2, GPUMemoryUtilization: 0.1, Server: m.Profiles[0].Server})
		}, "catalog.models[", "runs on one node"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cat := mustCluster(t, hospitalExample), mustCatalog(t)
			tc.mutate(c, cat)
			r := Validate(c, cat, ddx)
			for _, e := range r.Errors() {
				if strings.HasPrefix(e.Path, tc.path) && strings.Contains(e.Message, tc.message) {
					return
				}
			}
			t.Fatalf("want error at %s containing %q; got %v", tc.path, tc.message, r.Errors())
		})
	}
}

// A server engine's memory is capped at its share, so it is not charged
// vLLM's per-instance overhead; and its missing authentication is said.
func TestSpeechMemoryAndAuth(t *testing.T) {
	c, cat := mustCluster(t, hospitalExample), mustCatalog(t)
	r := Validate(c, cat, ddx)
	assertWarning(t, r, "kokoro (kokoro-fastapi) has no authentication of its own")
	// Five vLLMs (ornith, gpt-oss-20b, medgemma, bge-m3, stt), not six.
	want := 0.86 + 5*VLLMInstanceOverhead
	assertWarning(t, r, fmt.Sprintf("need %.2f of unified memory", want))
}
