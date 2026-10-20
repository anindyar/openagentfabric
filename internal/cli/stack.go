package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/bundle"
	"openagentfabric.dev/oaf/internal/config"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/reconcile"
	"openagentfabric.dev/oaf/internal/render"
	"openagentfabric.dev/oaf/internal/secrets"
)

// Default locations on the control node.
const (
	DefaultPKIDir = "/etc/oaf/pki"
)

// dialNodes connects to the node agents; tests replace it.
var dialNodes = reconcile.Dial

// stackFlags are shared by the commands that act on a whole cluster.
type stackFlags struct {
	cluster, catalog, pkiDir, secretsDir string
	timeout                              time.Duration
}

func (f *stackFlags) register(cmd *cobra.Command, timeout time.Duration) {
	cmd.Flags().StringVarP(&f.cluster, "file", "f", DefaultClusterPath, "path to cluster.yaml")
	cmd.Flags().StringVar(&f.catalog, "catalog", DefaultCatalogPath, "path to the model catalog")
	cmd.Flags().StringVar(&f.pkiDir, "pki-dir", DefaultPKIDir, "directory with ca.crt and oafd.{crt,key} (oaf pki init)")
	cmd.Flags().StringVar(&f.secretsDir, "secrets-dir", secrets.DefaultDir, "directory of generated secrets")
	cmd.Flags().DurationVar(&f.timeout, "timeout", timeout, "overall time limit")
}

// stack is a loaded cluster with its agents.
type stack struct {
	cluster  *config.Cluster
	catalog  *config.Catalog
	versions *config.Versions
	nodes    reconcile.Nodes
}

func (f *stackFlags) load(out io.Writer) (*stack, error) {
	c, err := config.LoadCluster(f.cluster)
	if err != nil {
		return nil, err
	}
	cat, err := config.LoadCatalog(f.catalog)
	if err != nil {
		return nil, err
	}
	v, err := config.ParseVersions(bundle.VersionsYAML)
	if err != nil {
		return nil, fmt.Errorf("embedded bundle: %w", err)
	}
	if c.Spec.Bundle.Version != v.Version {
		_, _ = fmt.Fprintf(out, "warning: cluster.yaml asks for bundle %s; this oaf binary carries %s\n", c.Spec.Bundle.Version, v.Version)
	}
	nodes, err := dialNodes(c, f.pkiDir)
	if err != nil {
		return nil, err
	}
	return &stack{cluster: c, catalog: cat, versions: v, nodes: nodes}, nil
}

// render discovers the fabric (when a deployment spans nodes) and renders.
func (s *stack) render(ctx context.Context, src secrets.Source) ([]render.Project, error) {
	fab, err := reconcile.Fabric(ctx, s.nodes, s.cluster)
	if err != nil {
		return nil, err
	}
	// Entitlement verification arrives in M5; until then no features are granted.
	return render.Render(render.Input{Cluster: s.cluster, Catalog: s.catalog, Versions: s.versions, Secrets: src, Fabric: fab})
}

func newApplyCmd() *cobra.Command {
	var f stackFlags
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Render cluster.yaml and converge every node to it",
		Long: "Renders the compose projects for every node from cluster.yaml, the catalog and the pinned\n" +
			"bundle, shows the plan, and applies it through the node agents. Safe to re-run: unchanged\n" +
			"projects are only converged. --dry-run shows the plan and changes nothing (no secrets are\n" +
			"generated either).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			return runApply(ctx, cmd.OutOrStdout(), &f, dryRun)
		},
	}
	f.register(cmd, 30*time.Minute)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the plan only")
	return cmd
}

func runApply(ctx context.Context, out io.Writer, f *stackFlags, dryRun bool) error {
	s, err := f.load(out)
	if err != nil {
		return err
	}
	store := secrets.Store{Dir: f.secretsDir}
	var src secrets.Source = store
	if dryRun {
		src = secrets.Preview{Store: store}
	}
	projects, err := s.render(ctx, src)
	if err != nil {
		return err
	}
	plan, err := reconcile.PlanApply(ctx, s.nodes, projects)
	if err != nil {
		return err
	}
	plan.Write(out)
	if dryRun {
		return nil
	}
	_, _ = fmt.Fprintln(out)
	if err := reconcile.Apply(ctx, s.nodes, projects, plan, out); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "applied; run `oaf status` to follow health (models can take many minutes to load)")
	return nil
}

func newStatusCmd() *cobra.Command {
	var f stackFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show every node's projects and container health",
		Long:  "Exits non-zero when a desired project is missing or unhealthy, or a node is unreachable.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			return runStatus(ctx, cmd.OutOrStdout(), &f)
		},
	}
	f.register(cmd, time.Minute)
	return cmd
}

func runStatus(ctx context.Context, out io.Writer, f *stackFlags) error {
	s, err := f.load(out)
	if err != nil {
		return err
	}
	// Status never generates secrets; with placeholders the render only
	// needs to name the projects.
	projects, err := s.render(ctx, secrets.Preview{Store: secrets.Store{Dir: f.secretsDir}})
	if err != nil {
		_, _ = fmt.Fprintf(out, "warning: cannot render the desired state (%v); showing what the agents run\n", err)
		projects = nil
	}
	states := reconcile.Status(ctx, s.nodes, projects)
	desired := map[string]bool{}
	for _, p := range projects {
		desired[p.Node+"/"+p.Name] = true
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NODE\tPROJECT\tSTATE\tSERVICES")
	bad := 0
	for _, st := range states {
		state := "healthy"
		switch {
		case st.Err != nil:
			state = "unreachable"
		case !st.Exists:
			state = "missing"
		case !desired[st.Node+"/"+st.Project] && projects != nil:
			state = "extra"
		case !st.Healthy():
			state = "unhealthy"
		}
		if state != "healthy" && state != "extra" {
			bad++
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", st.Node, st.Project, state, services(st))
	}
	_ = tw.Flush()
	if bad > 0 {
		return fmt.Errorf("%d project(s) not healthy", bad)
	}
	return nil
}

func services(st reconcile.ProjectState) string {
	if st.Err != nil {
		return st.Err.Error()
	}
	var parts []string
	for _, sv := range st.Services {
		parts = append(parts, sv.GetName()+"="+serviceState(sv))
	}
	return strings.Join(parts, " ")
}

func serviceState(sv *nodev1.Service) string {
	if h := sv.GetHealth(); h != "" {
		return sv.GetState() + "/" + h
	}
	return sv.GetState()
}

func newUninstallCmd() *cobra.Command {
	var f stackFlags
	var purge, yes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove every OAF project on every node",
		Long: "Removes all OAF compose projects (oaf-*) on every node in cluster.yaml. Other containers are\n" +
			"never touched. Volumes (Langfuse data, metrics, Grafana) are kept unless --purge is given.\n" +
			"Model weights, secrets, certificates and the node agents stay in place.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("uninstall removes every OAF project on every node; re-run with --yes to confirm")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			s, err := f.load(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			return reconcile.Uninstall(ctx, s.nodes, purge, cmd.OutOrStdout())
		},
	}
	f.register(cmd, 30*time.Minute)
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the projects' volumes (all traces, metrics and dashboards)")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm")
	return cmd
}

func newInstallCmd() *cobra.Command {
	var f stackFlags
	var force bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Preflight every node, then deploy the full stack",
		Long: "Runs the read-only preflight on every node through its agent, then `oaf fabric check`\n" +
			"(configuration and ib_write_bw on every link), and stops on any failure (--force\n" +
			"continues past failures). Then it applies cluster.yaml like `oaf apply`.\n" +
			"The node agents must already run (see docs/user-guide.md). Fabric links must already\n" +
			"have addresses; OAF does not assign them yet.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			out := cmd.OutOrStdout()
			s, err := f.load(out)
			if err != nil {
				return err
			}
			if err := preflightAll(ctx, out, s); err != nil && !force {
				return err
			}
			_, _ = fmt.Fprintln(out)
			if err := checkFabric(ctx, out, s.nodes, s.cluster, reconcile.FabricOptions{Bandwidth: true}); err != nil && !force {
				return err
			}
			_, _ = fmt.Fprintln(out)
			return runApply(ctx, out, &f, false)
		},
	}
	f.register(cmd, 30*time.Minute)
	cmd.Flags().BoolVar(&force, "force", false, "continue even if preflight or the fabric check fails")
	return cmd
}

func preflightAll(ctx context.Context, out io.Writer, s *stack) error {
	failed := 0
	for _, n := range s.cluster.Spec.Nodes {
		dataPath := ""
		if n.Storage != nil {
			dataPath = n.Storage.DataPath
		}
		resp, err := s.nodes[n.Name].Preflight(ctx, &nodev1.PreflightRequest{DataPath: dataPath})
		if err != nil {
			_, _ = fmt.Fprintf(out, "FAIL  %s: agent unreachable: %v\n", n.Name, err)
			failed++
			continue
		}
		// A single node uses no fabric: DGX OS powers the ConnectX ports down
		// when no cable is plugged in, so their absence is not a failure.
		single := s.cluster.Spec.Fabric.Topology == config.TopologySingle
		worst := "pass"
		var lines []string
		for _, r := range resp.GetResults() {
			st := r.GetStatus()
			if single && r.GetId() == "fabric" && st != "pass" {
				st = "skip"
			}
			switch {
			case st == "fail":
				worst = "fail"
			case st == "warn" && worst == "pass":
				worst = "warn"
			}
			if st != "pass" {
				lines = append(lines, fmt.Sprintf("      %-5s %-14s %s", strings.ToUpper(st), r.GetId(), r.GetSummary()))
			}
		}
		_, _ = fmt.Fprintf(out, "%-5s %s (%s)\n", strings.ToUpper(worst), n.Name, resp.GetHost())
		for _, l := range lines {
			_, _ = fmt.Fprintln(out, l)
		}
		if worst == "fail" {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("preflight failed on %d node(s)", failed)
	}
	return nil
}
