package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// resetDocker answers docker listings and records every command.
type resetDocker struct{ ran []string }

func (f *resetDocker) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	f.ran = append(f.ran, line)
	switch {
	case strings.HasPrefix(line, "docker ps"):
		return []byte("c1 oaf-gateway\nc2 hms\nc3 oaf-model-a\n"), nil
	case line == "docker inspect --format {{.Image}} c1 c3":
		return []byte("sha256:gw\nsha256:vllm\n"), nil
	case strings.HasPrefix(line, "docker image inspect") && strings.HasSuffix(line, "sha256:vllm"):
		return []byte("10.0.0.1:5000/oaf/vllm-glm53:v9 glm53:v9 "), nil
	case strings.HasPrefix(line, "docker image inspect"):
		return []byte(""), nil
	case strings.HasPrefix(line, "docker volume ls"):
		return []byte("oaf-langfuse_pg oaf-langfuse\nhms_data hms\n"), nil
	case strings.HasPrefix(line, "docker network ls"):
		return []byte("oaf-gateway_default oaf-gateway\n"), nil
	}
	return nil, nil
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// Everything OAF created goes; other containers, volumes, netplan files and
// registry CAs stay.
func TestReset(t *testing.T) {
	root := t.TempDir()
	at := func(p string) string { return filepath.Join(root, p) }
	write(t, at("/etc/systemd/system/oaf-node.service"), "x")
	write(t, at("/etc/systemd/system/oaf-api.service"), "x")
	write(t, at("/etc/systemd/system/other.service"), "x")
	write(t, at("/etc/netplan/40-oaf-fabric.yaml"), "x")
	write(t, at("/etc/netplan/00-installer-config.yaml"), "x")
	write(t, at("/etc/oaf/pki/ca.crt"), "CLUSTER CA\n")
	write(t, at("/etc/docker/certs.d/10.0.0.1:5000/ca.crt"), "CLUSTER CA")
	write(t, at("/etc/docker/certs.d/registry.example:443/ca.crt"), "OTHER CA")
	write(t, at("/var/lib/oaf/models/m/w.safetensors"), "x")
	write(t, at("/var/lib/oaf/projects/gateway/compose.yaml"), "x")
	write(t, at("/opt/oaf/src/install.sh"), "x")
	write(t, at("/usr/local/bin/oaf"), "x")

	d := &resetDocker{}
	var out strings.Builder
	r := resetter{Run: d, Root: root, Out: &out}
	if err := r.reset(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/etc/systemd/system/oaf-node.service", "/etc/systemd/system/oaf-api.service",
		"/etc/netplan/40-oaf-fabric.yaml", "/etc/oaf", "/etc/docker/certs.d/10.0.0.1:5000", "/var/lib/oaf",
		"/opt/oaf", "/usr/local/bin/oaf"} {
		if exists(at(p)) {
			t.Errorf("%s still exists", p)
		}
	}
	for _, p := range []string{"/etc/systemd/system/other.service", "/etc/netplan/00-installer-config.yaml",
		"/etc/docker/certs.d/registry.example:443/ca.crt"} {
		if !exists(at(p)) {
			t.Errorf("%s was removed", p)
		}
	}
	for _, want := range []string{
		"systemctl disable --now oaf-node.service",
		"systemctl daemon-reload",
		"docker rm --force --volumes c1 c3",
		"docker volume rm oaf-langfuse_pg",
		"docker network rm oaf-gateway_default",
		"docker image rm sha256:gw",
		"docker image rm 10.0.0.1:5000/oaf/vllm-glm53:v9 glm53:v9 sha256:vllm",
		"netplan apply",
	} {
		if !slices.Contains(d.ran, want) {
			t.Errorf("did not run %q; ran %q", want, d.ran)
		}
	}
	for _, ran := range d.ran {
		if strings.Contains(ran, "hms_data") || strings.Contains(ran, "c2") {
			t.Errorf("touched a non-OAF object: %q", ran)
		}
	}
	if slices.Contains(d.ran, "systemctl disable --now oaf-keys.service") {
		t.Error("disabled a unit that was not installed")
	}
}

func TestResetKeeps(t *testing.T) {
	root := t.TempDir()
	at := func(p string) string { return filepath.Join(root, p) }
	write(t, at("/var/lib/oaf/models/m/w.safetensors"), "x")
	write(t, at("/var/lib/oaf/projects/gateway/compose.yaml"), "x")

	d := &resetDocker{}
	r := resetter{Run: d, Root: root, Out: &strings.Builder{}, KeepWeights: true, KeepImages: true}
	if err := r.reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(at("/var/lib/oaf/models/m/w.safetensors")) {
		t.Error("weights removed with --keep-weights")
	}
	if exists(at("/var/lib/oaf/projects")) {
		t.Error("projects kept with --keep-weights")
	}
	for _, ran := range d.ran {
		if strings.HasPrefix(ran, "docker image") {
			t.Errorf("ran %q with --keep-images", ran)
		}
	}
}
