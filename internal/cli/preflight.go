package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/preflight"
)

func newPreflightCmd() *cobra.Command {
	var (
		sshTarget, record string
		asJSON            bool
		opts              = preflight.Defaults
	)
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Inspect a node before install (read-only)",
		Long: "Checks OS, GPU, memory, disk, docker, the NVIDIA runtime, existing workloads, ports,\n" +
			"QSFP fabric, RDMA tools, NCCL and time sync. It only runs an allowlist of read-only\n" +
			"commands, so it is safe on production nodes. Exits non-zero if any check fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var r hostexec.Runner = hostexec.Local{}
			if sshTarget != "" {
				r = hostexec.SSH{Target: sshTarget, ControlPath: filepath.Join(os.TempDir(), "oaf-ssh-%C")}
			}
			var rec *hostexec.Recorder
			if record != "" {
				rec = &hostexec.Recorder{Inner: r}
				r = rec
			}
			rep := preflight.Run(cmd.Context(), r, opts)
			if rec != nil {
				if err := rec.Save(record); err != nil {
					return err
				}
			}
			if err := printReport(cmd.OutOrStdout(), rep, asJSON); err != nil {
				return err
			}
			if rep.Worst() == preflight.Fail {
				return errors.New("preflight failed")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&sshTarget, "ssh", "", "inspect a remote node over SSH (user@host)")
	f.BoolVar(&asJSON, "json", false, "print the report as JSON")
	f.StringVar(&opts.DataPath, "data-path", opts.DataPath, "where OAF will store models and data")
	f.Float64Var(&opts.MinFreeDiskGiB, "min-free-disk-gib", opts.MinFreeDiskGiB, "warn below this much free disk")
	f.Float64Var(&opts.MinAvailableMemGiB, "min-available-mem-gib", opts.MinAvailableMemGiB, "warn below this much available memory")
	f.StringVar(&record, "record", "", "save every command result to this JSON file (test fixtures)")
	_ = f.MarkHidden("record")
	return cmd
}

var statusLabel = map[preflight.Status]string{
	preflight.Pass: "PASS",
	preflight.Info: "INFO",
	preflight.Warn: "WARN",
	preflight.Fail: "FAIL",
}

func printReport(out io.Writer, rep preflight.Report, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Fprintf(out, "preflight: %s\n\n", rep.Host)
	for _, r := range rep.Results {
		fmt.Fprintf(out, "%-4s  %-15s %s\n", statusLabel[r.Status], r.ID, r.Summary)
		for _, d := range r.Details {
			fmt.Fprintf(out, "      %-15s %s\n", "", d)
		}
	}
	fmt.Fprintf(out, "\nresult: %s\n", statusLabel[rep.Worst()])
	return nil
}
