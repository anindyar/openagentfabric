package cli

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/oafd"
	"openagentfabric.dev/oaf/internal/pki"
	"openagentfabric.dev/oaf/internal/secrets"
)

// oafd serves join.sh without a management token, and the admin side of
// growing the cluster (join tokens, topology) only with one.
func TestServeMountsGrowth(t *testing.T) {
	dir := t.TempDir()
	cluster := filepath.Join(dir, "cluster.yaml")
	raw, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cluster, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &stackFlags{cluster: cluster, catalog: "../../examples/catalog.yaml",
		secretsDir: filepath.Join(dir, "secrets"), pkiDir: filepath.Join(dir, "pki")}
	ca, err := pki.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.Save(f.pkiDir, "ca", ca); err != nil {
		t.Fatal(err)
	}
	store := secrets.Store{Dir: f.secretsDir}
	if err := store.Set("oaf-admin-test", "tok"); err != nil {
		t.Fatal(err)
	}
	c, _ := config.LoadCluster(cluster)
	srv := oafd.New(oafd.Config{ClusterPath: cluster, CatalogPath: f.catalog, Secrets: store, StateDir: filepath.Join(dir, "state")})
	if err := mountGrowth(srv, f, c, filepath.Join(dir, "state"), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/join.sh")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "oaf") {
		t.Fatalf("join.sh: %d %s", resp.StatusCode, body)
	}

	post := func(path, token, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := post("/api/v1/nodes/join-tokens", "", `{"count":1}`); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("join token without a management token: %d", r.StatusCode)
	}
	r := post("/api/v1/nodes/join-tokens", "tok", `{"count":1}`)
	var tok map[string]any
	_ = json.NewDecoder(r.Body).Decode(&tok)
	r.Body.Close()
	if r.StatusCode/100 != 2 || !strings.Contains(tok["command"].(string), "join.sh") {
		t.Fatalf("join token: %d %v", r.StatusCode, tok)
	}
	r = post("/api/v1/topology/recommend", "tok", `{"new_nodes":2,"cabling":"cx7-pair"}`)
	var rec map[string]any
	_ = json.NewDecoder(r.Body).Decode(&rec)
	r.Body.Close()
	if r.StatusCode != 200 || rec["options"] == nil {
		t.Fatalf("recommend: %d %v", r.StatusCode, rec)
	}
}
