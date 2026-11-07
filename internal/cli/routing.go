package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/config"
)

func newRoutingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "routing", Short: "Edit model: auto routing in cluster.yaml (rules, defaults, mode)"}
	rule := &cobra.Command{Use: "rule", Short: "Add, change or remove a routing rule"}
	rule.AddCommand(newRoutingRuleSetCmd(), newRoutingRuleRemoveCmd())
	cmd.AddCommand(rule, newRoutingModeCmd(), newRoutingDefaultsCmd())
	return cmd
}

// clusterFlags are the paths every cluster.yaml edit takes.
type clusterFlags struct{ cluster, catalog string }

func (f *clusterFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&f.cluster, "file", "f", DefaultClusterPath, "path to cluster.yaml")
	cmd.Flags().StringVar(&f.catalog, "catalog", DefaultCatalogPath, "path to the model catalog")
}

func newRoutingRuleSetCmd() *cobra.Command {
	var f clusterFlags
	var target, keywords, entitlement, before string
	var minTokens int
	var caseSensitive bool
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Add or replace routing rule <name> (idempotent)",
		Long: "Sets the rule to exactly these settings: when a model: auto request matches --keywords\n" +
			"(any of them) or is at least --min-tokens long, vSR sends it to --target, a deployment or\n" +
			"a LoRA adapter. Rules are ordered and the first match wins: a new rule goes last, or\n" +
			"before --before; an existing one keeps its place unless --before is given. With\n" +
			"--requires-entitlement the rule is rendered only while that entitlement is granted.\n" +
			"Comments in cluster.yaml are kept; the result is validated before it is written. Run\n" +
			"`oaf apply` afterwards.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := config.RoutingRule{Name: args[0], Target: target}
			if kw := splitList(keywords); len(kw) > 0 {
				r.Match.Signals = append(r.Match.Signals, config.Signal{Type: config.SignalKeyword, Keywords: kw, CaseSensitive: caseSensitive})
			}
			if minTokens > 0 {
				r.Match.Signals = append(r.Match.Signals, config.Signal{Type: config.SignalContext, MinTokens: minTokens})
			}
			if len(r.Match.Signals) == 0 {
				return errors.New("give --keywords and/or --min-tokens")
			}
			if entitlement != "" {
				r.Requires = &config.RuleRequires{Entitlement: entitlement}
			}
			var old *config.RoutingRule
			changed, err := editCluster(cmd.ErrOrStderr(), f.cluster, f.catalog, func(data []byte) ([]byte, bool, error) {
				out, o, changed, err := config.SetRoutingRule(data, r, before)
				old = o
				return out, changed, err
			}, nil)
			if err != nil {
				return err
			}
			verb := "added"
			switch {
			case !changed:
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s already has rule %q\n", f.cluster, r.Name)
				return err
			case old != nil:
				verb = "updated"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s rule %q → %s; run `oaf apply` to deploy it\n", verb, r.Name, r.Target)
			return err
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&target, "target", "", "deployment or adapter that matching requests go to (required)")
	cmd.Flags().StringVar(&keywords, "keywords", "", "comma-separated keywords; any one matches")
	cmd.Flags().BoolVar(&caseSensitive, "case-sensitive", false, "match keywords case-sensitively")
	cmd.Flags().IntVar(&minTokens, "min-tokens", 0, "also match prompts at least this many tokens long")
	cmd.Flags().StringVar(&entitlement, "requires-entitlement", "", "render the rule only while this entitlement feature is granted")
	cmd.Flags().StringVar(&before, "before", "", "put the rule right before this rule")
	_ = cmd.MarkFlagRequired("target")
	return cmd
}

func newRoutingRuleRemoveCmd() *cobra.Command {
	var f clusterFlags
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove routing rule <name> (an unknown name is an error)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			changed, err := editCluster(cmd.ErrOrStderr(), f.cluster, f.catalog, func(data []byte) ([]byte, bool, error) {
				out, err := config.RemoveRoutingRule(data, args[0])
				return out, err == nil, err
			}, nil)
			if err == nil && changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed rule %q; run `oaf apply` to deploy\n", args[0])
			}
			return err
		},
	}
	f.register(cmd)
	return cmd
}

func newRoutingModeCmd() *cobra.Command {
	var f clusterFlags
	cmd := &cobra.Command{
		Use:   "mode observe|active",
		Short: "observe: vSR only records its decision (requests go to the fast model); active: decisions apply",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := args[0]
			if mode != config.ModeObserve && mode != config.ModeActive {
				return fmt.Errorf("mode is observe or active, not %q", mode)
			}
			changed, err := editCluster(cmd.ErrOrStderr(), f.cluster, f.catalog, func(data []byte) ([]byte, bool, error) {
				return config.SetRouting(data, config.RoutingUpdate{Mode: &mode})
			}, nil)
			if err != nil {
				return err
			}
			if !changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "routing mode is already %s\n", mode)
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "routing mode %s; run `oaf apply` to deploy\n", mode)
			return err
		},
	}
	f.register(cmd)
	return cmd
}

func newRoutingDefaultsCmd() *cobra.Command {
	var f clusterFlags
	var fast, fallback string
	cmd := &cobra.Command{
		Use:   "defaults [--fast <model>] [--fallback <model>]",
		Short: "Set the model: auto default (fast) and the model used when a target is down (fallback)",
		Long: "A flag left out keeps its current value. Both name a chat deployment or a served LoRA\n" +
			"adapter. Run `oaf apply` afterwards.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fast == "" && fallback == "" {
				return errors.New("give --fast and/or --fallback")
			}
			changed, err := editCluster(cmd.ErrOrStderr(), f.cluster, f.catalog, func(data []byte) ([]byte, bool, error) {
				c, err := config.ParseCluster(data)
				if err != nil {
					return nil, false, err
				}
				d := c.Spec.Routing.Defaults
				if fast != "" {
					d.Fast = fast
				}
				if fallback != "" {
					d.Fallback = fallback
				}
				if d == c.Spec.Routing.Defaults {
					return data, false, nil
				}
				return config.SetRouting(data, config.RoutingUpdate{Defaults: &d})
			}, nil)
			if err != nil {
				return err
			}
			if !changed {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "routing defaults unchanged")
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "routing defaults set; run `oaf apply` to deploy")
			return err
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&fast, "fast", "", "default model for model: auto")
	cmd.Flags().StringVar(&fallback, "fallback", "", "model used when the chosen one is unhealthy")
	return cmd
}
