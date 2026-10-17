// Package version holds build metadata, set at link time via -ldflags.
package version

import "fmt"

// Build metadata. The Makefile overrides these with -X.
var (
	// Version is the release version (git describe).
	Version = "dev"
	// Commit is the short git commit.
	Commit = "unknown"
	// Date is the UTC build time.
	Date = "unknown"
)

// String returns a one-line human-readable version.
func String() string {
	return fmt.Sprintf("oaf %s (commit %s, built %s)", Version, Commit, Date)
}
