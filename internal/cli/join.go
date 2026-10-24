package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/join"
	"openagentfabric.dev/oaf/internal/pki"
	"openagentfabric.dev/oaf/internal/preflight"
)

// joinRunner runs host commands for `oaf join` (systemctl); tests replace it.
var joinRunner hostexec.Runner = hostexec.Local{}

// blockingChecks are the preflight checks that stop a join: without them
// the box cannot run OAF at all. Others (fabric, memory, workloads) are
// reported and judged when the admin picks a topology.
var blockingChecks = map[string]bool{"host": true, "gpu": true, "docker": true, "nvidia-runtime": true}

func newJoinCmd() *cobra.Command {
	var control, token, pin, name, address, pkiDir, unitDir, dataPath string
	var skipImages, force, noStart bool
	var imagesMode string
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join this box to a cluster (run by join.sh; nothing is deployed)",
		Long: "Runs the read-only preflight, reports this box's hardware, preloads the bundle images from\n" +
			"the control node, enrols in the cluster PKI with the one-time token, and starts the node\n" +
			"agent. The box then waits as a pending node until the admin chooses a topology.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Hour) // images are large
			defer cancel()
			hc, err := join.PinnedHTTP(pin, 0)
			if err != nil {
				return err
			}
			c := &join.Client{Control: control, Token: token, HTTP: hc}

			opts := preflight.Defaults
			if dataPath != "" {
				opts.DataPath = dataPath
			}
			hw := join.Collect(ctx, joinRunner, opts)
			_, _ = fmt.Fprintf(out, "==> %s: %s, %.1f GiB memory, %.0f GiB free at %s\n", hw.Hostname, orUnknown(hw.GPU), hw.MemTotalGiB, hw.DiskFreeGiB, hw.DataPath)
			blocked := 0
			for _, r := range hw.Preflight {
				if r.Status == preflight.Pass {
					continue
				}
				mark := strings.ToUpper(string(r.Status))
				if r.Status == preflight.Fail && blockingChecks[r.ID] {
					blocked++
				}
				_, _ = fmt.Fprintf(out, "    %-5s %-14s %s\n", mark, r.ID, r.Summary)
			}
			if blocked > 0 && !force {
				return fmt.Errorf("preflight: %d blocking check(s) failed; fix them or pass --force", blocked)
			}

			// Copied images arrive untagged: docker load cannot restore the
			// registry digest deployments pin, so a deployment pulls anyway.
			// Copy only when the box cannot reach the registries (offline);
			// a local registry on the control node is the offline answer (M5).
			if !skipImages && imagesMode == "auto" && registryReachable(ctx) {
				_, _ = fmt.Fprintln(out, "==> Registries are reachable: images are pulled when a deployment needs them")
				skipImages = true
			}
			if !skipImages && imagesMode != "never" {
				imgs, err := c.Images(ctx)
				if err != nil {
					return err
				}
				for _, img := range imgs {
					// Already here (a re-run, or pulled by hand): nothing to copy.
					if exec.CommandContext(ctx, "docker", "image", "inspect", img.Ref).Run() == nil {
						_, _ = fmt.Fprintf(out, "==> Image %s already present\n", img.Name)
						continue
					}
					_, _ = fmt.Fprintf(out, "==> Loading image %s (%s)\n", img.Name, img.Ref)
					// A missing image is not worth failing the join for: the
					// node pulls it from the registry when a deployment needs it.
					if err := c.LoadImage(ctx, img.Name); err != nil {
						_, _ = fmt.Fprintf(out, "    warning: %v; it will be pulled when a deployment needs it\n", err)
					}
				}
			}

			resp, err := c.Enrol(ctx, join.EnrolRequest{Name: name, Address: address, Hardware: hw})
			if err != nil {
				return err
			}
			if err := os.MkdirAll(pkiDir, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(pkiDir, pki.CACertFile), []byte(resp.CAPEM), 0o644); err != nil {
				return err
			}
			if err := pki.Save(pkiDir, "node", pki.Pair{CertPEM: []byte(resp.CertPEM), KeyPEM: []byte(resp.KeyPEM)}); err != nil {
				return err
			}
			if err := installNodeUnit(ctx, unitDir, !noStart); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "==> Joined as %s (%s). Nothing is deployed yet: choose the topology in the console.\n", resp.Name, resp.Address)
			return nil
		},
	}
	cmd.Flags().StringVar(&control, "control", "", "control node URL, e.g. https://192.0.2.10:8443")
	cmd.Flags().StringVar(&token, "token", "", "one-time join token")
	cmd.Flags().StringVar(&pin, "pin", "", "control node TLS key pin (sha256//...), from the join command")
	cmd.Flags().StringVar(&name, "name", "", "node name (default: from the hostname)")
	cmd.Flags().StringVar(&address, "address", "", "LAN address the control node reaches this box on (default: the address it sees)")
	cmd.Flags().StringVar(&pkiDir, "pki-dir", "/etc/oaf/pki", "where the node certificate goes")
	cmd.Flags().StringVar(&unitDir, "unit-dir", "/etc/systemd/system", "where oaf-node.service goes")
	cmd.Flags().StringVar(&dataPath, "data-path", "", "where models and data will live (disk check)")
	cmd.Flags().BoolVar(&skipImages, "skip-images", false, "do not preload the bundle images (they are pulled at deploy time instead)")
	cmd.Flags().StringVar(&imagesMode, "images", "auto", "copy bundle images from the control node: auto (only when the registries are unreachable), always, never")
	cmd.Flags().BoolVar(&force, "force", false, "join even if a blocking preflight check fails")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "install oaf-node.service without starting it")
	for _, f := range []string{"control", "token", "pin"} {
		_ = cmd.MarkFlagRequired(f)
	}
	return cmd
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown GPU"
	}
	return s
}

// installNodeUnit writes oaf-node.service (when it changed) and starts it.
func installNodeUnit(ctx context.Context, dir string, start bool) error {
	path := filepath.Join(dir, "oaf-node.service")
	cur, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(cur, []byte(join.NodeUnit)) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(join.NodeUnit), 0o644); err != nil {
			return err
		}
	}
	if !start {
		return nil
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "oaf-node.service"}, {"restart", "oaf-node.service"}} {
		if _, err := joinRunner.Run(ctx, "systemctl", args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

// registryReachable reports whether this box can reach the container
// registries the bundle pins (Docker Hub and GHCR answer /v2/ with 401).
func registryReachable(ctx context.Context) bool {
	c := &http.Client{Timeout: 8 * time.Second}
	for _, u := range []string{"https://registry-1.docker.io/v2/", "https://ghcr.io/v2/"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return false
		}
		resp, err := c.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
	}
	return true
}
