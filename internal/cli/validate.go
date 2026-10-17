package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/config"
)

// Default locations of the desired state (docs/adr/0001-stack.md, "State").
const (
	DefaultClusterPath = "/etc/oaf/cluster.yaml"
	DefaultCatalogPath = "/etc/oaf/catalog/models.yaml"
)

func newValidateCmd() *cobra.Command {
	var clusterPath, catalogPath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate cluster.yaml and the model catalog",
		Long: "Checks cluster.yaml and catalog/models.yaml against their schemas and the semantic rules\n" +
			"(topology, placement, memory budget, routing targets, licences). Exits non-zero on errors.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidate(cmd.OutOrStdout(), clusterPath, catalogPath)
		},
	}
	cmd.Flags().StringVarP(&clusterPath, "file", "f", DefaultClusterPath, "path to cluster.yaml")
	cmd.Flags().StringVar(&catalogPath, "catalog", DefaultCatalogPath, "path to the model catalog")
	return cmd
}

func runValidate(out io.Writer, clusterPath, catalogPath string) error {
	cat, err := config.LoadCatalog(catalogPath)
	if err != nil {
		return err
	}
	c, err := config.LoadCluster(clusterPath)
	if err != nil {
		return err
	}
	// Entitlement verification arrives later; until then no features are granted.
	r := config.Validate(c, cat, config.Options{})
	for _, i := range r.Errors() {
		fmt.Fprintf(out, "error:   %s\n", i)
	}
	for _, i := range r.Warnings() {
		fmt.Fprintf(out, "warning: %s\n", i)
	}
	if !r.OK() {
		return errors.New("validation failed")
	}
	_, err = fmt.Fprintf(out, "%s: valid (%d warning(s))\n", clusterPath, len(r.Warnings()))
	return err
}
