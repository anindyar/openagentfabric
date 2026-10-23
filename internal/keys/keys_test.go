package keys

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/secrets"
)

func newService(t *testing.T) (*Service, *int) {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	applies := 0
	return &Service{
		ClusterPath:  path,
		Secrets:      secrets.Store{Dir: filepath.Join(dir, "secrets")},
		ApplyGateway: func(context.Context) error { applies++; return nil },
	}, &applies
}

func TestIssueIsIdempotent(t *testing.T) {
	s, applies := newService(t)
	c1, err := s.Issue(context.Background(), "vclocal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c1.Name != "vclocal" || len(c1.Key) != 64 || *applies != 1 {
		t.Fatalf("issue = %+v, applies = %d", c1, *applies)
	}
	cl, err := config.LoadCluster(s.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if secretRefFor(cl, "vclocal") != "gateway-key-vclocal" {
		t.Fatalf("cluster.yaml keys = %+v", cl.Spec.Routing.Keys)
	}
	fi, _ := os.Stat(s.ClusterPath)
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("cluster.yaml mode %o, want the original 640", fi.Mode().Perm())
	}

	c2, err := s.Issue(context.Background(), "vclocal", nil)
	if err != nil || c2 != c1 {
		t.Fatalf("re-issue = %+v, %v; want the same key", c2, err)
	}
}

func TestRotate(t *testing.T) {
	s, _ := newService(t)
	issued, err := s.Issue(context.Background(), "vclocal", nil)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := s.Rotate(context.Background(), "vclocal")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Key == issued.Key {
		t.Fatal("rotation kept the old key")
	}
	if _, err := s.Owner(issued.Key); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("old key still recognised: %v", err)
	}
	if name, err := s.Owner(rotated.Key); err != nil || name != "vclocal" {
		t.Fatalf("new key owner = %q, %v", name, err)
	}
	if _, err := s.Rotate(context.Background(), "nope"); err == nil {
		t.Fatal("rotating an unknown key name succeeded")
	}
}

func TestRotateTooSoon(t *testing.T) {
	s, _ := newService(t)
	now := time.Unix(1_800_000_000, 0)
	s.Now = func() time.Time { return now }
	if _, err := s.Issue(context.Background(), "vclocal", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rotate(context.Background(), "vclocal"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rotate(context.Background(), "vclocal"); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("second rotation at once = %v, want ErrTooSoon", err)
	}
	now = now.Add(MinRotateInterval)
	if _, err := s.Rotate(context.Background(), "vclocal"); err != nil {
		t.Fatalf("rotation after the interval = %v", err)
	}
}

// A rotation whose apply fails must leave the old key in place: the app is
// told it failed and keeps using a key the gateway still accepts.
func TestRotateRestoresOnApplyFailure(t *testing.T) {
	s, _ := newService(t)
	issued, err := s.Issue(context.Background(), "vclocal", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.ApplyGateway = func(context.Context) error { return errors.New("agent unreachable") }
	if _, err := s.Rotate(context.Background(), "vclocal"); err == nil || !strings.Contains(err.Error(), "old key still works") {
		t.Fatalf("rotate error = %v", err)
	}
	if name, err := s.Owner(issued.Key); err != nil || name != "vclocal" {
		t.Fatalf("old key after failed rotation: owner %q, %v", name, err)
	}
}

func TestOwnerNeverGenerates(t *testing.T) {
	s, _ := newService(t)
	if _, err := s.Owner("anything"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("owner of a random key = %v", err)
	}
	if entries, _ := os.ReadDir(s.Secrets.Dir); len(entries) != 0 {
		t.Fatalf("Owner created %d secret(s)", len(entries))
	}
}

func TestHandlerRotate(t *testing.T) {
	s, _ := newService(t)
	issued, err := s.Issue(context.Background(), "vclocal", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler(slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()

	post := func(key string) (*http.Response, map[string]string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/keys/rotate", strings.NewReader("{}"))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp, body
	}

	if resp, _ := post(""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	if resp, _ := post("not-a-key"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", resp.StatusCode)
	}
	resp, body := post(issued.Key)
	if resp.StatusCode != http.StatusOK || body["name"] != "vclocal" || body["key"] == "" || body["key"] == issued.Key {
		t.Fatalf("rotate: %d %v", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("key response is cacheable")
	}
	if resp, _ := post(issued.Key); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old key after rotation: %d", resp.StatusCode)
	}
	if resp, _ := post(body["key"]); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("immediate second rotation: %d", resp.StatusCode)
	}
}
