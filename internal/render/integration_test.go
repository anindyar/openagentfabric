//go:build integration

package render

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/node"
)

// TestRenderedStackOnDocker checks every rendered project with `docker compose
// config`, then runs the control-node projects (Langfuse + stores, Prometheus +
// Grafana, agentgateway, node_exporter) on real docker through the node agent
// and probes them. vLLM needs a GPU, so it is only config-checked here.
// Run in CI: make integration.
func TestRenderedStackOnDocker(t *testing.T) {
	in := pairInput(t)
	in.Cluster.Spec.Nodes[0].Address = "127.0.0.1" // spark-1 is this machine
	projects := mustRender(t, in)

	for _, p := range projects {
		dir := filepath.Join(t.TempDir(), p.Name)
		for _, f := range p.Files {
			dst := filepath.Join(dir, f.Path)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, f.Content, os.FileMode(f.Mode)); err != nil {
				t.Fatal(err)
			}
		}
		out, err := exec.Command("docker", "compose", "--project-directory", dir, "--file", filepath.Join(dir, "compose.yaml"), "config", "--quiet").CombinedOutput()
		if err != nil {
			t.Fatalf("%s/%s: docker compose config: %v\n%s", p.Node, p.Name, err, out)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s := &node.Server{StateDir: t.TempDir(), Compose: node.Compose{Runner: hostexec.Local{}}}
	for _, p := range projects {
		if p.Node != "spark-1" || strings.HasPrefix(p.Name, modelProjectPrefix) {
			continue
		}
		req := &nodev1.ApplyProjectRequest{Name: p.Name}
		for _, f := range p.Files {
			req.Files = append(req.Files, &nodev1.File{Path: f.Path, Mode: f.Mode, Content: f.Content})
		}
		name := p.Name
		t.Cleanup(func() {
			if t.Failed() {
				out, _ := exec.Command("docker", "compose", "--project-name", node.ProjectPrefix+name, "logs", "--tail", "60").CombinedOutput()
				t.Logf("logs of %s:\n%s", name, out)
			}
			_, _ = s.RemoveProject(context.Background(), &nodev1.RemoveProjectRequest{Name: name, RemoveVolumes: true})
		})
		if _, err := s.ApplyProject(ctx, req); err != nil {
			t.Fatalf("apply %s: %v", p.Name, err)
		}
	}

	for _, name := range []string{ProjectLangfuse, ProjectMetrics, ProjectGateway, ProjectNodeExporter} {
		waitHealthy(ctx, t, s, name)
	}

	key := fakeSecret("gateway-key-default")
	pk, sk := "pk-lf-"+fakeSecret(SecretLangfusePublicKey), "sk-lf-"+fakeSecret(SecretLangfuseSecretKey)
	probes := []struct {
		name   string
		req    func() *http.Request
		status func(int) bool
	}{
		{"langfuse health", get("http://127.0.0.1:3000/api/public/health"), is(200)},
		{"langfuse init project keys", func() *http.Request {
			r := get("http://127.0.0.1:3000/api/public/projects")()
			r.SetBasicAuth(pk, sk)
			return r
		}, is(200)},
		{"prometheus ready", get("http://127.0.0.1:9090/-/ready"), is(200)},
		{"grafana health", get("http://127.0.0.1:3001/api/health"), is(200)},
		{"node_exporter metrics", get("http://127.0.0.1:9100/metrics"), is(200)},
		{"gateway readiness", get("http://127.0.0.1:15021/healthz/ready"), is(200)},
		{"gateway rejects a missing key", chat(""), is(401)},
		{"gateway rejects a wrong key", chat("not-the-key"), is(401)},
		// The key is accepted; the vLLM backend is not running here, so the
		// request fails upstream (5xx), not at authentication.
		{"gateway accepts the generated key", chat(key), func(c int) bool { return c != 401 && c != 403 }},
	}
	for _, p := range probes {
		var code int
		deadline := time.Now().Add(2 * time.Minute)
		for {
			resp, err := http.DefaultClient.Do(p.req().WithContext(ctx))
			if err == nil {
				code = resp.StatusCode
				_ = resp.Body.Close()
				if p.status(code) {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Errorf("%s: last status %d, err %v", p.name, code, err)
				break
			}
			time.Sleep(3 * time.Second)
		}
	}
}

func waitHealthy(ctx context.Context, t *testing.T, s *node.Server, name string) {
	t.Helper()
	for {
		st, err := s.ProjectStatus(ctx, &nodev1.ProjectStatusRequest{Name: name})
		if err != nil {
			t.Fatalf("status %s: %v", name, err)
		}
		ready, summary := len(st.GetServices()) > 0, []string{}
		for _, svc := range st.GetServices() {
			summary = append(summary, fmt.Sprintf("%s=%s/%s", svc.GetName(), svc.GetState(), svc.GetHealth()))
			if svc.GetState() != "running" || svc.GetHealth() != "" && svc.GetHealth() != "healthy" {
				ready = false
			}
		}
		if ready {
			t.Logf("%s healthy: %s", name, strings.Join(summary, " "))
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s not healthy in time: %s", name, strings.Join(summary, " "))
		case <-time.After(5 * time.Second):
		}
	}
}

func get(url string) func() *http.Request {
	return func() *http.Request {
		r, _ := http.NewRequest(http.MethodGet, url, nil)
		return r
	}
}

func chat(key string) func() *http.Request {
	return func() *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/chat/completions",
			strings.NewReader(`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		return r
	}
}

func is(want int) func(int) bool { return func(c int) bool { return c == want } }
