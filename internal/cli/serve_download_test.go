package cli

import (
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/oafd"
	"openagentfabric.dev/oaf/internal/reconcile"
)

// A model whose weights are downloading shows the download, whether its
// change is applied (project written, waiting) or not yet.
func TestModelPhaseDownloading(t *testing.T) {
	dl := &oafd.DownloadView{Node: "spark-1", ModelID: "m", State: "downloading", BytesDone: 18e9, BytesTotal: 47e9, BytesPerSecond: 32e6}
	find := func(string) *oafd.DownloadView { return dl }
	waiting := reconcile.ProjectState{Node: "spark-1", Project: "model-m", Exists: true}
	var dv oafd.DeploymentView
	modelPhase(&dv, []reconcile.ProjectState{waiting}, find)
	if dv.Phase != PhaseDownloading || dv.Progress != 38 || !strings.HasPrefix(dv.Detail, "Downloading 18.0 GB of 47.0 GB · 32 MB/s · 15 min left") {
		t.Fatalf("waiting for weights: %+v", dv)
	}
	dv = oafd.DeploymentView{Nodes: []string{"spark-1"}}
	modelPhase(&dv, nil, find)
	if dv.Phase != PhaseDownloading {
		t.Fatalf("not applied yet, downloading: %+v", dv)
	}
	dl = &oafd.DownloadView{Node: "spark-1", ModelID: "m", State: "failed", Error: "not enough disk space"}
	dv = oafd.DeploymentView{}
	modelPhase(&dv, []reconcile.ProjectState{waiting}, find)
	if dv.Phase != PhaseFailed || !strings.Contains(dv.Error, "not enough disk space") {
		t.Fatalf("download failed: %+v", dv)
	}
	// Replaced by a model still downloading: the previous one serves.
	dl = &oafd.DownloadView{Node: "spark-1", ModelID: "m", State: "downloading", BytesDone: 1, BytesTotal: 4}
	dv = oafd.DeploymentView{}
	modelPhase(&dv, []reconcile.ProjectState{running("healthy", 0)}, find)
	if dv.Phase != PhaseReady || !strings.HasPrefix(dv.Detail, "Serving the previous model; switches when the new one has downloaded (25%)") {
		t.Fatalf("replacement downloading: %+v", dv)
	}
	dl = &oafd.DownloadView{Node: "spark-1", ModelID: "m", State: "complete"}
	dv = oafd.DeploymentView{}
	modelPhase(&dv, []reconcile.ProjectState{waiting}, find)
	if dv.Phase != PhaseQueued || dv.Detail != "Waiting to start" {
		t.Fatalf("downloaded, not started yet: %+v", dv)
	}
}
