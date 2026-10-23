package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
)

func TestObservabilityEnable(t *testing.T) {
	data, err := os.ReadFile("../../examples/cluster.single.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cluster := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(cluster, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "observability", "enable", "logs", "remote-write", "anonymous-grafana", "-f", cluster)
	if err != nil || !strings.Contains(out, "enabled") {
		t.Fatalf("enable: %v\n%s", err, out)
	}
	c, err := config.LoadCluster(cluster)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Spec.Observability
	if !o.LogsEnabled() || o.Metrics == nil || !o.Metrics.RemoteWrite || o.Metrics.Retention != "15d" || o.Grafana == nil || !o.Grafana.AnonymousViewer {
		t.Fatalf("observability = %+v", o)
	}
	after, _ := os.ReadFile(cluster)
	if !strings.Contains(string(after), "licenseAcceptances") {
		t.Fatal("the rest of cluster.yaml was lost")
	}
	if out, err = runCLI(t, "observability", "enable", "logs", "-f", cluster); err != nil || !strings.Contains(out, "nothing to change") {
		t.Fatalf("second enable must be a no-op: %v\n%s", err, out)
	}
	if out, err = runCLI(t, "observability", "disable", "anonymous-grafana", "-f", cluster); err != nil {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	if c, _ = config.LoadCluster(cluster); c.Spec.Observability.Grafana.AnonymousViewer {
		t.Fatal("anonymous viewer still on")
	}
	for i := 0; i < 2; i++ {
		if out, err = runCLI(t, "observability", "scrape", "garrison", "https://10.0.0.5:8080/metrics", "--insecure", "-f", cluster); err != nil {
			t.Fatalf("scrape: %v\n%s", err, out)
		}
	}
	if !strings.Contains(out, "already scrapes") {
		t.Fatalf("second scrape must be a no-op:\n%s", out)
	}
	if out, err = runCLI(t, "observability", "scrape", "garrison", "https://10.0.0.6:8080/metrics", "-f", cluster); err != nil {
		t.Fatalf("scrape replace: %v\n%s", err, out)
	}
	c, _ = config.LoadCluster(cluster)
	if s := c.Spec.Observability.Metrics.Scrape; len(s) != 1 || s[0].URL != "https://10.0.0.6:8080/metrics" || s[0].InsecureSkipVerify {
		t.Fatalf("scrape = %+v", s)
	}
	if _, err = runCLI(t, "observability", "enable", "tracing", "-f", cluster); err == nil {
		t.Fatal("unknown feature accepted")
	}
}
