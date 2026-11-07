package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/bundle"
	"openagentfabric.dev/oaf/internal/config"
)

func newLicensesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "licenses", Aliases: []string{"licences"}, Short: "Show and record the licences cluster.yaml needs accepted"}
	cmd.AddCommand(newLicensesNeededCmd(), newLicensesAcceptCmd())
	return cmd
}

func newLicensesNeededCmd() *cobra.Command {
	var f clusterFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "needed",
		Short: "List the licences the enabled deployments need and cluster.yaml does not record",
		Long: "Engine images with NVIDIA terms (image/<component>) and gated or non-open models\n" +
			"(model/<catalog id>). Show each to the person installing; record their acceptance with\n" +
			"`oaf licenses accept`. A model's licence must be accepted before `oaf models add` takes it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, cat, v, err := loadForLicences(f)
			if err != nil {
				return err
			}
			need := config.NeededAcceptances(c, cat, v)
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"licenses_needed": need})
			}
			if len(need) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "no licences needed")
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "SUBJECT\tTERMS\tFOR\tURL")
			for _, n := range need {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n.Subject, n.Title, n.For, n.URL)
			}
			return tw.Flush()
		},
	}
	f.register(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newLicensesAcceptCmd() *cobra.Command {
	var f clusterFlags
	var by string
	cmd := &cobra.Command{
		Use:   "accept <subject>... --by <who>",
		Short: "Record that a person read and accepted licences (idempotent)",
		Long: "Adds each subject (image/<component> or model/<catalog id>) to spec.licenseAcceptances with\n" +
			"its terms URL, who accepted and when. Only record what a person actually accepted, e.g.\n" +
			"in an installer's licence screen. Subjects already recorded are left alone.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(by) == "" {
				return errors.New("--by (who accepted) is required")
			}
			_, cat, v, err := loadForLicences(f)
			if err != nil {
				return err
			}
			urls := map[string]string{}
			for _, sub := range args {
				u, ok := config.AcceptableURL(sub, cat, v)
				if !ok {
					return fmt.Errorf("%q is not a licence this bundle or catalog asks to accept", sub)
				}
				urls[sub] = u
			}
			data, err := os.ReadFile(f.cluster)
			if err != nil {
				return err
			}
			out, added, err := config.WithAcceptances(data, args, urls, strings.TrimSpace(by), time.Now().UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			if len(added) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "already accepted")
				return err
			}
			if err := config.WriteFileAtomic(f.cluster, out); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "accepted %s\n", strings.Join(added, ", "))
			return err
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&by, "by", "", "who accepted (e.g. their email); recorded in cluster.yaml (required)")
	return cmd
}

func loadForLicences(f clusterFlags) (*config.Cluster, *config.Catalog, *config.Versions, error) {
	c, err := config.LoadCluster(f.cluster)
	if err != nil {
		return nil, nil, nil, err
	}
	cat, err := config.LoadCatalog(f.catalog)
	if err != nil {
		return nil, nil, nil, err
	}
	v, err := config.ParseVersions(bundle.VersionsYAML)
	if err != nil {
		return nil, nil, nil, err
	}
	return c, cat, v, nil
}
