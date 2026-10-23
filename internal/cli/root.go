// Package cli wires the oaf command tree.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/version"
)

// NewRoot returns the root `oaf` command.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "oaf",
		Short:         "openagentfabric: install and run an LLM platform on a GB10 cluster",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newKeysCmd(), newModelsCmd(), newObservabilityCmd(), newVersionCmd(), newValidateCmd(), newPreflightCmd(), newInstallCmd(), newApplyCmd(), newStatusCmd(),
		newUninstallCmd(), newFabricCmd(), newNodeCmd(), newPKICmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the oaf version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return err
		},
	}
}
