// Package reconcile drives node agents toward the rendered desired state.
// It is used by the CLI now (oaf apply/status/uninstall) and by oafd later.
//
// A plan is computed without changing anything: each agent compares the
// rendered files with what it has (ApplyProject with dry_run) and lists the
// projects it manages, so projects that are no longer rendered (a disabled
// model) are removed. Applying a plan converges every desired project, even
// unchanged ones, because `compose up` also restarts stopped containers.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/gen/oaf/node/v1/nodev1connect"
	"openagentfabric.dev/oaf/internal/node"
	"openagentfabric.dev/oaf/internal/pki"
	"openagentfabric.dev/oaf/internal/render"
)

// Nodes are the agent clients of a cluster, by node name.
type Nodes map[string]nodev1connect.NodeServiceClient

// Dial creates an mTLS client for every node, as oafd (pkiDir holds ca.crt
// and oafd.{crt,key} from `oaf pki init`).
func Dial(c *config.Cluster, pkiDir string) (Nodes, error) {
	caPEM, err := os.ReadFile(filepath.Join(pkiDir, pki.CACertFile))
	if err != nil {
		return nil, err
	}
	cert, err := pki.Load(pkiDir, pki.ControlPlaneCN)
	if err != nil {
		return nil, err
	}
	nodes := Nodes{}
	for _, n := range c.Spec.Nodes {
		tlsCfg, err := pki.ClientTLS(caPEM, cert, n.Name)
		if err != nil {
			return nil, err
		}
		nodes[n.Name] = node.NewClient(hostPort(n.Address, config.PortNodeAgent), tlsCfg)
	}
	return nodes, nil
}

func hostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

func (ns Nodes) client(name string) (nodev1connect.NodeServiceClient, error) {
	c, ok := ns[name]
	if !ok {
		return nil, fmt.Errorf("no agent client for node %q", name)
	}
	return c, nil
}

// Fabric discovers the QSFP ports of the nodes that run multi-node
// deployments; the renderer needs their rails and GIDs.
func Fabric(ctx context.Context, ns Nodes, c *config.Cluster) (map[string][]fabric.Port, error) {
	need := map[string]bool{}
	for _, d := range c.Spec.Models {
		if nodes := c.PlacementNodes(d.Placement); d.IsEnabled() && len(nodes) > 1 {
			for _, n := range nodes {
				need[n] = true
			}
		}
	}
	out := map[string][]fabric.Port{}
	for _, n := range sortedKeys(need) {
		cl, err := ns.client(n)
		if err != nil {
			return nil, err
		}
		resp, err := cl.Fabric(ctx, &nodev1.FabricRequest{})
		if err != nil {
			return nil, fmt.Errorf("%s: fabric discovery: %w", n, err)
		}
		if out[n], err = node.FabricFromProto(resp.GetPorts()); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
	}
	return out, nil
}

// Action is what a step does.
type Action string

// Actions.
const (
	Create    Action = "create"
	Update    Action = "update"
	Unchanged Action = "unchanged"
	Remove    Action = "remove"
)

// Step is one project on one node.
type Step struct {
	Node    string
	Project string
	Action  Action
	Paths   []string // files that change (create/update)
}

// Plan is the ordered list of steps: removals first (they free memory and
// ports), then projects in dependency order.
type Plan []Step

// Changes counts the steps that change something.
func (p Plan) Changes() int {
	n := 0
	for _, s := range p {
		if s.Action != Unchanged {
			n++
		}
	}
	return n
}

// Write prints the plan, one line per step.
func (p Plan) Write(w io.Writer) {
	for _, s := range p {
		line := fmt.Sprintf("%-9s %s/%s", s.Action, s.Node, s.Project)
		if len(s.Paths) > 0 && s.Action == Update {
			line += "  (" + strings.Join(s.Paths, ", ") + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}
	_, _ = fmt.Fprintf(w, "%d change(s), %d unchanged\n", p.Changes(), len(p)-p.Changes())
}

// PlanApply computes the steps that bring the nodes to the desired projects.
func PlanApply(ctx context.Context, ns Nodes, desired []render.Project) (Plan, error) {
	byNode := map[string][]render.Project{}
	for _, p := range desired {
		byNode[p.Node] = append(byNode[p.Node], p)
	}
	var plan Plan
	var mu sync.Mutex
	err := eachNode(ctx, sortedKeys(ns), func(ctx context.Context, n string) error {
		cl := ns[n]
		list, err := cl.ListProjects(ctx, &nodev1.ListProjectsRequest{})
		if err != nil {
			return fmt.Errorf("%s: list projects: %w", n, err)
		}
		existing := map[string]bool{}
		for _, name := range list.GetNames() {
			existing[name] = true
		}
		var steps []Step
		for _, p := range byNode[n] {
			resp, err := cl.ApplyProject(ctx, applyRequest(p, true))
			if err != nil {
				return fmt.Errorf("%s/%s: %w", n, p.Name, err)
			}
			st := Step{Node: n, Project: p.Name, Action: Unchanged}
			switch {
			case !existing[p.Name]:
				st.Action, st.Paths = Create, resp.GetChangedPaths()
			case resp.GetChanged():
				st.Action, st.Paths = Update, resp.GetChangedPaths()
			}
			delete(existing, p.Name)
			steps = append(steps, st)
		}
		for _, name := range sortedKeys(existing) {
			steps = append(steps, Step{Node: n, Project: name, Action: Remove})
		}
		mu.Lock()
		plan = append(plan, steps...)
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	for n := range byNode {
		if _, ok := ns[n]; !ok {
			return nil, fmt.Errorf("no agent client for node %q", n)
		}
	}
	sortPlan(plan)
	return plan, nil
}

// Apply executes a plan made by PlanApply for the same desired projects.
// Removals keep volumes (a re-enabled model or a restored service finds its
// data); `oaf uninstall --purge` deletes them. progress gets one line per step.
func Apply(ctx context.Context, ns Nodes, desired []render.Project, plan Plan, progress io.Writer) error {
	byKey := map[string]render.Project{}
	for _, p := range desired {
		byKey[p.Node+"/"+p.Name] = p
	}
	var errs []error
	for _, s := range plan {
		cl, err := ns.client(s.Node)
		if err != nil {
			return err
		}
		if s.Action == Remove {
			if _, err := cl.RemoveProject(ctx, &nodev1.RemoveProjectRequest{Name: s.Project}); err != nil {
				errs = append(errs, fmt.Errorf("%s/%s: remove: %w", s.Node, s.Project, err))
				continue
			}
			_, _ = fmt.Fprintf(progress, "removed   %s/%s\n", s.Node, s.Project)
			continue
		}
		p, ok := byKey[s.Node+"/"+s.Project]
		if !ok {
			return fmt.Errorf("plan step %s/%s is not a desired project", s.Node, s.Project)
		}
		resp, err := cl.ApplyProject(ctx, applyRequest(p, false))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", s.Node, s.Project, err))
			continue
		}
		_, _ = fmt.Fprintf(progress, "%-9s %s/%s  %s\n", applied(s.Action, resp.GetChanged()), s.Node, s.Project, summarize(resp.GetServices()))
	}
	return errors.Join(errs...)
}

func applied(a Action, changed bool) string {
	switch {
	case a == Create:
		return "created"
	case changed:
		return "updated"
	default:
		return "converged"
	}
}

// Uninstall removes every OAF project on every node. With purge, volumes
// (databases, metrics, traces) are deleted too. Model weights on the host
// are never touched here.
func Uninstall(ctx context.Context, ns Nodes, purge bool, progress io.Writer) error {
	var errs []error
	for _, n := range sortedKeys(ns) {
		cl := ns[n]
		list, err := cl.ListProjects(ctx, &nodev1.ListProjectsRequest{})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: list projects: %w", n, err))
			continue
		}
		names := list.GetNames()
		sort.SliceStable(names, func(i, j int) bool { return removeOrder(names[i]) < removeOrder(names[j]) })
		for _, name := range names {
			if _, err := cl.RemoveProject(ctx, &nodev1.RemoveProjectRequest{Name: name, RemoveVolumes: purge}); err != nil {
				errs = append(errs, fmt.Errorf("%s/%s: %w", n, name, err))
				continue
			}
			_, _ = fmt.Fprintf(progress, "removed   %s/%s\n", n, name)
		}
	}
	return errors.Join(errs...)
}

// ProjectState is one project's live state on one node.
type ProjectState struct {
	Node     string
	Project  string
	Exists   bool
	Services []*nodev1.Service
	Err      error
}

// Healthy reports whether every container runs and passes its healthcheck.
func (s ProjectState) Healthy() bool {
	if s.Err != nil || !s.Exists || len(s.Services) == 0 {
		return false
	}
	for _, sv := range s.Services {
		if sv.GetState() != "running" || sv.GetHealth() != "" && sv.GetHealth() != "healthy" {
			return false
		}
	}
	return true
}

// ModelLogLines is how much of a model container's log Status asks for while
// it is not healthy.
const ModelLogLines = 300

// ModelProjectPrefix names the compose project of a model deployment.
const ModelProjectPrefix = "model-"

// IsModelProject reports whether a project serves a model deployment.
func IsModelProject(name string) bool {
	return len(name) > len(ModelProjectPrefix) && name[:len(ModelProjectPrefix)] == ModelProjectPrefix
}

// Status reports every desired project plus any extra project an agent
// still runs. An unreachable node is reported per project, not as an error.
func Status(ctx context.Context, ns Nodes, desired []render.Project) []ProjectState {
	want := map[string][]string{}
	for _, p := range desired {
		want[p.Node] = append(want[p.Node], p.Name)
	}
	var out []ProjectState
	var mu sync.Mutex
	_ = eachNode(ctx, sortedKeys(ns), func(ctx context.Context, n string) error {
		cl := ns[n]
		names := slices.Clone(want[n])
		if list, err := cl.ListProjects(ctx, &nodev1.ListProjectsRequest{}); err == nil {
			for _, x := range list.GetNames() {
				if !slices.Contains(names, x) {
					names = append(names, x)
				}
			}
		}
		var states []ProjectState
		for _, name := range names {
			st := ProjectState{Node: n, Project: name}
			req := &nodev1.ProjectStatusRequest{Name: name}
			if IsModelProject(name) {
				req.LogLines = ModelLogLines // loading progress, crash errors
			}
			resp, err := cl.ProjectStatus(ctx, req)
			if err != nil {
				st.Err = err
			} else {
				st.Exists, st.Services = resp.GetExists(), resp.GetServices()
			}
			states = append(states, st)
		}
		mu.Lock()
		out = append(out, states...)
		mu.Unlock()
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return applyOrder(out[i].Project) < applyOrder(out[j].Project) ||
			applyOrder(out[i].Project) == applyOrder(out[j].Project) && out[i].Project < out[j].Project
	})
	return out
}

func summarize(svcs []*nodev1.Service) string {
	parts := make([]string, 0, len(svcs))
	for _, s := range svcs {
		st := s.GetState()
		if h := s.GetHealth(); h != "" {
			st += "/" + h
		}
		parts = append(parts, s.GetName()+"="+st)
	}
	return strings.Join(parts, " ")
}

func applyRequest(p render.Project, dryRun bool) *nodev1.ApplyProjectRequest {
	req := &nodev1.ApplyProjectRequest{Name: p.Name, DryRun: dryRun}
	for _, f := range p.Files {
		req.Files = append(req.Files, &nodev1.File{Path: f.Path, Mode: f.Mode, Content: f.Content})
	}
	return req
}

// applyOrder puts dependencies first: metrics and stores before the models,
// and the gateway last so it starts with its backends in place.
func applyOrder(project string) int {
	switch {
	case project == render.ProjectNodeExporter:
		return 0
	case project == render.ProjectLangfuse:
		return 1
	case project == render.ProjectMetrics:
		return 2
	case strings.HasPrefix(project, "model-"):
		return 3
	case project == render.ProjectRouter:
		return 4
	case project == render.ProjectGateway:
		return 5
	default:
		return 6
	}
}

// removeOrder is the reverse: stop taking traffic first.
func removeOrder(project string) int { return -applyOrder(project) }

func sortPlan(p Plan) {
	sort.SliceStable(p, func(i, j int) bool {
		a, b := p[i], p[j]
		if (a.Action == Remove) != (b.Action == Remove) {
			return a.Action == Remove
		}
		oa, ob := applyOrder(a.Project), applyOrder(b.Project)
		if a.Action == Remove {
			oa, ob = removeOrder(a.Project), removeOrder(b.Project)
		}
		if oa != ob {
			return oa < ob
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Node < b.Node
	})
}

// eachNode runs f for every node concurrently and joins the errors.
func eachNode(ctx context.Context, names []string, f func(context.Context, string) error) error {
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f(ctx, n)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
