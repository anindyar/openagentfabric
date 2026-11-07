package cli

import (
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/config"
)

func newModelsAdapterCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "adapter", Short: "Add or remove LoRA adapters served by a model deployment"}
	cmd.AddCommand(newModelsAdapterAddCmd(), newModelsAdapterRemoveCmd())
	return cmd
}

func newModelsAdapterAddCmd() *cobra.Command {
	var clusterPath, catalogPath, dir, entitlement, guardrails string
	var rank int
	cmd := &cobra.Command{
		Use:   "add <deployment> <adapter-name>",
		Short: "Serve a LoRA adapter on a deployment's vLLM (idempotent; updates a changed adapter)",
		Long: "Sets spec.models[<deployment>].adapters[<adapter-name>] to the given settings: added if new,\n" +
			"left alone if it already has them, otherwise updated (the changes are printed). The flags\n" +
			"are the adapter's whole definition: --guardrails left out means it inherits the\n" +
			"deployment's. Copy the adapter directory (adapter_config.json, adapter_model.safetensors)\n" +
			"to --path on the deployment's node first: when it is on this host, its rank is checked.\n" +
			"Comments in cluster.yaml are kept; the result is validated before it is written. Run\n" +
			"`oaf apply` to serve it.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !path.IsAbs(dir) {
				return fmt.Errorf("--path must be an absolute directory, got %q", dir)
			}
			a := config.Adapter{Name: args[1], Path: path.Clean(dir), Rank: rank, Entitlement: entitlement}
			if cmd.Flags().Changed("guardrails") {
				a.Guardrails = splitList(guardrails)
			}
			var old *config.Adapter
			changed, err := editCluster(cmd.ErrOrStderr(), clusterPath, catalogPath, func(data []byte) ([]byte, bool, error) {
				out, o, changed, err := config.SetAdapter(data, args[0], a)
				old = o
				return out, changed, err
			}, func(c *config.Cluster, cat *config.Catalog) []config.Issue {
				return config.CheckAdapterFiles(c, cat, os.DirFS("/"))
			})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			switch {
			case !changed:
				_, err = fmt.Fprintf(w, "%s already serves adapter %q on %s\n", clusterPath, a.Name, args[0])
			case old == nil:
				_, err = fmt.Fprintf(w, "added adapter %q on %s (rank %d, %s); run `oaf apply` to serve it\n", a.Name, args[0], a.Rank, a.Path)
			default:
				_, err = fmt.Fprintf(w, "updated adapter %q on %s: %s; run `oaf apply` to serve it\n", a.Name, args[0], strings.Join(adapterChanges(*old, a), ", "))
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&clusterPath, "file", "f", DefaultClusterPath, "path to cluster.yaml")
	cmd.Flags().StringVar(&catalogPath, "catalog", DefaultCatalogPath, "path to the model catalog")
	cmd.Flags().StringVar(&dir, "path", "", "absolute adapter directory on the deployment's node(s) (required)")
	cmd.Flags().IntVar(&rank, "rank", 0, "LoRA rank, r in adapter_config.json (required)")
	cmd.Flags().StringVar(&entitlement, "entitlement", "", "serve it only while this entitlement feature is granted")
	cmd.Flags().StringVar(&guardrails, "guardrails", "", "comma-separated guardrail policies (default: the deployment's)")
	_ = cmd.MarkFlagRequired("path")
	_ = cmd.MarkFlagRequired("rank")
	return cmd
}

func adapterChanges(old, cur config.Adapter) []string {
	var out []string
	diff := func(field, a, b string) {
		if a != b {
			out = append(out, fmt.Sprintf("%s %s → %s", field, orNone(a), orNone(b)))
		}
	}
	diff("path", old.Path, cur.Path)
	diff("rank", strconv.Itoa(old.Rank), strconv.Itoa(cur.Rank))
	diff("entitlement", old.Entitlement, cur.Entitlement)
	g := func(l []string) string {
		if l == nil {
			return "(the deployment's)"
		}
		return "[" + strings.Join(l, ",") + "]"
	}
	diff("guardrails", g(old.Guardrails), g(cur.Guardrails))
	if len(out) == 0 {
		out = append(out, "rewritten")
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func newModelsAdapterRemoveCmd() *cobra.Command {
	var clusterPath, catalogPath string
	cmd := &cobra.Command{
		Use:   "remove <deployment> <adapter-name>",
		Short: "Stop serving a LoRA adapter (its files stay on disk)",
		Long: "Removes the adapter from cluster.yaml, comments kept. Validation refuses the change while a\n" +
			"routing default or an ungated rule still targets it (an entitlement-gated rule only goes\n" +
			"inactive, with a warning). Run `oaf apply` afterwards.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			changed, err := editCluster(cmd.ErrOrStderr(), clusterPath, catalogPath, func(data []byte) ([]byte, bool, error) {
				out, err := config.RemoveAdapter(data, args[0], args[1])
				return out, err == nil, err
			}, nil)
			if err != nil {
				return err
			}
			if changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed adapter %q from %s; run `oaf apply` to stop serving it\n", args[1], args[0])
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&clusterPath, "file", "f", DefaultClusterPath, "path to cluster.yaml")
	cmd.Flags().StringVar(&catalogPath, "catalog", DefaultCatalogPath, "path to the model catalog")
	return cmd
}
