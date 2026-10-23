package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/keys"
	"openagentfabric.dev/oaf/internal/reconcile"
	"openagentfabric.dev/oaf/internal/render"
	"openagentfabric.dev/oaf/internal/secrets"
)

func newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Issue and rotate the gateway API keys apps use",
		Long: "Each app that calls models through the gateway gets its own named key in\n" +
			"routing.keys. `issue` adds one (idempotent, for installers), `rotate` replaces one,\n" +
			"and `serve` runs the key service apps call to rotate their own key.",
	}
	cmd.AddCommand(newKeysIssueCmd(), newKeysRotateCmd(), newKeysListCmd(), newKeysServeCmd())
	return cmd
}

func (f *stackFlags) keyService(log io.Writer) *keys.Service {
	return &keys.Service{
		ClusterPath:  f.cluster,
		Secrets:      secrets.Store{Dir: f.secretsDir},
		ApplyGateway: func(ctx context.Context) error { return applyGateway(ctx, log, f) },
	}
}

// applyGateway re-renders the cluster and applies only the gateway projects.
// The plan covers every project, so nothing is mistaken for an extra and
// removed; the other projects' steps are dropped.
func applyGateway(ctx context.Context, out io.Writer, f *stackFlags) error {
	s, err := f.load(out)
	if err != nil {
		return err
	}
	projects, err := s.render(ctx, secrets.Store{Dir: f.secretsDir})
	if err != nil {
		return err
	}
	plan, err := reconcile.PlanApply(ctx, s.nodes, projects)
	if err != nil {
		return err
	}
	var gw reconcile.Plan
	for _, st := range plan {
		if st.Project == render.ProjectGateway && st.Action != reconcile.Remove {
			gw = append(gw, st)
		}
	}
	if len(gw) == 0 {
		return errors.New("no gateway project to apply")
	}
	return reconcile.Apply(ctx, s.nodes, projects, gw, out)
}

func newKeysIssueCmd() *cobra.Command {
	var f stackFlags
	var models []string
	var host, envOut string
	cmd := &cobra.Command{
		Use:   "issue <name>",
		Short: "Make sure an app has a gateway key, and print its connection settings",
		Long: "Adds routing.keys entry <name> (secret gateway-key-<name>) to cluster.yaml if it is\n" +
			"missing, generates the key, applies the gateway, and prints the app's settings as\n" +
			"AGENT_GATEWAY_* env lines. Re-running returns the same key. --host is the gateway\n" +
			"address as the app sees it (host.docker.internal for a container on the gateway node).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			creds, err := f.keyService(cmd.ErrOrStderr()).Issue(ctx, args[0], models)
			if err != nil {
				return err
			}
			env, err := appEnv(&f, creds, host)
			if err != nil {
				return err
			}
			if envOut == "" {
				_, err = io.WriteString(cmd.OutOrStdout(), env)
				return err
			}
			if err := os.WriteFile(envOut, []byte(env), 0o600); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", envOut)
			return nil
		},
	}
	f.register(cmd, 10*time.Minute)
	cmd.Flags().StringSliceVar(&models, "models", nil, "models the key may use (default: all)")
	cmd.Flags().StringVar(&host, "host", "", "gateway address as the app reaches it (default: the gateway node's address)")
	cmd.Flags().StringVar(&envOut, "env-out", "", "write the env lines to this file (0600) instead of stdout")
	return cmd
}

// appEnv is what an app needs to use the gateway: where it is, its key,
// where to rotate the key, and the default model and context window.
func appEnv(f *stackFlags, creds keys.Credentials, host string) (string, error) {
	c, err := config.LoadCluster(f.cluster)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = gatewayAddress(c)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "AGENT_GATEWAY_URL=http://%s/v1\n", joinHostPort(host, c.GatewayPort()))
	fmt.Fprintf(&b, "AGENT_GATEWAY_API_KEY=%s\n", creds.Key)
	fmt.Fprintf(&b, "AGENT_GATEWAY_ROTATE_URL=http://%s/v1/keys/rotate\n", joinHostPort(host, config.PortKeyService))
	fmt.Fprintf(&b, "AGENT_GATEWAY_MODEL=%s\n", render.AutoModel)
	if cat, err := config.LoadCatalog(f.catalog); err == nil {
		if w := smallestContextWindow(c, cat); w > 0 {
			fmt.Fprintf(&b, "AGENT_GATEWAY_CONTEXT_WINDOW=%d\n", w)
		}
		if name, dims := embeddingModel(c, cat); name != "" {
			fmt.Fprintf(&b, "AGENT_GATEWAY_EMBEDDING_MODEL=%s\n", name)
			if dims > 0 {
				fmt.Fprintf(&b, "AGENT_GATEWAY_EMBEDDING_DIMENSIONS=%d\n", dims)
			}
		}
	}
	return b.String(), nil
}

// embeddingModel is the first enabled embedding deployment and its vector size.
func embeddingModel(c *config.Cluster, cat *config.Catalog) (string, int) {
	for _, d := range c.Spec.Models {
		if !d.IsEnabled() {
			continue
		}
		if m, ok := cat.Lookup(d.CatalogRef); ok && m.IsEmbedding() {
			return d.Name, m.EmbeddingDimensions
		}
	}
	return "", 0
}

func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(port)
}

// gatewayAddress is the first gateway node's address.
func gatewayAddress(c *config.Cluster) string {
	for _, n := range c.Spec.Nodes {
		if n.HasRole(config.RoleGateway) {
			return n.Address
		}
	}
	return "127.0.0.1"
}

// smallestContextWindow is the smallest served window among the enabled chat
// models: an app that does not know which model "auto" picks must fit the
// smallest. Embedding models do not count. 0 when no model states one.
func smallestContextWindow(c *config.Cluster, cat *config.Catalog) int {
	smallest := 0
	for _, d := range c.Spec.Models {
		if !d.IsEnabled() {
			continue
		}
		m, known := cat.Lookup(d.CatalogRef)
		if known && m.IsEmbedding() {
			continue
		}
		w := d.MaxModelLen
		if w == 0 && known {
			if p, err := config.Profile(d, m, len(c.PlacementNodes(d.Placement))); err == nil {
				w = p.MaxModelLen
			}
		}
		if w > 0 && (smallest == 0 || w < smallest) {
			smallest = w
		}
	}
	return smallest
}

func newKeysRotateCmd() *cobra.Command {
	var f stackFlags
	cmd := &cobra.Command{
		Use:   "rotate <name>",
		Short: "Replace an app's gateway key; the old one stops working",
		Long: "Generates a new key for routing key <name>, applies the gateway, and prints the new key.\n" +
			"The gateway restarts to pick it up, so requests in flight are dropped. If the apply\n" +
			"fails the old key is restored.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			creds, err := f.keyService(cmd.ErrOrStderr()).Rotate(ctx, args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), creds.Key)
			return err
		},
	}
	f.register(cmd, 10*time.Minute)
	return cmd
}

func newKeysListCmd() *cobra.Command {
	var f stackFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the gateway keys in cluster.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := f.keyService(io.Discard).List()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tSECRET\tMODELS")
			for _, k := range list {
				models := strings.Join(k.Models, ",")
				if models == "" {
					models = "all"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", k.Name, k.SecretRef, models)
			}
			return tw.Flush()
		},
	}
	f.register(cmd, time.Minute)
	return cmd
}

func newKeysServeCmd() *cobra.Command {
	var f stackFlags
	var listen string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the key service apps call to rotate their own key (oaf-keys.service)",
		Long: "Serves POST /v1/keys/rotate: an app presents its current gateway key as a Bearer\n" +
			"token and receives a new one. It can rotate only the key it holds. Plain HTTP, like\n" +
			"the gateway itself: keep it on the LAN.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			svc := f.keyService(cmd.ErrOrStderr())
			hs := &http.Server{
				Addr:              listen,
				Handler:           svc.Handler(log),
				ReadHeaderTimeout: 10 * time.Second,
				// A rotation waits for the gateway to be recreated.
				WriteTimeout: f.timeout + 30*time.Second,
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			errc := make(chan error, 1)
			go func() { errc <- hs.ListenAndServe() }()
			log.Info("oaf key service listening", "addr", listen)
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
	f.register(cmd, 5*time.Minute)
	cmd.Flags().StringVar(&listen, "listen", ":"+strconv.Itoa(config.PortKeyService), "listen address")
	return cmd
}
