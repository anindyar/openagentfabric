package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
)

// Weight download states.
const (
	WeightsDownloading = "downloading"
	WeightsComplete    = "complete"
	WeightsFailed      = "failed"
)

// CompleteMarker is written into a model directory once every file is
// downloaded and verified.
const CompleteMarker = ".oaf-complete"

// HuggingFaceURL is where weights come from.
const HuggingFaceURL = "https://huggingface.co"

var modelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,127}$`)

// Weights downloads model weights from Hugging Face into model directories:
// pinned revisions, resumable, every LFS file checked against its SHA-256,
// one download per directory at a time. Downloads in progress survive an
// agent restart (Resume).
type Weights struct {
	// StateDir keeps the downloads to resume (weights/<model>.json).
	StateDir string
	// BaseURL overrides HuggingFaceURL (tests).
	BaseURL string
	Client  *http.Client
	// RetryDelay is the first pause before retrying a failed download
	// (default 10s; it grows with each attempt).
	RetryDelay time.Duration
	// Parallel files per download (default 3).
	Parallel int

	mu      sync.Mutex
	pulls   map[string]*pull             // by dir
	waiters map[string]map[string]func() // by dir, then key
}

type pull struct {
	spec    *nodev1.WeightsSpec
	state   string
	err     string
	started time.Time
	done    atomic.Int64
	total   atomic.Int64
	rate    atomic.Uint64 // bytes per second (float64 bits)
}

func (w *Weights) init() {
	if w.pulls == nil {
		w.pulls = map[string]*pull{}
		w.waiters = map[string]map[string]func(){}
	}
}

// validSpec checks a request before anything touches the disk.
func validSpec(s *nodev1.WeightsSpec) error {
	switch {
	case s == nil:
		return errors.New("no weights given")
	case !modelIDPattern.MatchString(s.GetModelId()):
		return fmt.Errorf("invalid model id %q", s.GetModelId())
	case !filepath.IsAbs(s.GetDir()) || filepath.Clean(s.GetDir()) != s.GetDir() || filepath.Base(s.GetDir()) != s.GetModelId():
		return fmt.Errorf("weights directory %q must be absolute and end in the model id", s.GetDir())
	case s.GetRepo() == "" || strings.Contains(s.GetRepo(), "..") || strings.Count(s.GetRepo(), "/") != 1:
		return fmt.Errorf("invalid Hugging Face repo %q", s.GetRepo())
	case s.GetRevision() == "":
		return errors.New("a pinned revision is required")
	}
	return nil
}

// Start begins (or continues) downloading spec's weights and returns their
// state. Complete weights are left alone.
func (w *Weights) Start(spec *nodev1.WeightsSpec) (*nodev1.WeightsStatus, error) {
	if err := validSpec(spec); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.init()
	if p := w.pulls[spec.GetDir()]; p != nil && p.state == WeightsDownloading {
		return p.status(), nil
	}
	if Complete(spec.GetDir()) {
		p := &pull{spec: spec, state: WeightsComplete, started: time.Now()}
		w.pulls[spec.GetDir()] = p
		return p.status(), nil
	}
	p := &pull{spec: spec, state: WeightsDownloading, started: time.Now()}
	w.pulls[spec.GetDir()] = p
	w.remember(spec)
	go w.run(p)
	return p.status(), nil
}

// All lists the downloads this agent knows of.
func (w *Weights) All() []*nodev1.WeightsStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*nodev1.WeightsStatus, 0, len(w.pulls))
	for _, p := range w.pulls {
		out = append(out, p.status())
	}
	return out
}

// WhenComplete runs fn once dir's weights are complete (at once if they
// are); key replaces an earlier waiter with the same key.
func (w *Weights) WhenComplete(dir, key string, fn func()) {
	if Complete(dir) {
		go fn()
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.init()
	if w.waiters[dir] == nil {
		w.waiters[dir] = map[string]func(){}
	}
	w.waiters[dir][key] = fn
}

// Forget drops a waiter (a project removed before its weights arrived).
func (w *Weights) Forget(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ws := range w.waiters {
		delete(ws, key)
	}
}

// Resume restarts the downloads an earlier agent left unfinished.
func (w *Weights) Resume() {
	entries, _ := os.ReadDir(filepath.Join(w.StateDir, "weights"))
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(w.StateDir, "weights", e.Name()))
		if err != nil {
			continue
		}
		var spec nodev1.WeightsSpec
		if json.Unmarshal(data, &spec) == nil {
			_, _ = w.Start(&spec)
		}
	}
}

func (w *Weights) remember(spec *nodev1.WeightsSpec) {
	dir := filepath.Join(w.StateDir, "weights")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	data, _ := json.Marshal(spec)
	_ = os.WriteFile(filepath.Join(dir, spec.GetModelId()+".json"), data, 0o644)
}

func (w *Weights) forgetSpec(spec *nodev1.WeightsSpec) {
	_ = os.Remove(filepath.Join(w.StateDir, "weights", spec.GetModelId()+".json"))
}

func (p *pull) status() *nodev1.WeightsStatus {
	return &nodev1.WeightsStatus{ModelId: p.spec.GetModelId(), Dir: p.spec.GetDir(), State: p.state, Error: p.err,
		BytesDone: p.done.Load(), BytesTotal: p.total.Load(), BytesPerSecond: floatFrom(p.rate.Load()),
		StartedAt: p.started.UTC().Format(time.RFC3339)}
}

// run downloads with a few retries for transient failures.
func (w *Weights) run(p *pull) {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			d := w.RetryDelay
			if d == 0 {
				d = 10 * time.Second
			}
			time.Sleep(time.Duration(attempt*attempt) * d)
		}
		if err = w.download(p); err == nil || errors.Is(err, errPermanent) {
			break
		}
	}
	w.mu.Lock()
	var fns []func()
	if err != nil {
		p.state, p.err = WeightsFailed, err.Error()
	} else {
		p.state = WeightsComplete
		p.done.Store(p.total.Load())
		w.forgetSpec(p.spec)
		for _, fn := range w.waiters[p.spec.GetDir()] {
			fns = append(fns, fn)
		}
		delete(w.waiters, p.spec.GetDir())
	}
	w.mu.Unlock()
	for _, fn := range fns {
		go fn()
	}
}

var errPermanent = errors.New("permanent")

type permanent struct{ msg string }

func (e permanent) Error() string { return e.msg }
func (e permanent) Unwrap() error { return errPermanent }

// hfFile is one file of a repo at a revision.
type hfFile struct {
	Name   string `json:"rfilename"`
	Size   int64  `json:"size"`
	SHA256 string // LFS files only
}

func (w *Weights) base() string {
	if w.BaseURL != "" {
		return strings.TrimRight(w.BaseURL, "/")
	}
	return HuggingFaceURL
}

func (w *Weights) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return &http.Client{Timeout: 0}
}

// list returns the files of the revision that match the include patterns.
func (w *Weights) list(ctx context.Context, spec *nodev1.WeightsSpec) ([]hfFile, error) {
	u := fmt.Sprintf("%s/api/models/%s/revision/%s?blobs=true", w.base(), spec.GetRepo(), url.PathEscape(spec.GetRevision()))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := w.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
		return nil, permanent{fmt.Sprintf("Hugging Face has no accessible %s at %s (HTTP %d; gated models need an access token)", spec.GetRepo(), spec.GetRevision(), resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("listing %s: HTTP %d", spec.GetRepo(), resp.StatusCode)
	}
	var body struct {
		Siblings []struct {
			Name string `json:"rfilename"`
			Size int64  `json:"size"`
			LFS  *struct {
				SHA256 string `json:"sha256"`
				Size   int64  `json:"size"`
			} `json:"lfs"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("listing %s: %w", spec.GetRepo(), err)
	}
	var out []hfFile
	for _, s := range body.Siblings {
		if !included(s.Name, spec.GetInclude()) || !safeRel(s.Name) {
			continue
		}
		f := hfFile{Name: s.Name, Size: s.Size}
		if s.LFS != nil {
			f.SHA256, f.Size = s.LFS.SHA256, s.LFS.Size
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, permanent{fmt.Sprintf("%s at %s has no files matching %v", spec.GetRepo(), spec.GetRevision(), spec.GetInclude())}
	}
	return out, nil
}

func included(name string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(name)); ok && !strings.Contains(p, "/") && !strings.Contains(name, "/") {
			return true
		}
	}
	return false
}

// safeRel refuses names that would leave the model directory.
func safeRel(name string) bool {
	return name != "" && !strings.HasPrefix(name, "/") && !strings.Contains(name, "..") && path.Clean(name) == name
}

func (w *Weights) download(p *pull) error {
	ctx := context.Background()
	spec := p.spec
	files, err := w.list(ctx, spec)
	if err != nil {
		return err
	}
	dir := spec.GetDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return permanent{err.Error()}
	}
	var total, have int64
	for _, f := range files {
		total += f.Size
		if st, err := os.Stat(filepath.Join(dir, f.Name)); err == nil && st.Size() == f.Size {
			have += f.Size
		}
	}
	p.total.Store(total)
	p.done.Store(have)
	if free, err := freeBytes(dir); err == nil && free < (total-have)+(total-have)/20 {
		return permanent{fmt.Sprintf("not enough disk space in %s: %s needed, %s free", dir, gb(total-have), gb(free))}
	}

	stop := make(chan struct{})
	go p.measure(stop)
	defer close(stop)

	n := w.Parallel
	if n <= 0 {
		n = 3
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, f := range files {
		target := filepath.Join(dir, f.Name)
		if st, err := os.Stat(target); err == nil && st.Size() == f.Size {
			continue // present (verified when it was written, or put there by hand)
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(f hfFile) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := w.fetch(ctx, p, f, target); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(f)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	marker, _ := json.Marshal(map[string]any{"repo": spec.GetRepo(), "revision": spec.GetRevision(), "files": len(files), "bytes": total,
		"completed_at": time.Now().UTC().Format(time.RFC3339)})
	return os.WriteFile(filepath.Join(dir, CompleteMarker), marker, 0o644)
}

// fetch downloads one file to target, resuming target.part, and checks its
// SHA-256 when Hugging Face publishes one.
func (w *Weights) fetch(ctx context.Context, p *pull, f hfFile, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	part := target + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	var h hash.Hash
	if f.SHA256 != "" {
		h = sha256.New()
	}
	offset, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if offset > f.Size {
		if err := out.Truncate(0); err != nil {
			return err
		}
		offset, _ = out.Seek(0, io.SeekStart)
	}
	if offset > 0 && h != nil {
		if _, err := out.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(h, out, offset); err != nil {
			return err
		}
	}
	p.done.Add(offset)
	if offset < f.Size {
		u := fmt.Sprintf("%s/%s/resolve/%s/%s", w.base(), p.spec.GetRepo(), url.PathEscape(p.spec.GetRevision()), escapePath(f.Name))
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		resp, err := w.client().Do(req)
		if err != nil {
			p.done.Add(-offset)
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		defer resp.Body.Close()
		switch {
		case offset > 0 && resp.StatusCode == http.StatusOK:
			// The server ignored the range: start over.
			p.done.Add(-offset)
			if err := out.Truncate(0); err != nil {
				return err
			}
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if h != nil {
				h.Reset()
			}
		case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent:
			p.done.Add(-offset)
			return fmt.Errorf("%s: HTTP %d", f.Name, resp.StatusCode)
		}
		dst := io.Writer(out)
		if h != nil {
			dst = io.MultiWriter(out, h)
		}
		if _, err := io.Copy(dst, &counting{r: resp.Body, n: &p.done}); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	if st, err := out.Stat(); err != nil || st.Size() != f.Size {
		return fmt.Errorf("%s: size mismatch", f.Name)
	}
	if h != nil && hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		_ = os.Remove(part)
		return fmt.Errorf("%s: SHA-256 mismatch, discarded", f.Name)
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return os.Rename(part, target)
}

// measure keeps a moving average of the download rate.
func (p *pull) measure(stop <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	last, rate := p.done.Load(), 0.0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			now := p.done.Load()
			r := float64(now-last) / 2
			last = now
			if rate == 0 {
				rate = r
			} else {
				rate = 0.7*rate + 0.3*r
			}
			p.rate.Store(floatBits(rate))
		}
	}
}

type counting struct {
	r io.Reader
	n *atomic.Int64
}

func (c *counting) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func escapePath(name string) string {
	parts := strings.Split(name, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// Complete reports whether dir holds a model's weights: downloaded by the
// agent (the marker), or put there by hand (a config and every shard the
// index names, nothing half-written).
func Complete(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, CompleteMarker)); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		return false
	}
	partial := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".part") || strings.HasSuffix(p, ".incomplete")) && !strings.Contains(p, "/.cache/") {
			partial = true
		}
		return nil
	})
	if partial {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, "model.safetensors.index.json"))
	if err != nil {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.safetensors"))
		return len(matches) > 0
	}
	var idx struct {
		WeightMap map[string]string `json:"weight_map"`
	}
	if json.Unmarshal(data, &idx) != nil || len(idx.WeightMap) == 0 {
		return false
	}
	for _, f := range idx.WeightMap {
		if !safeRel(f) {
			return false
		}
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func gb(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

func floatBits(f float64) uint64 { return math.Float64bits(f) }
func floatFrom(b uint64) float64 { return math.Float64frombits(b) }
