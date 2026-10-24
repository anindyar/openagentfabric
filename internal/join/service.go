package join

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"openagentfabric.dev/oaf/internal/pki"
)

// Image is one bundle image a joining box preloads.
type Image struct {
	Name string `json:"name"` // bundle component name
	Ref  string `json:"ref"`  // pinned reference (by digest)
}

// ImageSource lists the bundle's images and streams one as a `docker save`
// tarball.
type ImageSource interface {
	Images(ctx context.Context) ([]Image, error)
	Save(ctx context.Context, name string, w io.Writer) error
}

// Service is the join flow on the control node. oafd mounts PublicRoutes
// as they are (they authenticate with the join token) and AdminRoutes behind
// its admin authentication.
type Service struct {
	Tokens  *TokenStore
	Pending *PendingStore
	// CA signs node certificates (cert and key).
	CA pki.Pair
	// ControlURL is how new boxes reach oafd, e.g. https://192.0.2.10:8443.
	ControlURL string
	// Pin is oafd's TLS key pin (PinFromCert), baked into join.sh.
	Pin string
	// BinaryDir holds oaf-linux-<arch> builds for other architectures; this
	// binary itself serves its own architecture.
	BinaryDir string
	Images    ImageSource
	// Taken reports node names already in cluster.yaml, so a joining box
	// never takes one.
	Taken func() map[string]bool
	Now   func() time.Time
	// Emit receives progress events (oafd forwards them on /api/v1/events).
	Emit func(Event)
	// Nodes lists the nodes in cluster.yaml with their state, for GET
	// /api/v1/nodes (oafd fills it from the cluster and its agents).
	Nodes func(ctx context.Context) []NodeView
}

// Event is one progress event for the UI. Type is join, preflight, link or
// fabric (the wizard filters link tests by the link/fabric prefix).
type Event struct {
	Type    string `json:"type"`
	Node    string `json:"node,omitempty"`
	Status  string `json:"status,omitempty"`
	Message string `json:"message"`
	At      int64  `json:"at"`
}

func (s *Service) emit(e Event) {
	if s.Emit == nil {
		return
	}
	if e.At == 0 {
		e.At = time.Now().Unix()
		if s.Now != nil {
			e.At = s.Now().Unix()
		}
	}
	s.Emit(e)
}

// PublicRoutes are reachable by a box that holds a join token.
func (s *Service) PublicRoutes() map[string]http.Handler {
	return map[string]http.Handler{
		"GET /join.sh":                   http.HandlerFunc(s.script),
		"GET /api/v1/join/binary":        s.withToken(s.binary),
		"GET /api/v1/join/images":        s.withToken(s.imageList),
		"GET /api/v1/join/images/{name}": s.withToken(s.image),
		"POST /api/v1/join/enrol":        http.HandlerFunc(s.enrol),
	}
}

// AdminRoutes are for the admin (oafd wraps them in its authentication).
func (s *Service) AdminRoutes() map[string]http.Handler {
	return map[string]http.Handler{
		"POST /api/v1/nodes/join-tokens":      http.HandlerFunc(s.createToken),
		"GET /api/v1/nodes/join-tokens":       http.HandlerFunc(s.listTokens),
		"GET /api/v1/nodes":                   http.HandlerFunc(s.listNodes),
		"GET /api/v1/nodes/pending":           http.HandlerFunc(s.listPending),
		"DELETE /api/v1/nodes/pending/{name}": http.HandlerFunc(s.removePending),
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func bearer(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func tokenStatus(err error) int {
	switch {
	case errors.Is(err, ErrTokenUnknown), errors.Is(err, ErrTokenExpired), errors.Is(err, ErrTokenUsed):
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

func (s *Service) withToken(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.Tokens.Check(bearer(r)); err != nil {
			fail(w, tokenStatus(err), err.Error())
			return
		}
		h(w, r)
	})
}

func (s *Service) script(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = io.WriteString(w, Script(s.ControlURL, s.Pin))
}

var archRE = regexp.MustCompile(`^(arm64|amd64)$`)

func (s *Service) binary(w http.ResponseWriter, r *http.Request) {
	arch := r.URL.Query().Get("arch")
	if !archRE.MatchString(arch) {
		fail(w, http.StatusBadRequest, "arch must be arm64 or amd64")
		return
	}
	path := filepath.Join(s.BinaryDir, "oaf-linux-"+arch)
	if arch == runtime.GOARCH {
		if self, err := os.Executable(); err == nil {
			path = self
		}
	}
	f, err := os.Open(path)
	if err != nil {
		fail(w, http.StatusNotFound, "no oaf build for "+arch+" on the control node")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

func (s *Service) imageList(w http.ResponseWriter, r *http.Request) {
	if s.Images == nil {
		writeJSON(w, http.StatusOK, map[string]any{"images": []Image{}})
		return
	}
	imgs, err := s.Images.Images(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": imgs})
}

func (s *Service) image(w http.ResponseWriter, r *http.Request) {
	if s.Images == nil {
		fail(w, http.StatusNotFound, "no images")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := s.Images.Save(r.Context(), r.PathValue("name"), w); err != nil {
		// Headers may be out already; a broken stream fails `docker load`.
		fail(w, http.StatusNotFound, err.Error())
	}
}

// EnrolRequest is what `oaf join` sends.
type EnrolRequest struct {
	Name     string   `json:"name,omitempty"` // requested node name; default from the hostname
	Address  string   `json:"address,omitempty"`
	Hardware Hardware `json:"hardware"`
}

// EnrolResponse carries the node's identity in the cluster PKI.
type EnrolResponse struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	CAPEM   string `json:"ca_pem"`
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
}

var nameRE = regexp.MustCompile(`[^a-z0-9-]+`)

// NodeName turns a hostname into a valid node name (DNS label).
func NodeName(s string) string {
	s = strings.Trim(nameRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "node"
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "n-" + s
	}
	return s
}

func (s *Service) uniqueName(want string) (string, error) {
	taken := map[string]bool{}
	if s.Taken != nil {
		taken = s.Taken()
	}
	pend, err := s.Pending.List()
	if err != nil {
		return "", err
	}
	for _, p := range pend {
		taken[p.Name] = true
	}
	name := want
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", want, i)
	}
	return name, nil
}

func (s *Service) enrol(w http.ResponseWriter, r *http.Request) {
	var req EnrolRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "invalid enrolment request")
		return
	}
	tok := bearer(r)
	if _, err := s.Tokens.Check(tok); err != nil {
		fail(w, tokenStatus(err), err.Error())
		return
	}
	want := req.Name
	if want == "" {
		want = req.Hardware.Hostname
	}
	name, err := s.uniqueName(NodeName(want))
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The address the control node reaches the agent on: what the box says,
	// else the address its request came from.
	addr := strings.TrimSpace(req.Address)
	if addr == "" {
		addr, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	if net.ParseIP(addr) == nil {
		fail(w, http.StatusBadRequest, "address must be an IP")
		return
	}
	t, err := s.Tokens.Consume(tok, name)
	if err != nil {
		fail(w, tokenStatus(err), err.Error())
		return
	}
	o := pki.LeafOptions{CommonName: name, DNSNames: []string{name}, Server: true}
	seen := map[string]bool{}
	for _, a := range append([]string{addr}, req.Hardware.Addresses...) {
		if ip := net.ParseIP(a); ip != nil && !seen[a] {
			seen[a] = true
			o.IPs = append(o.IPs, ip)
		}
	}
	leaf, err := pki.Issue(s.CA, o)
	if err != nil {
		fail(w, http.StatusInternalServerError, "issuing the node certificate: "+err.Error())
		return
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	if err := s.Pending.Put(PendingNode{Name: name, Address: addr, Hardware: req.Hardware, EnrolledAt: now, TokenID: t.ID}); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.emit(Event{Type: "join", Node: name, Status: "pending", Message: fmt.Sprintf("%s joined from %s (%s, %.1f GiB): waiting for a topology", name, addr, orGPU(req.Hardware.GPU), req.Hardware.MemTotalGiB)})
	for _, res := range req.Hardware.Preflight {
		if res.Status != "pass" {
			s.emit(Event{Type: "preflight", Node: name, Status: string(res.Status), Message: res.ID + ": " + res.Summary})
		}
	}
	writeJSON(w, http.StatusOK, EnrolResponse{Name: name, Address: addr, CAPEM: string(s.CA.CertPEM), CertPEM: string(leaf.CertPEM), KeyPEM: string(leaf.KeyPEM)})
}

// TokenRequest asks for a join token: one token for the count boxes that
// join in this round.
type TokenRequest struct {
	Count      int    `json:"count,omitempty"`
	TTLMinutes int    `json:"ttl_minutes,omitempty"`
	Label      string `json:"label,omitempty"`
}

// TokenResponse is shown to the admin once. Times are epoch seconds.
type TokenResponse struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Count     int    `json:"count"`
	ExpiresAt int64  `json:"expires_at"`
	Command   string `json:"command"`
}

func (s *Service) createToken(w http.ResponseWriter, r *http.Request) {
	var req TokenRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "invalid request")
			return
		}
	}
	if req.TTLMinutes < 0 || req.TTLMinutes > 24*60 {
		fail(w, http.StatusBadRequest, "ttl_minutes must be between 1 and 1440")
		return
	}
	if req.Count < 0 || req.Count > 8 {
		fail(w, http.StatusBadRequest, "count must be between 1 and 8")
		return
	}
	clear, t, err := s.Tokens.Create(req.Label, time.Duration(req.TTLMinutes)*time.Minute, req.Count)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.emit(Event{Type: "join", Status: "token", Message: fmt.Sprintf("join token %s created for %d box(es), valid until %s", t.ID, t.MaxUses, t.ExpiresAt.UTC().Format(time.RFC3339))})
	writeJSON(w, http.StatusCreated, TokenResponse{ID: t.ID, Token: clear, Count: t.MaxUses, ExpiresAt: t.ExpiresAt.Unix(), Command: Command(s.ControlURL, s.Pin, clear)})
}

func (s *Service) listTokens(w http.ResponseWriter, _ *http.Request) {
	ts, err := s.Tokens.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	type view struct {
		ID        string   `json:"id"`
		Label     string   `json:"label,omitempty"`
		Count     int      `json:"count"`
		Uses      int      `json:"uses"`
		ExpiresAt int64    `json:"expires_at"`
		UsedBy    []string `json:"used_by,omitempty"`
	}
	out := make([]view, 0, len(ts))
	for _, t := range ts {
		out = append(out, view{ID: t.ID, Label: t.Label, Count: max(t.MaxUses, 1), Uses: t.Uses, ExpiresAt: t.ExpiresAt.Unix(), UsedBy: t.UsedBy})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (s *Service) listPending(w http.ResponseWriter, _ *http.Request) {
	ns, err := s.Pending.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ns == nil {
		ns = []PendingNode{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": ns})
}

func (s *Service) removePending(w http.ResponseWriter, r *http.Request) {
	ok, err := s.Pending.Remove(r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		fail(w, http.StatusNotFound, "no pending node "+r.PathValue("name"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DockerImages serves bundle images with `docker save`.
type DockerImages struct {
	List []Image
}

// Images implements ImageSource.
func (d DockerImages) Images(context.Context) ([]Image, error) {
	out := append([]Image(nil), d.List...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Save implements ImageSource.
func (d DockerImages) Save(ctx context.Context, name string, w io.Writer) error {
	for _, img := range d.List {
		if img.Name == name {
			cmd := exec.CommandContext(ctx, "docker", "save", img.Ref)
			cmd.Stdout = w
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("docker save %s: %v %s", img.Ref, err, strings.TrimSpace(stderr.String()))
			}
			return nil
		}
	}
	return fmt.Errorf("no bundle image %q", name)
}

func orGPU(s string) string {
	if s == "" {
		return "unknown GPU"
	}
	return s
}

func (s *Service) listNodes(w http.ResponseWriter, r *http.Request) {
	var out []NodeView
	if s.Nodes != nil {
		out = s.Nodes(r.Context())
	}
	pend, err := s.Pending.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range pend {
		out = append(out, PendingView(p))
	}
	if out == nil {
		out = []NodeView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}
