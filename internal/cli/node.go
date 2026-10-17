package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/node"
	"openagentfabric.dev/oaf/internal/pki"
	"openagentfabric.dev/oaf/internal/preflight"
)

func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "node", Short: "The node agent (oaf-node)"}
	cmd.AddCommand(newNodeServeCmd(), newNodeInfoCmd())
	return cmd
}

func newNodeServeCmd() *cobra.Command {
	var listen, pkiDir, stateDir string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the node agent (normally via systemd: oaf-node.service)",
		Long: "Serves the NodeService over mTLS. Reads <pki-dir>/ca.crt and <pki-dir>/node.{crt,key}\n" +
			"and accepts only the oafd client certificate. Manages compose projects named oaf-*\n" +
			"under <state-dir>/projects.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			caPEM, err := os.ReadFile(filepath.Join(pkiDir, pki.CACertFile))
			if err != nil {
				return err
			}
			cert, err := pki.Load(pkiDir, "node")
			if err != nil {
				return err
			}
			tlsCfg, err := pki.ServerTLS(caPEM, cert, pki.ControlPlaneCN)
			if err != nil {
				return err
			}
			srv := &node.Server{
				StateDir:         stateDir,
				Compose:          node.Compose{Runner: hostexec.Local{}},
				Inspect:          hostexec.Local{},
				PreflightOptions: preflight.Defaults,
			}
			hs := node.NewHTTPServer(listen, srv, tlsCfg)

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			errc := make(chan error, 1)
			go func() { errc <- hs.ListenAndServeTLS("", "") }()
			fmt.Fprintf(cmd.OutOrStdout(), "oaf-node listening on %s (state %s)\n", listen, stateDir)
			select {
			case err := <-errc:
				return err
			case <-ctx.Done():
				shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := hs.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			}
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ":"+strconv.Itoa(node.DefaultPort), "listen address")
	cmd.Flags().StringVar(&pkiDir, "pki-dir", "/etc/oaf/pki", "directory with ca.crt, node.crt and node.key")
	cmd.Flags().StringVar(&stateDir, "state-dir", "/var/lib/oaf", "agent state directory")
	return cmd
}

func newNodeInfoCmd() *cobra.Command {
	var addr, name, pkiDir string
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Ask a node agent who it is (uses the oafd client certificate)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			caPEM, err := os.ReadFile(filepath.Join(pkiDir, pki.CACertFile))
			if err != nil {
				return err
			}
			cert, err := pki.Load(pkiDir, "oafd")
			if err != nil {
				return err
			}
			if name == "" {
				name, _, _ = net.SplitHostPort(addr)
			}
			tlsCfg, err := pki.ClientTLS(caPEM, cert, name)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			info, err := node.NewClient(addr, tlsCfg).Info(ctx, &nodev1.InfoRequest{})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: oaf %s (%s)\n", info.GetHostname(), info.GetVersion(), info.GetArch())
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "agent address host:port")
	cmd.Flags().StringVar(&name, "name", "", "expected node name in its certificate (default: host from --addr)")
	cmd.Flags().StringVar(&pkiDir, "pki-dir", "/etc/oaf/pki", "directory with ca.crt and oafd.{crt,key}")
	_ = cmd.MarkFlagRequired("addr")
	return cmd
}
