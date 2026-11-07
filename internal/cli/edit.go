package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"openagentfabric.dev/oaf/internal/config"
)

// editCluster is how the CLI changes cluster.yaml for installers: edit the
// document (comments kept), validate the result against the catalog (memory
// budget, routing targets, licences), print warnings, and write it only if
// it has no errors. extra adds checks of the edited cluster (e.g. adapter
// files). It reports whether the file changed; nothing is applied.
func editCluster(out io.Writer, clusterPath, catalogPath string,
	edit func([]byte) ([]byte, bool, error), extra func(*config.Cluster, *config.Catalog) []config.Issue,
) (bool, error) {
	cat, err := config.LoadCatalog(catalogPath)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(clusterPath)
	if err != nil {
		return false, err
	}
	next, changed, err := edit(data)
	if err != nil || !changed {
		return false, err
	}
	c, err := config.ParseCluster(next)
	if err != nil {
		return false, err
	}
	r := config.Validate(c, cat, config.Options{})
	if extra != nil {
		r.Issues = append(r.Issues, extra(c, cat)...)
	}
	for _, w := range r.Warnings() {
		_, _ = fmt.Fprintf(out, "warning: %s\n", w)
	}
	if !r.OK() {
		for _, e := range r.Errors() {
			_, _ = fmt.Fprintf(out, "error:   %s\n", e)
		}
		return false, errors.New("cluster.yaml not changed: the result would not validate")
	}
	return true, config.WriteFileAtomic(clusterPath, next)
}

// splitList parses "a, b,c" into [a b c], dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
