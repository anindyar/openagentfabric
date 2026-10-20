package render

import (
	"encoding/json"
	"path"
)

// Grafana dashboards OAF provisions. They are OAF's own JSON, built here
// from a few panel helpers (Grafana is AGPL-3.0: nothing of its code is
// used, only its documented provisioning format). Queries use the metric and
// label names that vLLM, agentgateway and node_exporter export, checked
// against a live install.

const (
	dashboardsDir = "/etc/grafana/dashboards" // in the Grafana container
	homeDashboard = "oaf-overview.json"
)

type dashboard struct {
	uid, title string
	vars       []templateVar
	panels     []panel
}

type templateVar struct{ name, label, query string }

type panel struct {
	title, kind, unit string // kind: timeseries | stat | row
	w, h              int
	targets           []target
	max               *float64
}

type target struct{ expr, legend string }

func ts(title, unit string, w int, t ...target) panel {
	return panel{title: title, kind: "timeseries", unit: unit, w: w, h: 8, targets: t}
}

func stat(title, unit string, t target) panel {
	return panel{title: title, kind: "stat", unit: unit, w: 4, h: 4, targets: []target{t}}
}

func row(title string) panel { return panel{title: title, kind: "row", w: 24, h: 1} }

func q(expr, legend string) target { return target{expr: expr, legend: legend} }

func pct100() *float64 { v := 1.0; return &v }

// quantile is a histogram quantile over a rate, grouped by `by`.
func quantile(p, metric, sel, by string) string {
	return "histogram_quantile(" + p + ", sum by (le" + by + ") (rate(" + metric + "_bucket" + sel + "[$__rate_interval])))"
}

func oafDashboards() []dashboard {
	m := `{model=~"$model"}`
	n := `{node=~"$node"}`
	modelVar := templateVar{"model", "Model", "label_values(vllm:num_requests_running, model)"}
	nodeVar := templateVar{"node", "Node", "label_values(node_memory_MemTotal_bytes, node)"}
	kv := q(`max by (model) (vllm:kv_cache_usage_perc`+m+`)`, "{{model}}")
	kv100 := ts("KV cache usage", "percentunit", 8, kv)
	kv100.max = pct100()

	return []dashboard{
		{uid: "oaf-overview", title: "OAF overview", vars: []templateVar{modelVar, nodeVar}, panels: []panel{
			row("Traffic (gateway)"),
			stat("Requests/s", "reqps", q(`sum(rate(agentgateway_requests_total[$__rate_interval]))`, "")),
			stat("Errors (5xx) %", "percentunit", q(`(sum(rate(agentgateway_requests_total{status=~"5.."}[$__rate_interval])) or vector(0)) / clamp_min(sum(rate(agentgateway_requests_total[$__rate_interval])), 1e-9)`, "")),
			stat("Rejected (401) /s", "reqps", q(`sum(rate(agentgateway_requests_total{status="401"}[$__rate_interval])) or vector(0)`, "")),
			stat("Output tokens/s", "short", q(`sum(rate(vllm:generation_tokens_total`+m+`[$__rate_interval]))`, "")),
			stat("TTFT p95", "s", q(quantile("0.95", "vllm:time_to_first_token_seconds", m, ""), "")),
			stat("Running / waiting", "short", q(`sum(vllm:num_requests_running`+m+`) + sum(vllm:num_requests_waiting`+m+`)`, "")),
			row("Models"),
			ts("Tokens/s", "short", 8,
				q(`sum by (model) (rate(vllm:generation_tokens_total`+m+`[$__rate_interval]))`, "{{model}} output"),
				q(`sum by (model) (rate(vllm:prompt_tokens_total`+m+`[$__rate_interval]))`, "{{model}} prompt")),
			ts("Requests in the engine", "short", 8,
				q(`sum by (model) (vllm:num_requests_running`+m+`)`, "{{model}} running"),
				q(`sum by (model) (vllm:num_requests_waiting`+m+`)`, "{{model}} waiting")),
			kv100,
			row("Node"),
			ts("Unified memory", "bytes", 8,
				q(`node_memory_MemTotal_bytes`+n+` - node_memory_MemAvailable_bytes`+n, "{{node}} used"),
				q(`node_memory_MemAvailable_bytes`+n, "{{node}} available")),
			ts("CPU busy", "percentunit", 8, q(`1 - avg by (node) (rate(node_cpu_seconds_total{mode="idle",node=~"$node"}[$__rate_interval]))`, "{{node}}")),
			ts("Disk free (/)", "bytes", 8, q(`node_filesystem_avail_bytes{mountpoint="/",node=~"$node"}`, "{{node}}")),
		}},
		{uid: "oaf-models", title: "OAF models (vLLM)", vars: []templateVar{modelVar}, panels: []panel{
			row("Load"),
			ts("Requests in the engine", "short", 8,
				q(`sum by (model) (vllm:num_requests_running`+m+`)`, "{{model}} running"),
				q(`sum by (model) (vllm:num_requests_waiting`+m+`)`, "{{model}} waiting")),
			kv100,
			ts("Finished requests/s", "reqps", 8, q(`sum by (model, finished_reason) (rate(vllm:request_success_total`+m+`[$__rate_interval]))`, "{{model}} {{finished_reason}}")),
			row("Throughput"),
			ts("Output tokens/s", "short", 12, q(`sum by (model) (rate(vllm:generation_tokens_total`+m+`[$__rate_interval]))`, "{{model}}")),
			ts("Prompt tokens/s", "short", 12,
				q(`sum by (model) (rate(vllm:prompt_tokens_total`+m+`[$__rate_interval]))`, "{{model}} total"),
				q(`sum by (model) (rate(vllm:prompt_tokens_cached_total`+m+`[$__rate_interval]))`, "{{model}} from prefix cache")),
			row("Latency"),
			ts("Time to first token", "s", 8,
				q(quantile("0.5", "vllm:time_to_first_token_seconds", m, ", model"), "{{model}} p50"),
				q(quantile("0.95", "vllm:time_to_first_token_seconds", m, ", model"), "{{model}} p95")),
			ts("Inter-token latency", "s", 8,
				q(quantile("0.5", "vllm:inter_token_latency_seconds", m, ", model"), "{{model}} p50"),
				q(quantile("0.95", "vllm:inter_token_latency_seconds", m, ", model"), "{{model}} p95")),
			ts("End-to-end request latency", "s", 8,
				q(quantile("0.5", "vllm:e2e_request_latency_seconds", m, ", model"), "{{model}} p50"),
				q(quantile("0.95", "vllm:e2e_request_latency_seconds", m, ", model"), "{{model}} p95")),
			ts("Queue time p95", "s", 8, q(quantile("0.95", "vllm:request_queue_time_seconds", m, ", model"), "{{model}}")),
			ts("Prefill / decode time p95", "s", 8,
				q(quantile("0.95", "vllm:request_prefill_time_seconds", m, ", model"), "{{model}} prefill"),
				q(quantile("0.95", "vllm:request_decode_time_seconds", m, ", model"), "{{model}} decode")),
			ts("Request size p95 (tokens)", "short", 8,
				q(quantile("0.95", "vllm:request_prompt_tokens", m, ", model"), "{{model}} prompt"),
				q(quantile("0.95", "vllm:request_generation_tokens", m, ", model"), "{{model}} output")),
			row("Efficiency"),
			ts("Prefix cache hit rate", "percentunit", 8, q(`sum by (model) (rate(vllm:prefix_cache_hits_total`+m+`[$__rate_interval])) / clamp_min(sum by (model) (rate(vllm:prefix_cache_queries_total`+m+`[$__rate_interval])), 1e-9)`, "{{model}}")),
			ts("Speculative decoding acceptance", "percentunit", 8, q(`sum by (model) (rate(vllm:spec_decode_num_accepted_tokens_total`+m+`[$__rate_interval])) / clamp_min(sum by (model) (rate(vllm:spec_decode_num_draft_tokens_total`+m+`[$__rate_interval])), 1e-9)`, "{{model}}")),
			ts("Preemptions/s", "short", 8, q(`sum by (model) (rate(vllm:num_preemptions_total`+m+`[$__rate_interval]))`, "{{model}}")),
		}},
		{uid: "oaf-gateway", title: "OAF gateway", panels: []panel{
			row("Requests"),
			ts("Requests/s by status", "reqps", 12, q(`sum by (status) (rate(agentgateway_requests_total[$__rate_interval]))`, "{{status}}")),
			ts("Requests/s by backend", "reqps", 12, q(`sum by (backend) (rate(agentgateway_requests_total[$__rate_interval]))`, "{{backend}}")),
			ts("Request duration", "s", 12,
				q(quantile("0.5", "agentgateway_request_duration_seconds", "", ""), "p50"),
				q(quantile("0.95", "agentgateway_request_duration_seconds", "", ""), "p95")),
			ts("Rejected requests/s", "reqps", 12, q(`sum by (reason, status) (rate(agentgateway_requests_total{status=~"4..|5.."}[$__rate_interval]))`, "{{status}} {{reason}}")),
			row("LLM"),
			ts("Tokens/s by model", "short", 12, q(`sum by (gen_ai_request_model, gen_ai_token_type) (rate(agentgateway_gen_ai_client_token_usage_sum[$__rate_interval]))`, "{{gen_ai_request_model}} {{gen_ai_token_type}}")),
			ts("LLM call duration by model", "s", 12,
				q(quantile("0.5", "agentgateway_gen_ai_server_request_duration", "", ", gen_ai_request_model"), "{{gen_ai_request_model}} p50"),
				q(quantile("0.95", "agentgateway_gen_ai_server_request_duration", "", ", gen_ai_request_model"), "{{gen_ai_request_model}} p95")),
			row("Gateway process"),
			ts("Memory (RSS)", "bytes", 12, q(`agentgateway_process_rss`, "{{node}}")),
			ts("Connections/s", "short", 12, q(`sum by (node) (rate(agentgateway_downstream_connections_total[$__rate_interval]))`, "{{node}}")),
		}},
		{uid: "oaf-nodes", title: "OAF nodes", vars: []templateVar{nodeVar}, panels: []panel{
			row("Memory and CPU"),
			ts("Unified memory", "bytes", 12,
				q(`node_memory_MemTotal_bytes`+n+` - node_memory_MemAvailable_bytes`+n, "{{node}} used"),
				q(`node_memory_MemAvailable_bytes`+n, "{{node}} available")),
			ts("CPU busy", "percentunit", 12, q(`1 - avg by (node) (rate(node_cpu_seconds_total{mode="idle",node=~"$node"}[$__rate_interval]))`, "{{node}}")),
			ts("Load", "short", 12, q(`node_load1`+n, "{{node}} 1m"), q(`node_load15`+n, "{{node}} 15m")),
			ts("Swap used", "bytes", 12, q(`node_memory_SwapTotal_bytes`+n+` - node_memory_SwapFree_bytes`+n, "{{node}}")),
			row("Disk and network"),
			ts("Disk free", "bytes", 8, q(`node_filesystem_avail_bytes{fstype=~"ext4|xfs|btrfs",node=~"$node"}`, "{{node}} {{mountpoint}}")),
			ts("Network receive", "Bps", 8, q(`rate(node_network_receive_bytes_total{device!~"lo|docker.*|veth.*|br-.*",node=~"$node"}[$__rate_interval])`, "{{node}} {{device}}")),
			ts("Network transmit", "Bps", 8, q(`rate(node_network_transmit_bytes_total{device!~"lo|docker.*|veth.*|br-.*",node=~"$node"}[$__rate_interval])`, "{{node}} {{device}}")),
			ts("Temperatures (max per sensor chip)", "celsius", 24, q(`max by (node, chip) (node_hwmon_temp_celsius`+n+`)`, "{{node}} {{chip}}")),
		}},
	}
}

// json renders the dashboard in Grafana's schema with a 24-column grid
// layout: panels flow left to right and wrap.
func (d dashboard) json() ([]byte, error) {
	ds := map[string]string{"type": "prometheus", "uid": "oaf-prometheus"}
	var panels []map[string]any
	x, y, rowH := 0, 0, 0
	for i, p := range d.panels {
		if x+p.w > 24 || p.kind == "row" && x > 0 {
			x, y, rowH = 0, y+rowH, 0
		}
		jp := map[string]any{
			"id": i + 1, "title": p.title, "type": p.kind,
			"gridPos": map[string]int{"x": x, "y": y, "w": p.w, "h": p.h},
		}
		if p.kind != "row" {
			var ts []map[string]any
			for j, t := range p.targets {
				ts = append(ts, map[string]any{"refId": string(rune('A' + j)), "expr": t.expr, "legendFormat": t.legend, "datasource": ds})
			}
			defaults := map[string]any{"unit": p.unit}
			if p.max != nil {
				defaults["min"], defaults["max"] = 0, *p.max
			}
			jp["datasource"] = ds
			jp["targets"] = ts
			jp["fieldConfig"] = map[string]any{"defaults": defaults, "overrides": []any{}}
			if p.kind == "stat" {
				jp["options"] = map[string]any{"reduceOptions": map[string]any{"calcs": []string{"lastNotNull"}}, "graphMode": "area"}
			} else {
				jp["options"] = map[string]any{"legend": map[string]any{"displayMode": "list", "placement": "bottom"}, "tooltip": map[string]any{"mode": "multi"}}
			}
		} else {
			jp["collapsed"] = false
			jp["panels"] = []any{}
		}
		panels = append(panels, jp)
		x += p.w
		rowH = max(rowH, p.h)
	}
	var vars []map[string]any
	for _, v := range d.vars {
		vars = append(vars, map[string]any{
			"name": v.name, "label": v.label, "type": "query", "datasource": ds,
			"query": map[string]any{"query": v.query, "refId": "vars"}, "definition": v.query,
			"refresh": 2, "includeAll": true, "multi": true, "allValue": ".*",
			"current": map[string]any{"text": "All", "value": "$__all"},
		})
	}
	links := []map[string]any{{"title": "OAF", "type": "dashboards", "tags": []string{"oaf"}, "asDropdown": true, "includeVars": true, "keepTime": true}}
	return json.MarshalIndent(map[string]any{
		"uid": d.uid, "title": d.title, "tags": []string{"oaf"}, "editable": false,
		"schemaVersion": 39, "refresh": "30s", "timezone": "browser",
		"time":       map[string]string{"from": "now-3h", "to": "now"},
		"templating": map[string]any{"list": vars},
		"links":      links,
		"panels":     panels,
	}, "", "  ")
}

// dashboardFiles renders the provisioning config and one JSON file per
// dashboard, under grafana/ in the metrics project.
func dashboardFiles(bundleVersion string) ([]File, error) {
	prov, err := yamlFile(bundleVersion, "grafana/dashboards.yaml", map[string]any{
		"apiVersion": 1,
		"providers": []map[string]any{{
			"name": "oaf", "folder": "OAF", "type": "file", "disableDeletion": true,
			"allowUiUpdates": false, "updateIntervalSeconds": 30,
			"options": map[string]string{"path": dashboardsDir},
		}},
	})
	if err != nil {
		return nil, err
	}
	files := []File{prov}
	for _, d := range oafDashboards() {
		b, err := d.json()
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: path.Join("grafana/dashboards", d.uid+".json"), Mode: 0o644, Content: append(b, '\n')})
	}
	return files, nil
}
