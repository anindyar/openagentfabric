package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/secrets"
)

func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "secrets", Short: "Manage secrets OAF cannot generate (external provider API keys)"}
	cmd.AddCommand(newSecretsSetCmd())
	return cmd
}

func newSecretsSetCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret read from stdin, e.g. an external provider's API key",
		Long: "Reads one line from stdin and stores it as secret <name> (mode 0600), replacing any value.\n" +
			"Use it for values OAF never generates, such as spec.routing.providers[].secretRef:\n" +
			"  printf %s \"$OPENAI_API_KEY\" | sudo oaf secrets set provider-openai\n" +
			"Run `oaf apply` afterwards.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			v := strings.TrimSpace(line)
			if v == "" {
				return errors.New("no value on stdin")
			}
			if err := (secrets.Store{Dir: dir}).Set(args[0], v); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "stored %s; run `oaf apply` to use it\n", args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "secrets-dir", secrets.DefaultDir, "directory of secrets")
	return cmd
}
