package cli

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/join"
	"openagentfabric.dev/oaf/internal/pki"
)

// recordingRunner answers inspections from a replay and records the rest.
type recordingRunner struct {
	replay hostexec.Replay
	mu     sync.Mutex
	ran    []string
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemctl" {
		r.mu.Lock()
		r.ran = append(r.ran, hostexec.CmdLine(name, args...))
		r.mu.Unlock()
		return nil, nil
	}
	return r.replay.Run(ctx, name, args...)
}

// `oaf join` against a real control-node service: preflight reported,
// enrolled once, certificate and unit written, agent started.
func TestJoinCommand(t *testing.T) {
	dir := t.TempDir()
	ca, _ := pki.NewCA("test")
	s := &join.Service{
		Tokens:  &join.TokenStore{Path: filepath.Join(dir, "tokens.json")},
		Pending: &join.PendingStore{Path: filepath.Join(dir, "pending.json")},
		CA:      ca,
	}
	mux := http.NewServeMux()
	for p, h := range s.PublicRoutes() {
		mux.Handle(p, h)
	}
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	pin, _ := join.PinFromCert(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	token, _, err := s.Tokens.Create("", 0, 1)
	if err != nil {
		t.Fatal(err)
	}

	rr := &recordingRunner{replay: hostexec.Replay{
		"cat /etc/hostname": {Stdout: "gx10-new\n"},
		"cat /proc/meminfo": {Stdout: "MemTotal:       127504504 kB\nMemAvailable:   120000000 kB\n"},
	}}
	old := joinRunner
	joinRunner = rr
	defer func() { joinRunner = old }()

	pkiDir, unitDir := filepath.Join(dir, "pki"), filepath.Join(dir, "units")
	args := []string{"join", "--control", srv.URL, "--token", token, "--pin", pin, "--address", "192.0.2.21",
		"--pki-dir", pkiDir, "--unit-dir", unitDir, "--skip-images"}
	out, err := runCLI(t, args...)
	if err == nil || !strings.Contains(err.Error(), "blocking check") {
		t.Fatalf("a box that fails preflight must not join without --force: %v\n%s", err, out)
	}
	out, err = runCLI(t, append(args, "--force")...)
	if err != nil {
		t.Fatalf("join: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Joined as gx10-new (192.0.2.21)") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := pki.Load(pkiDir, "node"); err != nil {
		t.Fatalf("node certificate: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(pkiDir, pki.CACertFile)); string(b) != string(ca.CertPEM) {
		t.Fatal("CA not written")
	}
	if b, _ := os.ReadFile(filepath.Join(unitDir, "oaf-node.service")); string(b) != join.NodeUnit {
		t.Fatal("unit not written")
	}
	if strings.Join(rr.ran, "; ") != "systemctl daemon-reload; systemctl enable oaf-node.service; systemctl restart oaf-node.service" {
		t.Fatalf("systemctl calls: %v", rr.ran)
	}
	pend, _ := s.Pending.List()
	if len(pend) != 1 || pend[0].Name != "gx10-new" || pend[0].Hardware.MemTotalGiB != 121.6 {
		t.Fatalf("pending: %+v", pend)
	}
	if _, err := runCLI(t, append(args, "--force")...); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("the token must work once: %v", err)
	}
}
