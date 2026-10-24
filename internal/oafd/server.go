// Package oafd is OAF's management API (ADR-0002 §1): an HTTPS API on the
// control node that reads and edits cluster.yaml through the same tree edits
// as the CLI, validates the result, offers it as a plan with a diff, and
// applies it through the reconciler. It keeps no state of its own besides
// plans in flight; cluster.yaml stays the only source of truth.
//
// Every request under /api/v1 needs a management token (secret
// oaf-admin-<app>, from `oaf keys issue <app> --admin`). Writes are two
// steps: a POST/PUT/DELETE returns a plan; POST /api/v1/plans/{id}/apply
// applies it, refused with 409 if cluster.yaml changed since.
package oafd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/keys"
	"openagentfabric.dev/oaf/internal/secrets"
)

// AdminSecretPrefix names an app's management token: oaf-admin-<app>.
const AdminSecretPrefix = "oaf-admin-"

// Config wires the server to the cluster. The functions are the CLI's own
// code paths (oaf status, oaf apply); tests replace them.
type Config struct {
	ClusterPath string
	CatalogPath string
	Secrets     secrets.Store
	// StateDir keeps the list of plans so a restart can say "expired".
	StateDir string
	// Apply converges every node to cluster.yaml (oaf apply), writing
	// progress lines to w.
	Apply func(ctx context.Context, w io.Writer) error
	// Status reports nodes, projects and health (oaf status).
	Status func(ctx context.Context) (any, error)
	// GatewayConfig returns the rendered agentgateway config.yaml, read-only.
	// Optional.
	GatewayConfig func(ctx context.Context) ([]byte, error)
	// Keys issues and rotates app keys (the key service).
	Keys *keys.Service
	Log  *slog.Logger
	Now  func() time.Time
	// PlanTTL is how long an unapplied plan can be applied. Default 1 h.
	PlanTTL time.Duration
	// Versions is the bundle (which images need licences accepted).
	Versions *config.Versions
	// Check renders a candidate cluster.yaml without applying it; apply
	// refuses a plan whose change does not render. Optional.
	Check func(ctx context.Context, doc []byte) error
}

// Server is the management API.
type Server struct {
	cfg  Config
	api  *http.ServeMux
	root *http.ServeMux

	mu       sync.Mutex
	plans    map[string]*Plan
	applying string
	subs     map[chan Event]struct{}
}

// New builds the server and its routes.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PlanTTL == 0 {
		cfg.PlanTTL = time.Hour
	}
	s := &Server{cfg: cfg, api: http.NewServeMux(), root: http.NewServeMux(),
		plans: map[string]*Plan{}, subs: map[chan Event]struct{}{}}
	s.routes()
	s.root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.root.Handle("/api/v1/", s.auth(s.api))
	return s
}

// Handler is the server's HTTP handler.
func (s *Server) Handler() http.Handler { return s.root }

// Handle mounts another package's handler under /api/v1 (pattern is a
// ServeMux pattern such as "POST /api/v1/nodes/join-tokens"), behind the
// same authentication.
func (s *Server) Handle(pattern string, h http.Handler) { s.api.Handle(pattern, h) }

// HandlePublic mounts a handler that does its own authentication (the join
// flow: a join token, not a management token). A more specific pattern than
// /api/v1/ wins over the authenticated tree.
func (s *Server) HandlePublic(pattern string, h http.Handler) { s.root.Handle(pattern, h) }

// Publish sends a progress event to /api/v1/events subscribers (join,
// preflight, link and fabric progress from other packages).
func (s *Server) Publish(typ, status, message string) {
	s.publish(Event{Type: typ, Status: status, Message: message})
}

// SavePlan records a change built elsewhere (the topology planner) as a plan
// against before, with the same validation as the API's own plans.
func (s *Server) SavePlan(ctx context.Context, summary string, before, after []byte) (string, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	if err != nil {
		return "", err
	}
	p, _ := s.offer(r, before, after, summary)
	return p.ID, nil
}

// auth requires a management token: the value of any oaf-admin-* secret.
// Digests are compared in constant time; failures are slowed down.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		app := ""
		if ok && strings.TrimSpace(presented) != "" {
			app = s.tokenOwner(strings.TrimSpace(presented))
		}
		if app == "" {
			s.cfg.Log.Warn("management API: refused", "remote", r.RemoteAddr, "path", r.URL.Path)
			time.Sleep(500 * time.Millisecond)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "a management token is required (oaf keys issue <app> --admin)"})
			return
		}
		r.Header.Set("X-OAF-App", app)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) tokenOwner(presented string) string {
	entries, err := os.ReadDir(s.cfg.Secrets.Dir)
	if err != nil {
		return ""
	}
	want := sha256.Sum256([]byte(presented))
	found := ""
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, AdminSecretPrefix) {
			continue
		}
		v, ok, err := s.cfg.Secrets.Lookup(name)
		if err != nil || !ok {
			continue
		}
		got := sha256.Sum256([]byte(v))
		if subtle.ConstantTimeCompare(want[:], got[:]) == 1 && found == "" {
			found = strings.TrimPrefix(name, AdminSecretPrefix)
		}
	}
	return found
}

func (s *Server) readCluster() ([]byte, *config.Cluster, error) {
	data, err := os.ReadFile(s.cfg.ClusterPath)
	if err != nil {
		return nil, nil, err
	}
	c, err := config.ParseCluster(data)
	if err != nil {
		return nil, nil, err
	}
	return data, c, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
