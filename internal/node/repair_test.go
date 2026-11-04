package node

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/hostexec"
)

func TestParseDisconnected(t *testing.T) {
	out := strings.Join([]string{
		"oaf-gateway agentgateway oaf-gateway_default 0",
		"oaf-gateway agentgateway oaf-gateway_default 0", // a second replica
		"oaf-langfuse postgres oaf-langfuse_default 1",   // fine
		"oaf-model-ornith vllm host 0",                   // host network: never has one
		"vclocal frontend vclocal_default 0",             // not ours
		"",
	}, "\n")
	got := parseDisconnected(out)
	if len(got) != 1 || !slices.Equal(got["gateway"], []string{"agentgateway"}) {
		t.Fatalf("got %v", got)
	}
}

// inspectDocker answers `ps` and `inspect` the way docker does after a power
// cut left one gateway container without its network.
type inspectDocker struct{ calls []string }

func (d *inspectDocker) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := hostexec.CmdLine(name, args...)
	d.calls = append(d.calls, line)
	switch {
	case strings.HasPrefix(line, "docker ps"):
		return []byte("aaa\nbbb\nccc\n"), nil
	case strings.HasPrefix(line, "docker inspect"):
		return []byte("oaf-gateway agentgateway oaf-gateway_default 0\noaf-metrics grafana oaf-metrics_default 1\noaf-gone web oaf-gone_default 0\n"), nil
	}
	return nil, nil
}

func TestRepairNetworksRecreatesOnlyLostContainersOfKnownProjects(t *testing.T) {
	state := t.TempDir()
	for _, p := range []string{"gateway", "metrics"} {
		dir := filepath.Join(state, "projects", p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ComposeFile), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d := &inspectDocker{}
	s := &Server{StateDir: state, Compose: Compose{Runner: d}}
	done, err := s.RepairNetworks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(done, []string{"gateway/agentgateway"}) {
		t.Fatalf("recreated %v", done)
	}
	var ups []string
	for _, c := range d.calls {
		if strings.Contains(c, " up ") {
			ups = append(ups, c)
		}
	}
	if len(ups) != 1 || !strings.Contains(ups[0], "--project-name oaf-gateway") ||
		!strings.Contains(ups[0], "--force-recreate") || !strings.HasSuffix(ups[0], " agentgateway") {
		t.Fatalf("compose calls %q", ups)
	}
}
