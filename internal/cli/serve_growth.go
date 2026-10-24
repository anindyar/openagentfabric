package cli

import (
	"context"
	"fmt"
	"log/slog"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/reconcile"
	"os"
	"path/filepath"
	"time"

	"openagentfabric.dev/oaf/bundle"
	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/join"
	"openagentfabric.dev/oaf/internal/oafd"
	"openagentfabric.dev/oaf/internal/pki"
	"openagentfabric.dev/oaf/internal/topology"
)

// mountGrowth adds the join flow and the topology planner to oafd (ADR-0002
// §5). Joining needs the cluster CA's key on this node (oaf pki init).
func mountGrowth(srv *oafd.Server, f *stackFlags, c *config.Cluster, stateDir string, log *slog.Logger) error {
	ca, err := pki.Load(f.pkiDir, "ca")
	if err != nil {
		return fmt.Errorf("no cluster CA in %s: %w", f.pkiDir, err)
	}
	api, err := apiCert(f.pkiDir, stateDir, c)
	if err != nil {
		return err
	}
	pin, err := join.PinFromCert(api.CertPEM)
	if err != nil {
		return err
	}
	control := ""
	for _, n := range c.Spec.Nodes {
		if n.HasRole(config.RoleControl) {
			control = "https://" + joinHostPort(n.Address, config.PortOAFD)
		}
	}
	if control == "" {
		return fmt.Errorf("cluster.yaml names no control node")
	}
	var images []join.Image
	if v, err := config.ParseVersions(bundle.VersionsYAML); err == nil {
		for _, comp := range v.Components {
			images = append(images, join.Image{Name: comp.Name, Ref: comp.Image})
		}
	}
	growth := filepath.Join(stateDir, "join")
	if err := os.MkdirAll(growth, 0o700); err != nil {
		return err
	}
	pending := &join.PendingStore{Path: filepath.Join(growth, "pending.json")}
	svc := &join.Service{
		Tokens:     &join.TokenStore{Path: filepath.Join(growth, "tokens.json")},
		Pending:    pending,
		CA:         ca,
		ControlURL: control,
		Pin:        pin,
		BinaryDir:  filepath.Join(stateDir, "binaries"),
		Images:     join.DockerImages{List: images, OnlyPresent: true},
		Taken: func() map[string]bool {
			cur, err := config.LoadCluster(f.cluster)
			out := map[string]bool{}
			if err == nil {
				for _, n := range cur.Spec.Nodes {
					out[n.Name] = true
				}
			}
			return out
		},
		Emit:  func(e join.Event) { srv.Publish(e.Type, e.Status, e.Message) },
		Nodes: func(ctx context.Context) []join.NodeView { return clusterNodes(ctx, f) },
	}
	for p, h := range svc.PublicRoutes() {
		srv.HandlePublic(p, h)
	}
	for p, h := range svc.AdminRoutes() {
		srv.Handle(p, h)
	}
	for p, h := range topology.Routes(topology.Deps{
		Load: func(context.Context) ([]byte, *config.Cluster, *config.Catalog, error) {
			raw, err := os.ReadFile(f.cluster)
			if err != nil {
				return nil, nil, nil, err
			}
			cur, err := config.ParseCluster(raw)
			if err != nil {
				return nil, nil, nil, err
			}
			cat, err := config.LoadCatalog(f.catalog)
			return raw, cur, cat, err
		},
		Pending:  pending,
		SavePlan: srv.SavePlan,
	}) {
		srv.Handle(p, h)
	}
	log.Info("adding boxes is available", "join", control+"/join.sh")
	return nil
}

// clusterNodes are the nodes in cluster.yaml with the state oafd's status
// knows, for GET /api/v1/nodes.
func clusterNodes(ctx context.Context, f *stackFlags) []join.NodeView {
	out := []join.NodeView{}
	v, err := statusView(ctx, f)
	if err != nil {
		cur, cerr := config.LoadCluster(f.cluster)
		if cerr != nil {
			return out
		}
		for _, n := range cur.Spec.Nodes {
			out = append(out, join.NodeView{Name: n.Name, Address: n.Address, Roles: n.Roles, State: "offline"})
		}
		return out
	}
	cur, _ := config.LoadCluster(f.cluster)
	desktops := nodeDesktops(ctx, f, cur)
	for _, n := range v.Nodes {
		nv := join.NodeView{Name: n.Name, Address: n.Address, Roles: n.Roles, State: n.State, Health: n.Health}
		if n.Hardware != nil {
			nv.Hardware = &join.HardwareView{GPU: n.Hardware.GPU, MemoryGiB: n.Hardware.MemoryGiB,
				MemoryFreeGiB: n.Hardware.MemoryFreeGiB, DiskFreeGiB: n.Hardware.DiskFreeGiB, Serial: n.Hardware.Serial}
		}
		nv.Desktop = desktops[n.Name]
		if cur != nil {
			for _, cn := range cur.Spec.Nodes {
				if cn.Name == n.Name {
					nv.Headless = cn.Headless
				}
			}
		}
		out = append(out, nv)
	}
	return out
}

// nodeDesktops asks each node's agent for its desktop state (a few seconds at
// most; an unreachable agent just has none).
func nodeDesktops(ctx context.Context, f *stackFlags, c *config.Cluster) map[string]*join.DesktopView {
	out := map[string]*join.DesktopView{}
	if c == nil {
		return out
	}
	ns, err := reconcile.Dial(c, f.pkiDir)
	if err != nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for name, cl := range ns {
		resp, err := cl.Desktop(ctx, &nodev1.DesktopRequest{})
		if err != nil {
			continue
		}
		out[name] = &join.DesktopView{Mode: resp.GetMode(), DisplayManagerActive: resp.GetDisplayManagerActive(), EstimatedMiB: resp.GetEstimatedMib()}
	}
	return out
}
