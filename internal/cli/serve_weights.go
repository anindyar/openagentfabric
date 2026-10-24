package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"openagentfabric.dev/oaf/internal/config"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/oafd"
	"openagentfabric.dev/oaf/internal/reconcile"
	"openagentfabric.dev/oaf/internal/render"
)

// prefetch starts downloading a deployment's weights on each of its nodes
// (oafd calls it when a model is chosen, before its plan is approved).
func prefetch(ctx context.Context, f *stackFlags, c *config.Cluster, d config.ModelDeployment, log *slog.Logger) {
	s, err := f.load(io.Discard)
	if err != nil {
		log.Warn("weights download not started", "model", d.Name, "err", err)
		return
	}
	for node, w := range render.ModelWeights(c, s.catalog, d) {
		cl, ok := s.nodes[node]
		if !ok {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp, err := cl.PullWeights(cctx, &nodev1.PullWeightsRequest{Weights: reconcile.WeightsSpec(w)})
		cancel()
		if err != nil {
			log.Warn("weights download not started", "model", d.Name, "node", node, "err", err)
			continue
		}
		log.Info("weights", "model", w.ModelID, "node", node, "state", resp.GetStatus().GetState())
	}
}

// downloads asks every node for its weight downloads.
func downloads(ctx context.Context, ns reconcile.Nodes) []oafd.DownloadView {
	out := []oafd.DownloadView{}
	for name, cl := range ns {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := cl.Weights(cctx, &nodev1.WeightsRequest{})
		cancel()
		if err != nil {
			continue // an older agent, or the node is down (reported elsewhere)
		}
		for _, w := range resp.GetWeights() {
			out = append(out, oafd.DownloadView{Node: name, ModelID: w.GetModelId(), State: w.GetState(), BytesDone: w.GetBytesDone(),
				BytesTotal: w.GetBytesTotal(), BytesPerSecond: w.GetBytesPerSecond(), Error: w.GetError(), StartedAt: w.GetStartedAt()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelID != out[j].ModelID {
			return out[i].ModelID < out[j].ModelID
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// downloadPhase is what a model waiting for its weights on a node shows;
// ok is false when nothing is downloading there.
func downloadPhase(node string, dl *oafd.DownloadView) (p oafd.DeploymentView, ok bool) {
	if dl == nil {
		return p, false
	}
	p.Name = node
	switch dl.State {
	case "downloading":
		p.Phase = PhaseDownloading
		if dl.BytesTotal > 0 {
			p.Progress = int(dl.BytesDone * 100 / dl.BytesTotal)
		}
		p.Detail = fmt.Sprintf("Downloading %s of %s", gbs(dl.BytesDone), gbs(dl.BytesTotal))
		if dl.BytesPerSecond > 0 {
			left := time.Duration(float64(dl.BytesTotal-dl.BytesDone)/dl.BytesPerSecond) * time.Second
			p.Detail += fmt.Sprintf(" · %.0f MB/s · %s left", dl.BytesPerSecond/1e6, roundLeft(left))
		}
		p.Since = dl.StartedAt
		return p, true
	case "failed":
		p.Phase, p.Error = PhaseFailed, "Downloading the weights on "+node+" failed: "+dl.Error
		return p, true
	}
	return p, false
}

func gbs(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

func roundLeft(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()+0.5))
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
