package join

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/pki"
	"openagentfabric.dev/oaf/internal/preflight"
)

func TestTokens(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	s := &TokenStore{Path: filepath.Join(t.TempDir(), "join", "tokens.json"), Now: func() time.Time { return now }}
	clear, tok, err := s.Create("rack 2", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(clear, TokenPrefix) || tok.ExpiresAt != now.Add(DefaultTokenTTL) {
		t.Fatalf("token %q %+v", clear, tok)
	}
	raw, _ := os.ReadFile(s.Path)
	if strings.Contains(string(raw), clear) {
		t.Fatal("the token is stored in clear")
	}
	if _, err := s.Check(clear); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(clear); err != nil {
		t.Fatal("Check must not use the token up")
	}
	if _, err := s.Check("oafj_nope"); !errors.Is(err, ErrTokenUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := s.Consume(clear, "spark-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(clear, "spark-3"); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("second use: %v", err)
	}
	c2, _, _ := s.Create("", 10*time.Minute, 1)
	now = now.Add(11 * time.Minute)
	if _, err := s.Check(c2); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired: %v", err)
	}
	// One token for a pair: two enrolments, then used up.
	c3, _, _ := s.Create("pair", time.Hour, 2)
	for _, n := range []string{"a", "b"} {
		if _, err := s.Consume(c3, n); err != nil {
			t.Fatalf("use %s: %v", n, err)
		}
	}
	if _, err := s.Consume(c3, "c"); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("third use of a 2-box token: %v", err)
	}
	list, _ := s.List()
	if len(list) != 3 || len(list[2].UsedBy) != 1 || list[2].UsedBy[0] != "spark-2" || list[0].Uses != 2 {
		t.Fatalf("list %+v", list)
	}
}

type testImages struct{}

func (testImages) Images(context.Context) ([]Image, error) {
	return []Image{{Name: "vllm", Ref: "example/vllm@sha256:00"}}, nil
}

func (testImages) Save(_ context.Context, name string, w io.Writer) error {
	if name != "vllm" {
		return errors.New("no such image")
	}
	_, err := w.Write([]byte("TARBALL"))
	return err
}

// The whole flow against a real TLS server: the admin makes a token, the box
// fetches join.sh and the binary, enrols once, and shows up as pending with
// a certificate from the cluster CA.
func TestServiceFlow(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	if err := os.WriteFile(filepath.Join(dir, "oaf-linux-"+other), []byte("BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Service{
		Tokens:    &TokenStore{Path: filepath.Join(dir, "tokens.json")},
		Pending:   &PendingStore{Path: filepath.Join(dir, "pending.json")},
		CA:        ca,
		BinaryDir: dir,
		Images:    testImages{},
		Taken:     func() map[string]bool { return map[string]bool{"spark-1": true} },
		Nodes: func(context.Context) []NodeView {
			return []NodeView{{Name: "spark-1", Address: "192.0.2.11", Roles: []string{"control"}, State: "joined"}}
		},
	}
	var events []Event
	s.Emit = func(e Event) { events = append(events, e) }
	mux := http.NewServeMux()
	for p, h := range s.PublicRoutes() {
		mux.Handle(p, h)
	}
	for p, h := range s.AdminRoutes() {
		mux.Handle(p, h)
	}
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	pin, err := PinFromCert(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	s.ControlURL, s.Pin = srv.URL, pin
	admin := srv.Client()

	// Admin: a token and the command.
	resp, err := admin.Post(srv.URL+"/api/v1/nodes/join-tokens", "application/json", strings.NewReader(`{"count":1,"ttl_minutes":30}`))
	if err != nil {
		t.Fatal(err)
	}
	var tr TokenResponse
	decode(t, resp, http.StatusCreated, &tr)
	if d := tr.ExpiresAt - time.Now().Unix(); d < 29*60 || d > 31*60 || tr.Count != 1 {
		t.Fatalf("token response %+v", tr)
	}
	if !strings.Contains(tr.Command, "--pinnedpubkey '"+pin+"'") || !strings.Contains(tr.Command, srv.URL+"/join.sh") || !strings.Contains(tr.Command, "export "+TokenEnv+"="+tr.Token+";") || strings.Contains(tr.Command, "--token") {
		t.Fatalf("command %q", tr.Command)
	}

	// The script is public and carries the URL and pin, never a token.
	resp, _ = admin.Get(srv.URL + "/join.sh")
	body := readAll(t, resp)
	if !strings.Contains(body, `CONTROL="`+srv.URL+`"`) || !strings.Contains(body, `PIN="`+pin+`"`) || strings.Contains(body, tr.Token) {
		t.Fatalf("join.sh:\n%s", body)
	}

	// Downloads need the token.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/join/binary?arch="+other, nil)
	if resp, _ := admin.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("binary without a token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer "+tr.Token)
	resp, _ = admin.Do(req)
	if got := readAll(t, resp); got != "BINARY" {
		t.Fatalf("binary: %q", got)
	}

	// The box's client: pinned, enrols once.
	hc, err := PinnedHTTP(pin, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Control: srv.URL, Token: tr.Token, HTTP: hc}
	imgs, err := c.Images(context.Background())
	if err != nil || len(imgs) != 1 {
		t.Fatalf("images %v %v", imgs, err)
	}
	er, err := c.Enrol(context.Background(), EnrolRequest{Address: "192.0.2.12", Hardware: Hardware{Hostname: "Spark-1", Addresses: []string{"192.0.2.12"}, GPU: "NVIDIA GB10"}})
	if err != nil {
		t.Fatal(err)
	}
	if er.Name != "spark-1-2" { // spark-1 is in cluster.yaml already
		t.Fatalf("name %q", er.Name)
	}
	block, _ := pem.Decode([]byte(er.CertPEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "spark-1-2"}); err != nil {
		t.Fatalf("node cert does not verify: %v", err)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "192.0.2.12" {
		t.Fatalf("cert IPs %v", cert.IPAddresses)
	}
	if _, err := c.Enrol(context.Background(), EnrolRequest{Hardware: Hardware{Hostname: "x"}}); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("second enrol: %v", err)
	}
	if _, err := c.Images(context.Background()); err == nil {
		t.Fatal("a used token still downloads")
	}

	resp, _ = admin.Get(srv.URL + "/api/v1/nodes/pending")
	var pend struct {
		Nodes []PendingNode `json:"nodes"`
	}
	decode(t, resp, http.StatusOK, &pend)
	if len(pend.Nodes) != 1 || pend.Nodes[0].Name != "spark-1-2" || pend.Nodes[0].Hardware.GPU != "NVIDIA GB10" || pend.Nodes[0].TokenID != tr.ID {
		t.Fatalf("pending %+v", pend.Nodes)
	}
	resp, _ = admin.Get(srv.URL + "/api/v1/nodes")
	var nodes struct {
		Nodes []NodeView `json:"nodes"`
	}
	decode(t, resp, http.StatusOK, &nodes)
	if len(nodes.Nodes) != 2 || nodes.Nodes[0].State != "joined" || nodes.Nodes[1].State != "pending" || nodes.Nodes[1].Hardware.GPU != "NVIDIA GB10" || nodes.Nodes[1].JoinedAt == 0 {
		t.Fatalf("nodes %+v", nodes.Nodes)
	}
	if len(events) < 2 || events[0].Type != "join" || events[len(events)-1].Node != "spark-1-2" || events[len(events)-1].Status != "pending" {
		t.Fatalf("events %+v", events)
	}
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/nodes/pending/spark-1-2", nil)
	if resp, _ := admin.Do(req); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove pending: %d", resp.StatusCode)
	}

	// A different key behind the same address is refused.
	ca2, _ := pki.NewCA("other")
	bad, _ := PinFromCert(ca2.CertPEM)
	hc2, _ := PinnedHTTP(bad, 5*time.Second)
	if _, err := (&Client{Control: srv.URL, Token: "x", HTTP: hc2}).Images(context.Background()); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("wrong pin: %v", err)
	}
}

func decode(t *testing.T, resp *http.Response, code int, v any) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != code {
		t.Fatalf("HTTP %d, want %d: %s", resp.StatusCode, code, readAll(t, resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNodeName(t *testing.T) {
	for in, want := range map[string]string{"Spark-B10X": "spark-b10x", "gx10_lab.local": "gx10-lab-local", "1box": "n-1box", "": "node"} {
		if got := NodeName(in); got != want {
			t.Errorf("NodeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnitMatchesDeployFile(t *testing.T) {
	b, err := os.ReadFile("../../deploy/systemd/oaf-node.service")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != NodeUnit {
		t.Fatal("join.NodeUnit differs from deploy/systemd/oaf-node.service; keep them identical")
	}
}

func TestHardwareParsing(t *testing.T) {
	lan := lanAddresses([]byte(`[{"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8,"scope":"host"}]},
		{"ifname":"enP7s7","addr_info":[{"family":"inet","local":"192.0.2.12","prefixlen":24,"scope":"global"},{"family":"inet6","local":"fe80::1","scope":"link"}]},
		{"ifname":"enp1s0f1np1","addr_info":[{"family":"inet","local":"10.200.0.1","prefixlen":30,"scope":"global"}]},
		{"ifname":"docker0","addr_info":[{"family":"inet","local":"172.17.0.1","prefixlen":16,"scope":"global"}]}]`),
		map[string]bool{"enp1s0f1np1": true})
	if len(lan) != 1 || lan[0] != "192.0.2.12" {
		t.Fatalf("lan %v", lan)
	}
	ns := neighbors([]byte(`[{"dst":"fe80::2","dev":"enp1s0f1np1","lladdr":"AA:BB:CC:00:00:02","state":["REACHABLE"]},
		{"dst":"fe80::3","lladdr":"aa:bb:cc:00:00:03","state":["FAILED"]},{"dst":"fe80::4","state":["INCOMPLETE"]}]`))
	if len(ns) != 1 || ns[0].MAC != "aa:bb:cc:00:00:02" {
		t.Fatalf("neighbors %v", ns)
	}
	// Collect runs read-only: a replayed box with nothing but meminfo still reports.
	r := hostexec.Replay{
		"cat /proc/meminfo": {Stdout: "MemTotal:       127504504 kB\nMemAvailable:   100000000 kB\n"},
		"uname -m":          {Stdout: "aarch64\n"},
		"cat /etc/hostname": {Stdout: "gx10-new\n"},
	}
	h := Collect(context.Background(), r, preflight.Options{})
	if h.Hostname != "gx10-new" || h.Arch != "aarch64" || h.MemTotalGiB != 121.6 || len(h.Preflight) == 0 {
		t.Fatalf("hardware %+v", h)
	}
}

// join.sh keeps the token out of every command line (G7): it reads it from
// the environment, sends it to curl on stdin, and hands it to oaf join in
// the environment.
func TestScriptKeepsTokenOffCommandLines(t *testing.T) {
	sc := Script("https://192.0.2.10:8443", "sha256//pin")
	for _, want := range []string{`TOKEN="${OAF_JOIN_TOKEN:-}"`, `-K - -o "$tmp"`, `header = "Authorization: Bearer $TOKEN"`, `export OAF_JOIN_TOKEN="$TOKEN"`} {
		if !strings.Contains(sc, want) {
			t.Errorf("join.sh lacks %q", want)
		}
	}
	for _, bad := range []string{`--token "$TOKEN"`, `-H "Authorization: Bearer $TOKEN"`} {
		if strings.Contains(sc, bad) {
			t.Errorf("join.sh still has %q", bad)
		}
	}
	if cmd := Command("https://192.0.2.10:8443", "sha256//pin", "oafj_x"); !strings.HasPrefix(cmd, "export OAF_JOIN_TOKEN=oafj_x; ") || strings.Count(cmd, "oafj_x") != 1 {
		t.Errorf("command %q", cmd)
	}
}
