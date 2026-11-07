package node

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/hostexec"
)

// upDocker records `compose up` calls.
type upDocker struct {
	mu    sync.Mutex
	ups   []string
	fails int // the first fails ups fail (e.g. the engine image pull times out)
}

func (d *upDocker) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := hostexec.CmdLine(name, args...)
	if strings.Contains(line, " up ") {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.fails > 0 {
			d.fails--
			return nil, errors.New("pulling vllm: context deadline exceeded")
		}
		d.ups = append(d.ups, line)
	}
	return nil, nil
}

func (d *upDocker) waitUp(t *testing.T, project string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		for _, u := range d.ups {
			if strings.Contains(u, ProjectPrefix+project+" ") {
				d.mu.Unlock()
				return
			}
		}
		d.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was never started (ups: %v)", project, d.ups)
}

// gatedHub is a fakeHub whose file downloads wait until release is closed.
func gatedHub(t *testing.T, release chan struct{}) *httptest.Server {
	hub := &fakeHub{files: map[string][]byte{"config.json": []byte(`{}`), "model.safetensors": []byte(strings.Repeat("w", 4096))}}
	inner := hub.server(t)
	t.Cleanup(inner.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/resolve/") {
			<-release
		}
		resp, err := http.Get(inner.URL + r.URL.String())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 1<<16)
		for {
			n, err := resp.Body.Read(buf)
			_, _ = w.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func modelRequest(dir string) *nodev1.ApplyProjectRequest {
	return &nodev1.ApplyProjectRequest{Name: "model-m", Files: []*nodev1.File{{Path: ComposeFile, Mode: 0o644, Content: []byte("services: {}\n")}},
		Weights: &nodev1.WeightsSpec{ModelId: "m", Dir: dir, Repo: "org/model", Revision: "abc123"}}
}

// A model applied while its weights download starts when they complete.
func TestModelStartsWhenWeightsComplete(t *testing.T) {
	release := make(chan struct{})
	hub := gatedHub(t, release)
	state := t.TempDir()
	dir := filepath.Join(state, "models", "m")
	d := &upDocker{}
	s := &Server{StateDir: state, Compose: Compose{Runner: d}, Downloads: &Weights{StateDir: state, BaseURL: hub.URL, RetryDelay: time.Millisecond}}
	resp, err := s.ApplyProject(context.Background(), modelRequest(dir))
	if err != nil || !resp.GetWaitingForWeights() {
		t.Fatalf("apply: %v %v", resp, err)
	}
	close(release)
	d.waitUp(t, "model-m")
}

// The agent restarts (a binary swap) while the download is in flight; the
// new agent resumes the download and starts the model when it completes.
func TestModelStartsAfterAgentRestartMidDownload(t *testing.T) {
	release := make(chan struct{})
	hub := gatedHub(t, release)
	state := t.TempDir()
	dir := filepath.Join(state, "models", "m")
	first := &Server{StateDir: state, Compose: Compose{Runner: &upDocker{}}, Downloads: &Weights{StateDir: state, BaseURL: hub.URL, RetryDelay: time.Millisecond}}
	if _, err := first.ApplyProject(context.Background(), modelRequest(dir)); err != nil {
		t.Fatal(err)
	}
	waitState(t, first.Downloads, dir, WeightsDownloading)
	// What the killed agent leaves: a half-written file. (Its goroutines die with it;
	// here the old download stays blocked on the hub and is never released.)
	_ = os.WriteFile(filepath.Join(dir, "model.safetensors.part"), []byte("ww"), 0o644)

	d := &upDocker{}
	release2 := make(chan struct{})
	second := &Server{StateDir: state, Compose: Compose{Runner: d}, Downloads: &Weights{StateDir: state, BaseURL: gatedHub(t, release2).URL, RetryDelay: time.Millisecond}}
	second.ResumeWeights()
	close(release2)
	d.waitUp(t, "model-m")
	close(release)
}

// The download finished while no agent was waiting for it (the agent
// restarted after the last file but before starting the model).
func TestModelStartsAfterRestartWithCompleteWeights(t *testing.T) {
	release := make(chan struct{})
	close(release)
	hub := gatedHub(t, release)
	state := t.TempDir()
	dir := filepath.Join(state, "models", "m")
	first := &Server{StateDir: state, Compose: Compose{Runner: &upDocker{}}, Downloads: &Weights{StateDir: state, BaseURL: hub.URL, RetryDelay: time.Millisecond}}
	first.Downloads.WhenComplete(dir, "block", func() {}) // nothing else
	if _, err := first.ApplyProject(context.Background(), modelRequest(dir)); err != nil {
		t.Fatal(err)
	}
	waitState(t, first.Downloads, dir, WeightsComplete)
	// The first agent "died" before its waiter ran: put the waiting record back.
	_ = os.MkdirAll(filepath.Join(state, "waiting"), 0o755)
	_ = os.WriteFile(filepath.Join(state, "waiting", "model-m"), []byte(dir), 0o644)
	d := &upDocker{}
	second := &Server{StateDir: state, Compose: Compose{Runner: d}, Downloads: &Weights{StateDir: state, BaseURL: hub.URL}}
	second.ResumeWeights()
	d.waitUp(t, "model-m")
}

// Seen on a DGX Spark: the weights finished, the deferred start failed (the
// engine image was still being pulled when its deadline passed), and the
// model never started until the next apply. A failed start is retried, and
// the waiting record stays until the model is up, so an agent restart in
// between also starts it.
func TestModelStartRetriedAfterFailure(t *testing.T) {
	release := make(chan struct{})
	close(release)
	hub := gatedHub(t, release)
	state := t.TempDir()
	dir := filepath.Join(state, "models", "m")
	d := &upDocker{fails: 2}
	s := &Server{StateDir: state, Compose: Compose{Runner: d}, StartRetry: 10 * time.Millisecond,
		Downloads: &Weights{StateDir: state, BaseURL: hub.URL, RetryDelay: time.Millisecond}}
	if _, err := s.ApplyProject(context.Background(), modelRequest(dir)); err != nil {
		t.Fatal(err)
	}
	d.waitUp(t, "model-m")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(state, "waiting", "model-m")); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the waiting record outlived the start")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A failed start keeps the waiting record, so the next agent starts it.
func TestFailedStartSurvivesRestart(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "models", "m")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, CompleteMarker), []byte("{}"), 0o644)
	_ = os.MkdirAll(filepath.Join(state, "projects", "model-m"), 0o755)
	_ = os.WriteFile(filepath.Join(state, "projects", "model-m", ComposeFile), []byte("services: {}\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(state, "waiting"), 0o755)
	_ = os.WriteFile(filepath.Join(state, "waiting", "model-m"), []byte(dir), 0o644)

	failing := &upDocker{fails: 1 << 30}
	first := &Server{StateDir: state, Compose: Compose{Runner: failing}, StartRetry: time.Hour, Downloads: &Weights{StateDir: state}}
	first.ResumeWeights()
	time.Sleep(50 * time.Millisecond) // its first start fails; the retry is an hour away
	if _, err := os.Stat(filepath.Join(state, "waiting", "model-m")); err != nil {
		t.Fatalf("a failed start dropped the waiting record: %v", err)
	}
	d := &upDocker{}
	second := &Server{StateDir: state, Compose: Compose{Runner: d}, Downloads: &Weights{StateDir: state}}
	second.ResumeWeights()
	d.waitUp(t, "model-m")
}
