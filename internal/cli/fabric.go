package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/node"
	"openagentfabric.dev/oaf/internal/reconcile"
)

func newFabricCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fabric",
		Short: "Check the QSFP fabric",
	}
	cmd.AddCommand(newFabricCheckCmd())
	return cmd
}

func newFabricCheckCmd() *cobra.Command {
	var (
		f         stackFlags
		ssh       []string
		bandwidth bool
		duration  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify every fabric link and measure its bandwidth",
		Long: "For each cable in fabric.links: carrier on both ends, MTU, a shared subnet per rail and a\n" +
			"RoCE v2 GID for each rail address. Then ib_write_bw runs on each rail and on both rails\n" +
			"together, checked against fabric.acceptance. Nothing is reconfigured; the bandwidth test\n" +
			"loads the links for a few seconds.\n\n" +
			"The node agents run the tests. --ssh node=user@host runs them over SSH instead (before\n" +
			"the agents are installed); give it for every node on a link.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			c, err := config.LoadCluster(f.cluster)
			if err != nil {
				return err
			}
			var ns reconcile.Nodes
			if len(ssh) > 0 {
				ns, err = sshNodes(ssh)
			} else {
				ns, err = dialNodes(c, f.pkiDir)
			}
			if err != nil {
				return err
			}
			return checkFabric(ctx, cmd.OutOrStdout(), ns, c, reconcile.FabricOptions{Bandwidth: bandwidth, Duration: duration})
		},
	}
	f.register(cmd, 10*time.Minute)
	cmd.Flags().StringArrayVar(&ssh, "ssh", nil, "run on node over SSH instead of its agent (node=user@host; repeatable)")
	cmd.Flags().BoolVar(&bandwidth, "bandwidth", true, "run ib_write_bw (false checks the configuration only)")
	cmd.Flags().DurationVar(&duration, "duration", reconcile.DefaultBandwidthTime, "length of each bandwidth run")
	return cmd
}

// sshNodes runs in-process agents whose commands go over SSH. They can only
// inspect and run bandwidth tests: they have no compose runner.
func sshNodes(targets []string) (reconcile.Nodes, error) {
	ns := reconcile.Nodes{}
	for _, t := range targets {
		name, target, ok := strings.Cut(t, "=")
		if !ok || name == "" || target == "" {
			return nil, fmt.Errorf("--ssh %q: want node=user@host", t)
		}
		r := hostexec.SSH{Target: target, ControlPath: filepath.Join(os.TempDir(), "oaf-ssh-%C")}
		ns[name] = &node.Server{Inspect: r, Bench: r}
	}
	return ns, nil
}

func checkFabric(ctx context.Context, out io.Writer, ns reconcile.Nodes, c *config.Cluster, opts reconcile.FabricOptions) error {
	if len(c.Spec.Fabric.Links) == 0 {
		_, _ = fmt.Fprintln(out, "fabric: no links to check")
		return nil
	}
	results := reconcile.CheckFabric(ctx, ns, c, opts, out)
	failed := 0
	for _, r := range results {
		if r.OK() {
			summary := fmt.Sprintf("%d rails", len(r.Rails))
			if r.DualGbps > 0 {
				summary += fmt.Sprintf(", %.1f Gb/s together (min %.0f per rail, %.0f together)", r.DualGbps, r.MinGbpsPerRail, r.MinDual)
			}
			_, _ = fmt.Fprintf(out, "PASS  %s  %s\n", r.Name, summary)
			continue
		}
		failed++
		_, _ = fmt.Fprintf(out, "FAIL  %s\n", r.Name)
		for _, p := range r.Problems {
			_, _ = fmt.Fprintf(out, "      %s\n", p)
		}
	}
	if failed > 0 {
		return fmt.Errorf("fabric check failed on %d link(s)", failed)
	}
	return nil
}
