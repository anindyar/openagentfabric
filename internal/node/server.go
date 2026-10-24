// Package node implements oaf-node, the agent on every node. oafd drives it
// over mTLS; it manages only its own compose projects (named oaf-*) under its
// state directory and serves read-only inspection.
package node

import (
	"context"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"openagentfabric.dev/oaf/internal/fabric"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/gen/oaf/node/v1/nodev1connect"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/join"
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
	// Bench runs ib_write_bw for RailBandwidth; nil disables it.
	Bench hostexec.Runner
	// Host makes host changes (the desktop mode); nil disables them.
	Host hostexec.Runner

	mu sync.Mutex // serialises project changes

	hwMu sync.Mutex
	hw   join.Hardware
	hwAt time.Time
}

var _ nodev1connect.NodeServiceHandler = (*Server)(nil)

func (s *Server) projectsDir() string { return filepath.Join(s.StateDir, "projects") }

// Info implements NodeService.
func (s *Server) Info(ctx context.Context, _ *nodev1.InfoRequest) (*nodev1.InfoResponse, error) {
	host, _ := os.Hostname()
	resp := &nodev1.InfoResponse{Hostname: host, Version: version.Version, Arch: runtime.GOARCH}
	if s.Inspect != nil {
		h := s.hardware(ctx)
		resp.Gpu, resp.Serial = h.GPU, h.Serial
		resp.MemTotalGib, resp.MemAvailableGib = h.MemTotalGiB, h.MemAvailableGiB
		resp.DiskFreeGib, resp.DiskTotalGib = h.DiskFreeGiB, h.DiskTotalGiB
	}
	return resp, nil
}

// hardware is join.Basics, cached for 30 s: the console polls Info.
func (s *Server) hardware(ctx context.Context) join.Hardware {
	s.hwMu.Lock()
	defer s.hwMu.Unlock()
	if time.Since(s.hwAt) < 30*time.Second {
		return s.hw
	}
	path := s.PreflightOptions.DataPath
	if path == "" {
		path = "/var/lib/oaf"
	}
	s.hw, s.hwAt = join.Basics(ctx, s.Inspect, path), time.Now()
	return s.hw
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
	paths := changedPaths(current, req.GetFiles())
	changed := len(paths) > 0
	if req.GetDryRun() {
		services, err := s.services(ctx, req.GetName())
		if err != nil {
			return nil, err
		}
		return &nodev1.ApplyProjectResponse{Changed: changed, Services: services, ChangedPaths: paths}, nil
	}
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
	return &nodev1.ApplyProjectResponse{Changed: changed, Services: services, ChangedPaths: paths}, nil
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

// ListProjects implements NodeService.
func (s *Server) ListProjects(context.Context, *nodev1.ListProjectsRequest) (*nodev1.ListProjectsResponse, error) {
	entries, err := os.ReadDir(s.projectsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return &nodev1.ListProjectsResponse{}, nil
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	resp := &nodev1.ListProjectsResponse{}
	for _, e := range entries { // ReadDir sorts by name
		if !e.IsDir() || validateName(e.Name()) != nil {
			continue // skips the .<name>.new-*/.old swap directories
		}
		if _, err := os.Stat(filepath.Join(s.projectsDir(), e.Name(), ComposeFile)); err == nil {
			resp.Names = append(resp.Names, e.Name())
		}
	}
	return resp, nil
}

// Fabric implements NodeService.
func (s *Server) Fabric(ctx context.Context, _ *nodev1.FabricRequest) (*nodev1.FabricResponse, error) {
	ports, err := fabric.Discover(ctx, hostexec.ReadOnly{Inner: s.Inspect})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "fabric discovery: "+err.Error())
	}
	return &nodev1.FabricResponse{Ports: FabricToProto(ports)}, nil
}

// Bandwidth test timing: how long a client retries while its server starts,
// and how long a server waits for its client beyond the test itself.
const (
	benchConnectWithin = 30 * time.Second
	benchServerWait    = time.Minute
)

// RailBandwidth implements NodeService. Only RDMA devices of this node's
// QSFP ports are accepted, and every argument is bounded.
func (s *Server) RailBandwidth(ctx context.Context, req *nodev1.RailBandwidthRequest) (*nodev1.RailBandwidthResponse, error) {
	if s.Bench == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "this agent does not run bandwidth tests")
	}
	t := fabric.BandwidthTest{
		RDMADev:       req.GetRdmaDev(),
		GID:           int(req.GetGidIndex()),
		Port:          int(req.GetTcpPort()),
		Duration:      time.Duration(req.GetDurationSeconds()) * time.Second,
		QPS:           int(req.GetQueuePairs()),
		ConnectWithin: benchConnectWithin,
	}
	if a := req.GetServerAddr(); a != "" {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, "server_addr: "+err.Error())
		}
		t.Server = ip
	}
	if err := t.Validate(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	ports, err := fabric.Discover(ctx, hostexec.ReadOnly{Inner: s.Inspect})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "fabric discovery: "+err.Error())
	}
	if !slices.ContainsFunc(ports, func(p fabric.Port) bool {
		return slices.ContainsFunc(p.Halves, func(n fabric.Netdev) bool { return n.RDMADev == t.RDMADev })
	}) {
		return nil, connect.NewError(connect.CodeInvalidArgument, t.RDMADev+" is not an RDMA device of a QSFP port on this node")
	}
	if !t.Server.IsValid() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Duration+benchServerWait)
		defer cancel()
	}
	gbps, err := fabric.RunBandwidth(ctx, s.Bench, t)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err.Error())
	}
	return &nodev1.RailBandwidthResponse{Gbps: gbps}, nil
}

// Desktop implements NodeService.
func (s *Server) Desktop(ctx context.Context, req *nodev1.DesktopRequest) (*nodev1.DesktopResponse, error) {
	var st DesktopState
	var err error
	if req.GetSet() {
		st, err = SetDesktop(ctx, s.Inspect, s.Host, req.GetHeadless(), req.GetDryRun())
	} else {
		st, err = DesktopReport(ctx, s.Inspect)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "desktop: "+err.Error())
	}
	return &nodev1.DesktopResponse{Mode: st.Mode, DisplayManagerActive: st.DisplayManagerActive,
		EstimatedMib: st.EstimatedMiB, Changed: st.Changed, Warnings: st.Warnings}, nil
}
