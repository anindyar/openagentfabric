package cli

import (
	"strings"
	"testing"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/oafd"
	"openagentfabric.dev/oaf/internal/reconcile"
)

func running(health string, restarts int32, log ...string) reconcile.ProjectState {
	return reconcile.ProjectState{Node: "spark-1", Project: "model-m", Exists: true, Services: []*nodev1.Service{{
		Name: "vllm", State: "running", Health: health, StartedAt: "2026-10-25T00:00:00Z", RestartCount: restarts, LogTail: log}}}
}

// Log lines as vLLM writes them on a GB10 (shortened).
var (
	logStart    = "(APIServer pid=1) INFO 10-24 22:23:36 [api_utils.py:286] non-default args: {'model_tag': '/models/m'}"
	logShards   = "(Worker_TP0 pid=357) Loading safetensors checkpoint shards:  61% Completed | 20/33 [06:50<04:33, 21.06s/it]"
	logLoaded   = "(EngineCore pid=159) INFO 10-24 20:24:36 [default_loader.py:484] Loading weights took 169.60 seconds"
	logGraphs   = "Capturing CUDA graphs (PIECEWISE):  43%|████▎     | 3/7 [00:21<02:09, 21.52s/it]"
	logTrace    = "Traceback (most recent call last):"
	logMemory   = "(EngineCore pid=269) ERROR 10-24 22:23:50 [core.py:1433] ValueError: Free memory on device cuda:0 (31.63/121.69 GiB) on startup is less than desired GPU memory utilization (0.5, 60.84 GiB)."
	logEngine   = "RuntimeError: Engine core initialization failed. See root cause above. Failed core proc(s): {}"
	logNoWeight = "  Value error, Invalid repository ID or local directory specified: '/models/m'."
)

func phaseOf(sts ...reconcile.ProjectState) oafd.DeploymentView {
	var dv oafd.DeploymentView
	modelPhase(&dv, sts, nil)
	return dv
}

func TestModelPhase(t *testing.T) {
	if p := phaseOf(); p.Phase != PhaseQueued {
		t.Fatalf("nothing on the nodes yet: %+v", p)
	}
	if p := phaseOf(reconcile.ProjectState{Node: "spark-1", Project: "model-m"}); p.Phase != PhaseQueued {
		t.Fatalf("project not created yet: %+v", p)
	}
	if p := phaseOf(running("starting", 0, logStart)); p.Phase != PhaseStarting {
		t.Fatalf("just started: %+v", p)
	}
	p := phaseOf(running("starting", 0, logStart, logShards))
	if p.Phase != PhaseLoading || p.Detail != "Loading weights 20/33" || p.Progress < 40 || p.Progress > 60 || p.Since == "" {
		t.Fatalf("loading weights: %+v", p)
	}
	if p := phaseOf(running("starting", 0, logStart, logShards, logLoaded, logGraphs)); p.Phase != PhaseLoading || !strings.HasPrefix(p.Detail, "Capturing CUDA graphs 43%") || p.Progress < 85 {
		t.Fatalf("warming up: %+v", p)
	}
	if p := phaseOf(running("healthy", 0)); p.Phase != PhaseReady || p.Progress != 100 {
		t.Fatalf("ready: %+v", p)
	}
	// Crash loop: the run failed and the container is about to restart.
	p = phaseOf(running("starting", 22, logStart, logTrace, logMemory, logEngine))
	if p.Phase != PhaseFailed || !strings.Contains(p.Error, "Not enough free memory on spark-1") || p.Restarts != 22 {
		t.Fatalf("crash loop: %+v", p)
	}
	// An earlier crash, then a new run that is loading: loading.
	if p := phaseOf(running("starting", 1, logStart, logTrace, logMemory, logEngine, logStart, logShards)); p.Phase != PhaseLoading ||
		!strings.HasPrefix(p.Error, "The previous start failed: Not enough free memory") {
		t.Fatalf("restarted after a crash: %+v", p)
	}
	exited := reconcile.ProjectState{Node: "spark-1", Project: "model-m", Exists: true, Services: []*nodev1.Service{{
		Name: "vllm", State: "restarting", LogTail: []string{logStart, logTrace, logNoWeight, logEngine}}}}
	if p := phaseOf(exited); p.Phase != PhaseFailed || !strings.Contains(p.Error, "weights are not (all) on spark-1") {
		t.Fatalf("no weights: %+v", p)
	}
	// The root cause, not the wrappers around it (GLM on the published build).
	glm := running("starting", 3, logStart, logTrace,
		"(Worker_TP0 pid=378) ERROR 10-24 15:18:10 [multiproc_executor.py:944] ValueError: No valid attention backend found for cuda with AttentionSelectorConfig(head_size=512)",
		"(EngineCore pid=300) ERROR 10-24 15:18:12 [core.py:1366] Exception: WorkerProc initialization failed due to an exception in a background process. See stack trace for root cause.",
		logEngine)
	if p := phaseOf(glm); p.Phase != PhaseFailed || !strings.HasPrefix(p.Error, "ValueError: No valid attention backend") {
		t.Fatalf("root cause: %+v", p)
	}
	quoted := running("starting", 3, logStart, logTrace,
		"(EngineCore pid=280) RuntimeError: Worker failed with error 'concat_and_cache_mla, pe_dim must be 64 for fp8_ds_mla', please check the stack trace above for the root cause",
		logEngine)
	if p := phaseOf(quoted); !strings.HasPrefix(p.Error, "concat_and_cache_mla, pe_dim must be 64") {
		t.Fatalf("quoted worker error: %+v", p)
	}
	// A pair: as far as the slower node; failed when one node failed.
	other := running("starting", 0, logStart)
	other.Node = "spark-2"
	if p := phaseOf(running("starting", 0, logStart, logShards), other); p.Phase != PhaseStarting || !strings.HasPrefix(p.Detail, "spark-2: ") {
		t.Fatalf("pair: %+v", p)
	}
	if p := phaseOf(running("healthy", 0), exited); p.Phase != PhaseFailed {
		t.Fatalf("pair with a failed node: %+v", p)
	}
}
