package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
)

// fakeHub serves a repo at a revision the way Hugging Face does: the file
// list with LFS hashes, and files with byte ranges.
type fakeHub struct {
	files    map[string][]byte
	lfs      map[string]bool
	gets     atomic.Int32
	failOnce atomic.Bool // the first file request breaks off halfway
}

func (h *fakeHub) server(_ *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/org/model/revision/abc123"):
			type lfs struct {
				SHA256 string `json:"sha256"`
				Size   int    `json:"size"`
			}
			var sib []map[string]any
			for name, b := range h.files {
				s := map[string]any{"rfilename": name, "size": len(b)}
				if h.lfs[name] {
					sum := sha256.Sum256(b)
					s["lfs"] = lfs{SHA256: hex.EncodeToString(sum[:]), Size: len(b)}
				}
				sib = append(sib, s)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"siblings": sib})
		case strings.HasPrefix(r.URL.Path, "/org/model/resolve/abc123/"):
			h.gets.Add(1)
			b, ok := h.files[strings.TrimPrefix(r.URL.Path, "/org/model/resolve/abc123/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			from := 0
			if rg := r.Header.Get("Range"); rg != "" {
				from, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(b)-1, len(b)))
				w.WriteHeader(http.StatusPartialContent)
			}
			body := b[from:]
			if h.lfs[strings.TrimPrefix(r.URL.Path, "/org/model/resolve/abc123/")] && h.failOnce.CompareAndSwap(true, false) {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				_, _ = w.Write(body[:len(body)/2])
				if hj, ok := w.(http.Hijacker); ok {
					c, _, _ := hj.Hijack()
					_ = c.Close()
				}
				return
			}
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
}

func waitState(t *testing.T, w *Weights, dir, want string) *nodev1.WeightsStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, st := range w.All() {
			if st.GetDir() == dir && st.GetState() == want {
				return st
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never became %s: %v", dir, want, w.All())
	return nil
}

func TestWeightsDownload(t *testing.T) {
	shard := []byte(strings.Repeat("weights!", 4096))
	hub := &fakeHub{files: map[string][]byte{
		"config.json":                      []byte(`{"model_type":"x"}`),
		"model-00001-of-00001.safetensors": shard,
		"model.safetensors.index.json":     []byte(`{"weight_map":{"a":"model-00001-of-00001.safetensors"}}`),
		"original/consolidated.pth":        []byte("skip me"),
	}, lfs: map[string]bool{"model-00001-of-00001.safetensors": true}}
	hub.failOnce.Store(true)
	srv := hub.server(t)
	defer srv.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "models", "m-1")
	w := &Weights{StateDir: root, BaseURL: srv.URL, RetryDelay: 10 * time.Millisecond}
	spec := &nodev1.WeightsSpec{ModelId: "m-1", Dir: dir, Repo: "org/model", Revision: "abc123", Include: []string{"*.json", "*.safetensors"}}

	if _, err := w.Start(&nodev1.WeightsSpec{ModelId: "m-1", Dir: "/etc/m-2", Repo: "org/model", Revision: "abc123"}); err == nil {
		t.Fatal("a directory not named after the model was accepted")
	}
	started := make(chan struct{})
	w.WhenComplete(dir, "model-m", func() { close(started) })
	if _, err := w.Start(spec); err != nil {
		t.Fatal(err)
	}
	// The first try of the shard breaks off; a retry resumes it.
	st := waitState(t, w, dir, WeightsComplete)
	if st.GetBytesDone() != st.GetBytesTotal() || st.GetBytesTotal() == 0 {
		t.Fatalf("progress: %v", st)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting project was not started")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "model-00001-of-00001.safetensors"))
	if string(got) != string(shard) {
		t.Fatal("shard content differs")
	}
	if _, err := os.Stat(filepath.Join(dir, "original")); err == nil {
		t.Fatal("a file outside the include patterns was downloaded")
	}
	if !Complete(dir) {
		t.Fatal("not complete after download")
	}
	// Complete weights are not fetched again.
	gets := hub.gets.Load()
	if st, err := w.Start(spec); err != nil || st.GetState() != WeightsComplete || hub.gets.Load() != gets {
		t.Fatalf("restart of complete weights: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(root, "weights", "m-1.json")); err == nil {
		t.Fatal("a finished download is still remembered for resume")
	}
}

// Weights put there by hand count as complete; half-written ones do not.
func TestWeightsCompleteByHand(t *testing.T) {
	dir := t.TempDir()
	if Complete(dir) {
		t.Fatal("empty directory complete")
	}
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "model.safetensors.index.json"), []byte(`{"weight_map":{"a":"m-1.safetensors","b":"m-2.safetensors"}}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "m-1.safetensors"), []byte("x"), 0o644)
	if Complete(dir) {
		t.Fatal("a missing shard counted as complete")
	}
	_ = os.WriteFile(filepath.Join(dir, "m-2.safetensors"), []byte("x"), 0o644)
	if !Complete(dir) {
		t.Fatal("complete weights by hand not recognised")
	}
	_ = os.WriteFile(filepath.Join(dir, "m-3.safetensors.part"), []byte("x"), 0o644)
	if Complete(dir) {
		t.Fatal("a partial file counted as complete")
	}
}

// Not enough disk is a permanent failure with the sizes in the message.
func TestWeightsDiskCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"siblings": []map[string]any{{"rfilename": "huge.safetensors", "size": int64(1) << 60}}})
	}))
	defer srv.Close()
	root := t.TempDir()
	dir := filepath.Join(root, "m-2")
	w := &Weights{StateDir: root, BaseURL: srv.URL, RetryDelay: 10 * time.Millisecond}
	if _, err := w.Start(&nodev1.WeightsSpec{ModelId: "m-2", Dir: dir, Repo: "org/model", Revision: "abc123"}); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, w, dir, WeightsFailed)
	if !strings.Contains(st.GetError(), "not enough disk space") {
		t.Fatalf("error: %q", st.GetError())
	}
}
