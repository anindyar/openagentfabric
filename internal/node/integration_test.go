//go:build integration

package node

import (
	"context"
	"testing"
	"time"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/hostexec"
)

// TestRealCompose drives real docker compose (run in CI: make integration).
func TestRealCompose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s := &Server{StateDir: t.TempDir(), Compose: Compose{Runner: hostexec.Local{}}}
	req := &nodev1.ApplyProjectRequest{Name: "itest", Files: []*nodev1.File{{
		Path:    "compose.yaml",
		Content: []byte("services:\n  sleeper:\n    image: busybox:1.37\n    command: [sleep, \"300\"]\n"),
	}}}
	t.Cleanup(func() { _, _ = s.RemoveProject(context.Background(), &nodev1.RemoveProjectRequest{Name: "itest"}) })

	first, err := s.ApplyProject(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !first.GetChanged() || len(first.GetServices()) != 1 || first.GetServices()[0].GetState() != "running" {
		t.Fatalf("first apply: %v", first)
	}
	second, err := s.ApplyProject(ctx, req)
	if err != nil || second.GetChanged() {
		t.Fatalf("second apply should be a no-op: %v %v", second, err)
	}
	st, err := s.ProjectStatus(ctx, &nodev1.ProjectStatusRequest{Name: "itest"})
	if err != nil || !st.GetExists() || st.GetServices()[0].GetName() != "sleeper" {
		t.Fatalf("status: %v %v", st, err)
	}
	rm, err := s.RemoveProject(ctx, &nodev1.RemoveProjectRequest{Name: "itest"})
	if err != nil || !rm.GetExisted() {
		t.Fatalf("remove: %v %v", rm, err)
	}
	after, err := s.ProjectStatus(ctx, &nodev1.ProjectStatusRequest{Name: "itest"})
	if err != nil || after.GetExists() || len(after.GetServices()) != 0 {
		t.Fatalf("after remove: %v %v", after, err)
	}
}
