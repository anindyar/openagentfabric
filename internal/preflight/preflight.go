// Package preflight inspects a node before OAF installs anything on it.
//
// Every check runs through a hostexec.ReadOnly runner, so preflight is safe
// on production boxes: it can observe, never change. It reports what OAF
// would collide with (existing inference containers, memory pressure, busy
// ports, configured QSFP ports) rather than taking anything over.
package preflight

import (
	"context"
	"sort"

	"openagentfabric.dev/oaf/internal/fabric"
	"openagentfabric.dev/oaf/internal/hostexec"
)

// Status of one check.
type Status string

// Statuses, from best to worst.
const (
	Pass Status = "pass"
	Info Status = "info"
	Warn Status = "warn"
	Fail Status = "fail"
)

func (s Status) rank() int {
	switch s {
	case Fail:
		return 3
	case Warn:
		return 2
	case Info:
		return 1
	}
	return 0
}

// Result of one check.
type Result struct {
	ID      string   `json:"id"`
	Status  Status   `json:"status"`
	Summary string   `json:"summary"`
	Details []string `json:"details,omitempty"`
}

// Report is the outcome of a preflight run.
type Report struct {
	Host    string        `json:"host"`
	Results []Result      `json:"results"`
	Ports   []fabric.Port `json:"ports,omitempty"`
}

// Worst returns the worst status in the report.
func (r Report) Worst() Status {
	w := Pass
	for _, res := range r.Results {
		if res.Status.rank() > w.rank() {
			w = res.Status
		}
	}
	return w
}

// Options tune the checks.
type Options struct {
	// DataPath is where OAF will keep models and data (disk check).
	DataPath string
	// MinFreeDiskGiB below this is a warning.
	MinFreeDiskGiB float64
	// MinAvailableMemGiB below this is a warning.
	MinAvailableMemGiB float64
}

// Defaults for Options.
var Defaults = Options{DataPath: "/var/lib/oaf", MinFreeDiskGiB: 200, MinAvailableMemGiB: 24}

type check struct {
	id  string
	run func(context.Context, *env) Result
}

type env struct {
	r     hostexec.Runner
	opts  Options
	ports []fabric.Port
}

// Run executes every check. The runner is wrapped read-only.
func Run(ctx context.Context, r hostexec.Runner, opts Options) Report {
	ro := hostexec.ReadOnly{Inner: r}
	e := &env{r: ro, opts: opts}
	rep := Report{Host: hostname(ctx, ro)}
	for _, c := range checks {
		res := c.run(ctx, e)
		res.ID = c.id
		rep.Results = append(rep.Results, res)
	}
	rep.Ports = e.ports
	sort.SliceStable(rep.Results, func(i, j int) bool { return order(rep.Results[i].ID) < order(rep.Results[j].ID) })
	return rep
}

func order(id string) int {
	for i, c := range checks {
		if c.id == id {
			return i
		}
	}
	return len(checks)
}

func hostname(ctx context.Context, r hostexec.Runner) string {
	out, err := r.Run(ctx, "cat", "/etc/hostname")
	if err != nil {
		return "unknown"
	}
	return trim(out)
}
