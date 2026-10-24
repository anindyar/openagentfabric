package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"openagentfabric.dev/oaf/internal/oafd"
	"openagentfabric.dev/oaf/internal/reconcile"
)

// Model phases, as the console shows them.
const (
	PhaseQueued   = "queued"   // in cluster.yaml, not started on its nodes yet
	PhaseStarting = "starting" // container up, nothing to report yet
	PhaseLoading  = "loading"  // weights loading, compiling, warming up
	PhaseReady    = "ready"    // healthy: requests are served
	PhaseFailed   = "failed"   // exited or crash-looping; Error says why
	PhaseRemoving = "removing" // removed from cluster.yaml, still on a node
	PhaseDisabled = "disabled"
)

var (
	// vLLM writes one of these when a run starts; what follows is that run.
	runStart = regexp.MustCompile(`non-default args|vLLM API server version`)
	shards   = regexp.MustCompile(`Loading safetensors checkpoint shards:\s+(\d+)%.*?(\d+)/(\d+)`)
	graphs   = regexp.MustCompile(`Capturing CUDA graphs.*?(\d+)%`)
	// An error line: "ValueError: ...", "RuntimeError: ...", "torch.OutOfMemoryError: ...".
	errLine = regexp.MustCompile(`\b([A-Za-z_.]*(?:Error|Exception)): (.+)$`)
	logHead = regexp.MustCompile(`^\([^)]*\)\s+(?:ERROR|WARNING|INFO)\s+\S+\s+\S+\s+\[[^\]]*\]\s*`)
)

// modelPhase reads a deployment's containers (one per node) into what the
// console shows. A multi-node model is as far along as its slowest node and
// failed when any node failed.
func modelPhase(dv *oafd.DeploymentView, sts []reconcile.ProjectState) {
	var parts []oafd.DeploymentView
	for _, st := range sts {
		p := nodePhase(st)
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		dv.Phase, dv.Detail = PhaseQueued, "Waiting for the change to be applied"
		return
	}
	rank := map[string]int{PhaseFailed: 0, PhaseQueued: 1, PhaseStarting: 2, PhaseLoading: 3, PhaseReady: 4}
	worst := parts[0]
	for _, p := range parts[1:] {
		if rank[p.Phase] < rank[worst.Phase] || p.Phase == worst.Phase && p.Progress < worst.Progress {
			worst = p
		}
	}
	dv.Phase, dv.Progress, dv.Detail, dv.Error, dv.Since = worst.Phase, worst.Progress, worst.Detail, worst.Error, worst.Since
	for _, p := range parts {
		dv.Restarts = max(dv.Restarts, p.Restarts)
	}
	if len(parts) > 1 && worst.Phase != PhaseReady && worst.Detail != "" {
		dv.Detail = worst.Name + ": " + worst.Detail
	}
}

// nodePhase is one node's part; Name carries the node.
func nodePhase(st reconcile.ProjectState) oafd.DeploymentView {
	p := oafd.DeploymentView{Name: st.Node}
	switch {
	case st.Err != nil:
		p.Phase, p.Error = PhaseFailed, st.Node+" is unreachable: "+st.Err.Error()
		return p
	case !st.Exists || len(st.Services) == 0:
		p.Phase, p.Detail = PhaseQueued, "Waiting for the change to be applied"
		return p
	}
	sv := st.Services[0]
	p.Since, p.Restarts = sv.GetStartedAt(), int(sv.GetRestartCount())
	run := currentRun(sv.GetLogTail())
	switch {
	case sv.GetState() == "running" && (sv.GetHealth() == "" || sv.GetHealth() == "healthy"):
		p.Phase, p.Progress = PhaseReady, 100
		return p
	case sv.GetState() == "exited" || sv.GetState() == "dead" || sv.GetState() == "restarting":
		p.Phase, p.Error = PhaseFailed, failure(st.Node, sv.GetLogTail())
		if p.Error == "" {
			p.Error = fmt.Sprintf("the model container on %s stopped (exit code %d)", st.Node, sv.GetExitCode())
		}
		return p
	}
	// Running, health still starting: a crash in this run means it is about
	// to restart (a crash loop), otherwise it is loading.
	if e := failure(st.Node, run); e != "" {
		p.Phase, p.Error = PhaseFailed, e
		return p
	}
	p.Phase, p.Progress, p.Detail = loadProgress(run)
	return p
}

// currentRun is the log after the last run start.
func currentRun(lines []string) []string {
	for i := len(lines) - 1; i >= 0; i-- {
		if runStart.MatchString(lines[i]) {
			return lines[i:]
		}
	}
	return lines
}

// loadProgress estimates how far a vLLM start is: weights are most of it,
// then compiling and CUDA graphs, then the API coming up.
func loadProgress(run []string) (phase string, progress int, detail string) {
	phase, progress, detail = PhaseStarting, 2, "Starting"
	for _, l := range run {
		switch {
		case strings.Contains(l, "Application startup complete"):
			phase, progress, detail = PhaseLoading, 98, "Serving, waiting for the health check"
		case graphs.MatchString(l):
			pct, _ := strconv.Atoi(graphs.FindStringSubmatch(l)[1])
			phase, progress, detail = PhaseLoading, 85+pct*10/100, fmt.Sprintf("Capturing CUDA graphs %d%%", pct)
		case strings.Contains(l, "Loading weights took") || strings.Contains(l, "Model loading took") || strings.Contains(l, "torch.compile"):
			phase, progress, detail = PhaseLoading, 80, "Weights loaded; compiling and warming up"
		case shards.MatchString(l):
			m := shards.FindStringSubmatch(l)
			pct, _ := strconv.Atoi(m[1])
			phase, progress, detail = PhaseLoading, 5+pct*75/100, fmt.Sprintf("Loading weights %s/%s", m[2], m[3])
		case strings.Contains(l, "Starting to load model") || strings.Contains(l, "Loading model from"):
			phase, progress, detail = PhaseLoading, 5, "Loading weights"
		}
	}
	return phase, progress, detail
}

// failure is the cause of a crash in log lines, said plainly when it is a
// known one, or "".
func failure(node string, lines []string) string {
	// A fatal start error, or a traceback near the end (vLLM also logs
	// harmless tracebacks early on, as warnings).
	crashed := false
	for i, l := range lines {
		if strings.Contains(l, "Engine core initialization failed") || strings.Contains(l, "EngineDeadError") ||
			strings.Contains(l, "Traceback (most recent call last)") && i >= len(lines)-60 {
			crashed = true
		}
	}
	if !crashed {
		return ""
	}
	cause := ""
	for i := len(lines) - 1; i >= 0 && cause == ""; i-- {
		l := logHead.ReplaceAllString(lines[i], "")
		if m := errLine.FindStringSubmatch(l); m != nil && !strings.Contains(m[2], "See root cause above") &&
			!strings.HasPrefix(m[2], "Worker failed with error") && !strings.Contains(l, "Engine core initialization failed") {
			cause = m[1] + ": " + m[2]
		}
	}
	all := strings.Join(lines, "\n")
	switch {
	case strings.Contains(all, "Invalid repository ID or local directory") || strings.Contains(all, "Weight files referenced in index but missing"):
		return "The model weights are not (all) on " + node + " yet; download them there first"
	case strings.Contains(cause, "Free memory on device"):
		return "Not enough free memory on " + node + ": " + clip(cause, 220)
	case strings.Contains(cause, "OutOfMemory"):
		return "Out of memory on " + node + ": lower its memory share or context"
	case cause == "":
		return "The model crashed on " + node + " while starting; see its log"
	}
	return clip(cause, 300) + " (on " + node + ")"
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
