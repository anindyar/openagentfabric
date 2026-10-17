// Command oaf is the single OAF binary: CLI, control plane (oafd) and node agent (oaf-node).
package main

import (
	"fmt"
	"os"

	"openagentfabric.dev/oaf/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
