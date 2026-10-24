package reconcile

import (
	"context"
	"fmt"
	"io"

	"openagentfabric.dev/oaf/internal/config"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
)

// Desktop applies each node's headless setting (spec.nodes[].headless)
// through its agent; nodes that leave it unset are not touched. Run before
// the projects, so memory a desktop gives back is free when models start.
func Desktop(ctx context.Context, ns Nodes, c *config.Cluster, dryRun bool, progress io.Writer) error {
	for _, n := range c.Spec.Nodes {
		if n.Headless == nil {
			continue
		}
		cl, err := ns.client(n.Name)
		if err != nil {
			return err
		}
		before, err := cl.Desktop(ctx, &nodev1.DesktopRequest{})
		if err != nil {
			return fmt.Errorf("%s: desktop: %w", n.Name, err)
		}
		resp, err := cl.Desktop(ctx, &nodev1.DesktopRequest{Set: true, Headless: *n.Headless, DryRun: dryRun})
		if err != nil {
			return fmt.Errorf("%s: desktop: %w", n.Name, err)
		}
		verb := "unchanged"
		if resp.GetChanged() {
			verb = "update   "
			if !dryRun {
				verb = "updated  "
			}
		}
		line := fmt.Sprintf("%s %s/desktop  %s", verb, n.Name, resp.GetMode())
		if resp.GetChanged() {
			line += " (was " + before.GetMode()
			if *n.Headless && before.GetEstimatedMib() > 0 {
				line += fmt.Sprintf(", about %d MiB back", before.GetEstimatedMib())
			}
			line += ")"
		}
		_, _ = fmt.Fprintln(progress, line)
		for _, w := range resp.GetWarnings() {
			_, _ = fmt.Fprintf(progress, "warning: %s: %s\n", n.Name, w)
		}
	}
	return nil
}
