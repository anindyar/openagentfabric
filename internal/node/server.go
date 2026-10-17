// Package node implements oaf-node, the agent on every node. oafd drives it
// over mTLS; it manages only its own compose projects (named oaf-*) under its
// state directory and serves read-only inspection.
package node

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"connectrpc.com/connect/v2"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/gen/oaf/node/v1/nodev1connect"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/preflight"
	"openagentfabric.dev/oaf/internal/version"
)

// Server implements the NodeService.
type Server struct {
	nodev1connect.UnimplementedNodeServiceHandler

	// StateDir holds projects/<name>/ for each managed compose project.
	StateDir string
	// Compose runs docker compose (mutating).
	Compose Compose
	// Inspect runs preflight commands; it is wrapped read-only by preflight.
	Inspect hostexec.Runner
	// Preflight defaults.
	PreflightOptions preflight.Options

	mu sync.Mutex // serialises project changes
}

var _ nodev1connect.NodeServiceHandler = (*Server)(nil)

func (s *Server) projectsDir() string { return filepath.Join(s.StateDir, "projects") }

// Info implements NodeService.
func (s *Server) Info(context.Context, *nodev1.InfoRequest) (*nodev1.InfoResponse, error) {
	host, _ := os.Hostname()
	return &nodev1.InfoResponse{Hostname: host, Version: version.Version, Arch: runtime.GOARCH}, nil
}

// Preflight implements NodeService.
func (s *Server) Preflight(ctx context.Context, req *nodev1.PreflightRequest) (*nodev1.PreflightResponse, error) {
	opts := s.PreflightOptions
	if req.GetDataPath() != "" {
		opts.DataPath = req.GetDataPath()
	}
	rep := preflight.Run(ctx, s.Inspect, opts)
	resp := &nodev1.PreflightResponse{Host: rep.Host, Worst: string(rep.Worst())}
	for _, r := range rep.Results {
		resp.Results = append(resp.Results, &nodev1.CheckResult{Id: r.ID, Status: string(r.Status), Summary: r.Summary, Details: r.Details})
	}
	return resp, nil
}

// ApplyProject implements NodeService.
func (s *Server) ApplyProject(ctx context.Context, req *nodev1.ApplyProjectRequest) (*nodev1.ApplyProjectResponse, error) {
	if err := validateName(req.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := validateFiles(req.GetFiles()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.projectsDir(), 0o755); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	dir := filepath.Join(s.projectsDir(), req.GetName())
	current, err := readDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	changed := !sameFiles(current, req.GetFiles())
	if changed {
		if err := writeProject(s.projectsDir(), req.GetName(), req.GetFiles()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, "writing project: "+err.Error())
		}
	}
	// Always converge: `up` is idempotent and restarts anything that stopped.
	if err := s.Compose.Up(ctx, req.GetName(), dir); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, "compose up: "+err.Error())
	}
	services, err := s.services(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	return &nodev1.ApplyProjectResponse{Changed: changed, Services: services}, nil
}

// RemoveProject implements NodeService.
func (s *Server) RemoveProject(ctx context.Context, req *nodev1.RemoveProjectRequest) (*nodev1.RemoveProjectResponse, error) {
	if err := validateName(req.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.projectsDir(), req.GetName())
	if _, err := os.Stat(filepath.Join(dir, ComposeFile)); errors.Is(err, fs.ErrNotExist) {
		return &nodev1.RemoveProjectResponse{Existed: false}, nil
	}
	if err := s.Compose.Down(ctx, req.GetName(), dir, req.GetRemoveVolumes()); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, "compose down: "+err.Error())
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	return &nodev1.RemoveProjectResponse{Existed: true}, nil
}

// ProjectStatus implements NodeService.
func (s *Server) ProjectStatus(ctx context.Context, req *nodev1.ProjectStatusRequest) (*nodev1.ProjectStatusResponse, error) {
	if err := validateName(req.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	_, err := os.Stat(filepath.Join(s.projectsDir(), req.GetName(), ComposeFile))
	exists := err == nil
	services, err := s.services(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	return &nodev1.ProjectStatusResponse{Exists: exists, Services: services}, nil
}

func (s *Server) services(ctx context.Context, name string) ([]*nodev1.Service, error) {
	states, err := s.Compose.PS(ctx, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, "compose ps: "+err.Error())
	}
	out := make([]*nodev1.Service, 0, len(states))
	for _, st := range states {
		out = append(out, &nodev1.Service{Name: st.Service, State: st.State, Health: st.Health, Image: st.Image})
	}
	return out, nil
}
