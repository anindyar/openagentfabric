package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/config"
)

// observabilityFeatures are the switches apps installed next to OAF turn on.
var observabilityFeatures = map[string][]string{
	"logs":              {"observability", "logs", "enabled"},
	"remote-write":      {"observability", "metrics", "remoteWrite"},
	"anonymous-grafana": {"observability", "grafana", "anonymousViewer"},
}

func newObservabilityCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "observability", Short: "Turn optional observability features on or off in cluster.yaml"}
	cmd.AddCommand(newObservabilitySetCmd("enable", true), newObservabilitySetCmd("disable", false), newObservabilityScrapeCmd(), newObservabilityGrafanaURLCmd())
	return cmd
}

func newObservabilityScrapeCmd() *cobra.Command {
	var clusterPath string
	var insecure bool
	cmd := &cobra.Command{
		Use:   "scrape <name> <url>",
		Short: "Have Prometheus scrape an app's metrics endpoint (idempotent; replaces <name>'s url)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(clusterPath)
			if err != nil {
				return err
			}
			out, changed, err := config.SetScrapeTarget(data, config.ScrapeTarget{Name: args[0], URL: args[1], InsecureSkipVerify: insecure})
			if err != nil {
				return err
			}
			if !changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s already scrapes %s\n", clusterPath, args[1])
				return err
			}
			if err := config.WriteFileAtomic(clusterPath, out); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "scraping %s as app-%s; run `oaf apply` to deploy\n", args[1], args[0])
			return err
		},
	}
	cmd.Flags().StringVarP(&clusterPath, "file", "f", DefaultClusterPath, "path to cluster.yaml")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "accept a self-signed certificate")
	return cmd
}

func newObservabilitySetCmd(verb string, value bool) *cobra.Command {
	var clusterPath string
	cmd := &cobra.Command{
		Use:   verb + " <feature>...",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " features: logs, remote-write, anonymous-grafana (idempotent)",
		Long: "logs: Loki on the observability node, with a Grafana datasource.\n" +
			"remote-write: Prometheus accepts pushed metrics on /api/v1/write.\n" +
			"anonymous-grafana: dashboards can be viewed without signing in, for embedding.\n" +
			"Comments in cluster.yaml are kept; run `oaf apply` afterwards.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(clusterPath)
			if err != nil {
				return err
			}
			changed := false
			for _, f := range args {
				path, ok := observabilityFeatures[f]
				if !ok {
					return fmt.Errorf("unknown feature %q (want logs, remote-write or anonymous-grafana)", f)
				}
				out, c, err := config.SetSpecBool(data, value, path...)
				if err != nil {
					return err
				}
				data, changed = out, changed || c
			}
			if !changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: nothing to change\n", clusterPath)
				return err
			}
			if err := config.WriteFileAtomic(clusterPath, data); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%sd %s; run `oaf apply` to deploy\n", verb, strings.Join(args, ", "))
			return err
		},
	}
	cmd.Flags().StringVarP(&clusterPath, "file", "f", DefaultClusterPath, "path to cluster.yaml")
	return cmd
}

func newObservabilityGrafanaURLCmd() *cobra.Command {
	var clusterPath string
	cmd := &cobra.Command{
		Use:   "grafana-url <url>",
		Short: "Set the public URL an app serves Grafana at through its reverse proxy (\"\" clears it)",
		Long: "For an app that embeds Grafana from its own origin, e.g. https://console/grafana/. A path\n" +
			"makes Grafana serve from that sub path, on its own port too. Run `oaf apply` afterwards.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(clusterPath)
			if err != nil {
				return err
			}
			out, changed, err := config.SetSpecString(data, args[0], "observability", "grafana", "rootUrl")
			if err != nil {
				return err
			}
			if !changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: nothing to change\n", clusterPath)
				return err
			}
			if err := config.WriteFileAtomic(clusterPath, out); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "grafana rootUrl set; run `oaf apply` to deploy\n")
			return err
		},
	}
	cmd.Flags().StringVarP(&clusterPath, "file", "f", DefaultClusterPath, "path to cluster.yaml")
	return cmd
}
