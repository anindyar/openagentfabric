package render

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Speech deployments are served by name through the gateway (their own
// endpoints), never through vSR, and Kokoro runs as a server engine.
func TestSpeechDeployments(t *testing.T) {
	ps := mustRender(t, hospitalInput(t))

	cmd, vols := vllmCommand(t, ps, "model-stt")
	if !strings.HasPrefix(strings.Join(cmd, " "), "/models/whisper-large-v3-turbo --served-model-name stt") ||
		!slices.Contains(vols, "/var/lib/oaf/models/whisper-large-v3-turbo:/models/whisper-large-v3-turbo:ro") {
		t.Errorf("stt: %v %v", cmd, vols)
	}
	var cf composeFile
	if err := yaml.Unmarshal([]byte(projectFile(t, ps, "model-stt", "compose.yaml")), &cf); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cf.Services["vllm"].Image, "192.168.1.11:5000/oaf/vllm-audio@sha256:") {
		t.Errorf("stt image = %s", cf.Services["vllm"].Image)
	}

	cf = composeFile{}
	if err := yaml.Unmarshal([]byte(projectFile(t, ps, "model-tts", "compose.yaml")), &cf); err != nil {
		t.Fatal(err)
	}
	s := cf.Services["server"]
	if s == nil {
		t.Fatal("model-tts has no server service")
	}
	if !strings.HasPrefix(s.Image, "ghcr.io/remsky/kokoro-fastapi-cpu:v0.9.2@sha256:") || s.GPUs != "" || len(s.Command) != 0 ||
		s.NetworkMode != "host" || s.Environment["PORT"] != "8005" || s.Environment["HOST"] != "0.0.0.0" ||
		s.Environment["DOWNLOAD_MODEL"] != "false" || s.MemLimit != "2491m" || len(s.Volumes) != 0 ||
		!strings.Contains(strings.Join(s.Healthcheck.Test, " "), "127.0.0.1:8005/health") {
		t.Errorf("tts service = %+v", s)
	}
	for _, p := range ps {
		if p.Name == "model-tts" && p.Weights != nil {
			t.Error("bundled weights must not be downloaded")
		}
	}

	gw := projectFile(t, ps, "gateway", "config.yaml")
	var g struct {
		LLM struct {
			Models []struct {
				Name   string            `yaml:"name"`
				Params map[string]string `yaml:"params"`
			} `yaml:"models"`
		} `yaml:"llm"`
	}
	if err := yaml.Unmarshal([]byte(gw), &g); err != nil {
		t.Fatal(err)
	}
	upstream := map[string]string{}
	for _, m := range g.LLM.Models {
		upstream[m.Name] = m.Params["model"] + " " + m.Params["baseUrl"]
	}
	if upstream["stt"] != "stt http://192.168.1.11:8004/v1" || upstream["tts"] != "kokoro http://192.168.1.11:8005/v1" {
		t.Errorf("gateway speech backends: %v", upstream)
	}
	for _, f := range []string{"vsr.yaml", "envoy.yaml"} {
		if c := projectFile(t, ps, "router", f); strings.Contains(c, "stt") || strings.Contains(c, "tts") {
			t.Errorf("speech model in %s", f)
		}
	}
	if prom := projectFile(t, ps, "metrics", "prometheus.yml"); strings.Contains(prom, ":8005") || !strings.Contains(prom, ":8004") {
		t.Errorf("prometheus scrapes the wrong speech targets:\n%s", prom)
	}
}
