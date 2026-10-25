package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strconv"
	"strings"

	"openagentfabric.dev/oaf/internal/config"
)

const langfuseBucket = "langfuse"

// langfuse renders Langfuse (web + worker) and its stores: Postgres,
// ClickHouse, Valkey (Redis-compatible queue) and SeaweedFS (S3 for events
// and media). Only Langfuse web is published; the stores are reachable on the
// project network only.
func (r *renderer) langfuse(n config.Node) (Project, error) {
	p := Project{Node: n.Name, Name: ProjectLangfuse}
	v := r.Versions.Version
	env := newEnvFile()
	sec := map[string]string{}
	for _, name := range []string{SecretPostgres, SecretClickHouse, SecretValkey, SecretS3AccessKey, SecretS3SecretKey,
		SecretLangfuseSalt, SecretLangfuseEncrypt, SecretLangfuseNextAuth, SecretLangfusePublicKey,
		SecretLangfuseSecretKey, SecretLangfuseAdmin} {
		val, err := r.secret(name)
		if err != nil {
			return p, err
		}
		sec[name] = val
	}
	pgPass := env.ref("POSTGRES_PASSWORD", sec[SecretPostgres])
	chPass := env.ref("CLICKHOUSE_PASSWORD", sec[SecretClickHouse])
	vkPass := env.ref("VALKEY_PASSWORD", sec[SecretValkey])
	s3Key := env.ref("S3_ACCESS_KEY", sec[SecretS3AccessKey])
	s3Secret := env.ref("S3_SECRET_KEY", sec[SecretS3SecretKey])

	img := map[string]string{}
	for _, c := range []string{"postgres", "clickhouse", "valkey", "seaweedfs", "langfuse-web", "langfuse-worker"} {
		i, err := r.image(c)
		if err != nil {
			return p, err
		}
		img[c] = i
	}

	postgres := newService(img["postgres"])
	postgres.Environment = map[string]string{
		"POSTGRES_USER":     "langfuse",
		"POSTGRES_PASSWORD": pgPass,
		"POSTGRES_DB":       "langfuse",
		"TZ":                "UTC",
		"PGTZ":              "UTC",
	}
	postgres.Volumes = []string{"postgres-data:/var/lib/postgresql/data"}
	postgres.Healthcheck = &healthcheck{Test: []string{"CMD-SHELL", "pg_isready -U langfuse -d langfuse"}, Interval: "5s", Timeout: "5s", Retries: 20}

	clickhouse := newService(img["clickhouse"])
	clickhouse.User = "101:101"
	clickhouse.Environment = map[string]string{
		"CLICKHOUSE_DB":       "default",
		"CLICKHOUSE_USER":     "clickhouse",
		"CLICKHOUSE_PASSWORD": chPass,
	}
	clickhouse.Volumes = []string{"clickhouse-data:/var/lib/clickhouse", "clickhouse-logs:/var/log/clickhouse-server"}
	clickhouse.Ulimits = map[string]int{"nofile": 262144}
	clickhouse.Healthcheck = &healthcheck{Test: []string{"CMD-SHELL", "wget --no-verbose --tries=1 --spider http://127.0.0.1:8123/ping || exit 1"}, Interval: "5s", Timeout: "5s", Retries: 20}

	// noeviction: Langfuse keeps its queues in Valkey and must not lose jobs.
	valkey := newService(img["valkey"])
	valkey.Command = []string{"valkey-server", "--requirepass", vkPass, "--maxmemory", strconv.Itoa(valkeyMaxMemoryMiB) + "mb", "--maxmemory-policy", "noeviction", "--appendonly", "yes"}
	valkey.Environment = map[string]string{"REDISCLI_AUTH": vkPass}
	valkey.Volumes = []string{"valkey-data:/data"}
	valkey.Healthcheck = &healthcheck{Test: []string{"CMD-SHELL", "valkey-cli ping | grep -q PONG"}, Interval: "5s", Timeout: "5s", Retries: 20}

	// `weed mini` is single-node SeaweedFS with S3; it creates the credentials
	// and the bucket from these variables on startup.
	seaweed := newService(img["seaweedfs"])
	seaweed.Command = []string{"mini", "-dir=/data"}
	seaweed.Environment = map[string]string{
		"AWS_ACCESS_KEY_ID":     s3Key,
		"AWS_SECRET_ACCESS_KEY": s3Secret,
		"S3_BUCKET":             langfuseBucket,
	}
	seaweed.Volumes = []string{"seaweedfs-data:/data"}
	seaweed.Healthcheck = &healthcheck{Test: []string{"CMD-SHELL", "curl -fsS http://127.0.0.1:8333/healthz >/dev/null"}, Interval: "5s", Timeout: "5s", Retries: 30, StartPeriod: "10s"}

	common := map[string]string{
		"NEXTAUTH_URL":               "http://" + hostPort(n.Address, config.PortLangfuse),
		"DATABASE_URL":               "postgresql://langfuse:" + pgPass + "@postgres:5432/langfuse",
		"SALT":                       env.ref("LANGFUSE_SALT", sec[SecretLangfuseSalt]),
		"ENCRYPTION_KEY":             env.ref("LANGFUSE_ENCRYPTION_KEY", sec[SecretLangfuseEncrypt]),
		"TELEMETRY_ENABLED":          "false",
		"CLICKHOUSE_MIGRATION_URL":   "clickhouse://clickhouse:9000",
		"CLICKHOUSE_URL":             "http://clickhouse:8123",
		"CLICKHOUSE_USER":            "clickhouse",
		"CLICKHOUSE_PASSWORD":        chPass,
		"CLICKHOUSE_CLUSTER_ENABLED": "false",
		"REDIS_HOST":                 "valkey",
		"REDIS_PORT":                 "6379",
		"REDIS_AUTH":                 vkPass,
	}
	for _, kind := range []struct{ env, prefix string }{{"EVENT", "events/"}, {"MEDIA", "media/"}} {
		k := "LANGFUSE_S3_" + kind.env + "_UPLOAD_"
		common[k+"BUCKET"] = langfuseBucket
		common[k+"REGION"] = "us-east-1"
		common[k+"ACCESS_KEY_ID"] = s3Key
		common[k+"SECRET_ACCESS_KEY"] = s3Secret
		common[k+"ENDPOINT"] = "http://seaweedfs:8333"
		common[k+"FORCE_PATH_STYLE"] = "true"
		common[k+"PREFIX"] = kind.prefix
	}
	deps := healthy("postgres", "clickhouse", "valkey", "seaweedfs")

	worker := newService(img["langfuse-worker"])
	worker.Environment = maps.Clone(common)
	worker.Environment["NODE_OPTIONS"] = nodeHeap("langfuse-worker")
	worker.DependsOn = deps

	web := newService(img["langfuse-web"])
	web.Environment = maps.Clone(common)
	web.Environment["NODE_OPTIONS"] = nodeHeap("langfuse-web")
	name := r.Cluster.Metadata.Name
	for k, val := range map[string]string{
		"HOSTNAME":                         "0.0.0.0",
		"NEXTAUTH_SECRET":                  env.ref("LANGFUSE_NEXTAUTH_SECRET", sec[SecretLangfuseNextAuth]),
		"AUTH_DISABLE_SIGNUP":              "true",
		"LANGFUSE_INIT_ORG_ID":             "oaf",
		"LANGFUSE_INIT_ORG_NAME":           name,
		"LANGFUSE_INIT_PROJECT_ID":         "oaf",
		"LANGFUSE_INIT_PROJECT_NAME":       name,
		"LANGFUSE_INIT_PROJECT_PUBLIC_KEY": "pk-lf-" + env.ref("LANGFUSE_PUBLIC_KEY", sec[SecretLangfusePublicKey]),
		"LANGFUSE_INIT_PROJECT_SECRET_KEY": "sk-lf-" + env.ref("LANGFUSE_SECRET_KEY", sec[SecretLangfuseSecretKey]),
		"LANGFUSE_INIT_USER_EMAIL":         "admin@oaf.local",
		"LANGFUSE_INIT_USER_NAME":          "admin",
		"LANGFUSE_INIT_USER_PASSWORD":      env.ref("LANGFUSE_ADMIN_PASSWORD", sec[SecretLangfuseAdmin]),
	} {
		web.Environment[k] = val
	}
	web.Ports = []string{portMap(config.PortLangfuse, 3000)}
	web.DependsOn = deps
	web.Healthcheck = &healthcheck{Test: []string{"CMD-SHELL", "wget -q --spider http://127.0.0.1:3000/api/public/health || exit 1"}, Interval: "10s", Timeout: "5s", Retries: 30, StartPeriod: "60s"}

	cf := &composeFile{
		Services: map[string]*service{
			"postgres": postgres, "clickhouse": clickhouse, "valkey": valkey, "seaweedfs": seaweed,
			"langfuse-worker": worker, "langfuse-web": web,
		},
		Volumes: volumes("postgres-data", "clickhouse-data", "clickhouse-logs", "valkey-data", "seaweedfs-data"),
	}
	return finish(p, v, cf, env)
}

// metrics renders Prometheus, Grafana and, if enabled, Loki. Grafana and Loki
// are AGPL-3.0: OAF runs the unmodified images and only configures them
// through files and their HTTP APIs.
func (r *renderer) metrics(n config.Node) (Project, error) {
	p := Project{Node: n.Name, Name: ProjectMetrics}
	v := r.Versions.Version
	env := newEnvFile()
	promImg, err := r.image("prometheus")
	if err != nil {
		return p, err
	}
	grafImg, err := r.image("grafana")
	if err != nil {
		return p, err
	}
	gpass, err := r.secret(SecretGrafanaAdmin)
	if err != nil {
		return p, err
	}

	retention, interval := "15d", "15s"
	if m := r.Cluster.Spec.Observability.Metrics; m != nil {
		if m.Retention != "" {
			retention = m.Retention
		}
		if m.ScrapeInterval != "" {
			interval = m.ScrapeInterval
		}
	}

	prom := newService(promImg)
	prom.Command = []string{
		"--config.file=/etc/prometheus/prometheus.yml",
		"--storage.tsdb.path=/prometheus",
		"--storage.tsdb.retention.time=" + retention,
	}
	if m := r.Cluster.Spec.Observability.Metrics; m != nil && m.RemoteWrite {
		prom.Command = append(prom.Command, "--web.enable-remote-write-receiver")
	}
	prom.Ports = []string{portMap(config.PortPrometheus, 9090)}
	prom.Volumes = []string{"./prometheus.yml:/etc/prometheus/prometheus.yml:ro", "prometheus-data:/prometheus"}
	if r.hasModels() {
		key, err := r.secret(SecretVLLMAPIKey)
		if err != nil {
			return p, err
		}
		prom.Environment = map[string]string{"VLLM_API_KEY": env.ref("VLLM_API_KEY", key)}
		// $$ reaches the shell as $: compose must not interpolate these.
		prom.Entrypoint = []string{"/bin/sh", "-c", `umask 077 && printf %s "$$VLLM_API_KEY" > ` + vllmKeyFile + ` && exec /bin/prometheus "$$@"`, "prometheus"}
	}
	prom.Healthcheck = &healthcheck{Test: []string{"CMD-SHELL", "wget -q --spider http://127.0.0.1:9090/-/healthy || exit 1"}, Interval: "10s", Timeout: "5s", Retries: 10}

	graf := newService(grafImg)
	graf.Environment = map[string]string{
		"GF_SECURITY_ADMIN_USER":                    "admin",
		"GF_SECURITY_ADMIN_PASSWORD":                env.ref("GRAFANA_ADMIN_PASSWORD", gpass),
		"GF_SECURITY_ALLOW_EMBEDDING":               "true", // the OAF UI embeds dashboards (M4)
		"GF_USERS_ALLOW_SIGN_UP":                    "false",
		"GF_ANALYTICS_REPORTING_ENABLED":            "false",
		"GF_ANALYTICS_CHECK_FOR_UPDATES":            "false",
		"GF_PLUGINS_PREINSTALL_DISABLED":            "true", // works air-gapped
		"GF_SERVER_ROOT_URL":                        "http://" + hostPort(n.Address, config.PortGrafana) + "/",
		"GF_AUTH_ANONYMOUS_ENABLED":                 "false",
		"GF_SECURITY_DISABLE_GRAVATAR":              "true",
		"GF_NEWS_NEWS_FEED_ENABLED":                 "false",
		"GF_ANALYTICS_FEEDBACK_LINKS_ENABLED":       "false",
		"GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH": dashboardsDir + "/" + homeDashboard,
	}
	if g := r.Cluster.Spec.Observability.Grafana; g != nil {
		if g.AnonymousViewer {
			graf.Environment["GF_AUTH_ANONYMOUS_ENABLED"] = "true"
			graf.Environment["GF_AUTH_ANONYMOUS_ORG_ROLE"] = "Viewer"
		}
		if g.RootURL != "" {
			root := strings.TrimRight(g.RootURL, "/") + "/"
			graf.Environment["GF_SERVER_ROOT_URL"] = root
			if u, err := url.Parse(root); err == nil && u.Path != "/" {
				graf.Environment["GF_SERVER_SERVE_FROM_SUB_PATH"] = "true"
			}
		}
	}
	graf.Ports = []string{portMap(config.PortGrafana, 3000)}
	graf.Volumes = []string{
		"grafana-data:/var/lib/grafana",
		"./grafana/datasources.yaml:/etc/grafana/provisioning/datasources/oaf.yaml:ro",
		"./grafana/dashboards.yaml:/etc/grafana/provisioning/dashboards/oaf.yaml:ro",
		// Grafana rescans these directories, so dashboard edits need no restart.
		"./grafana/dashboards:" + dashboardsDir + ":ro",
		AppDashboardsDir + ":" + appDashboardsMount + ":ro",
	}
	graf.DependsOn = healthy("prometheus")

	cf := &composeFile{
		Services: map[string]*service{"prometheus": prom, "grafana": graf},
		Volumes:  volumes("prometheus-data", "grafana-data"),
	}
	datasources := []map[string]any{{
		"name": "Prometheus", "uid": "oaf-prometheus", "type": "prometheus", "access": "proxy",
		"url": "http://prometheus:9090", "isDefault": true, "editable": false,
	}}
	var extra []File
	if r.Cluster.Spec.Observability.LogsEnabled() {
		lokiImg, err := r.image("loki")
		if err != nil {
			return p, err
		}
		retention := "7d"
		if l := r.Cluster.Spec.Observability.Logs; l.Retention != "" {
			retention = l.Retention
		}
		loki := newService(lokiImg)
		loki.Command = []string{"-config.file=/etc/loki/config.yaml"}
		loki.Ports = []string{portMap(config.PortLoki, 3100)}
		loki.Volumes = []string{"./loki.yaml:/etc/loki/config.yaml:ro", "loki-data:/loki"}
		cf.Services["loki"] = loki
		cf.Volumes["loki-data"] = map[string]string{}
		lokiCfg, err := yamlFile(v, "loki.yaml", lokiConfig(retention))
		if err != nil {
			return p, err
		}
		extra = append(extra, lokiCfg)
		datasources = append(datasources, map[string]any{
			"name": "Loki", "uid": "oaf-loki", "type": "loki", "access": "proxy",
			"url": "http://loki:3100", "editable": false,
		})
	}
	promCfg, err := yamlFile(v, "prometheus.yml", r.prometheusConfig(interval))
	if err != nil {
		return p, err
	}
	ds, err := yamlFile(v, "grafana/datasources.yaml", map[string]any{
		"apiVersion":  1,
		"datasources": datasources,
	})
	if err != nil {
		return p, err
	}
	dash, err := dashboardFiles(v)
	if err != nil {
		return p, err
	}
	files := append([]File{promCfg, ds}, dash...)
	return finish(p, v, cf, env, append(files, extra...)...)
}

// lokiConfig is single-binary Loki on the filesystem, with the compactor
// deleting logs older than retention.
func lokiConfig(retention string) map[string]any {
	return map[string]any{
		"auth_enabled": false,
		"server":       map[string]any{"http_listen_port": 3100, "log_level": "warn"},
		"common": map[string]any{
			"path_prefix":        "/loki",
			"replication_factor": 1,
			"ring":               map[string]any{"kvstore": map[string]string{"store": "inmemory"}},
			"storage": map[string]any{"filesystem": map[string]string{
				"chunks_directory": "/loki/chunks", "rules_directory": "/loki/rules",
			}},
		},
		"schema_config": map[string]any{"configs": []map[string]any{{
			"from": "2024-01-01", "store": "tsdb", "object_store": "filesystem", "schema": "v13",
			"index": map[string]any{"prefix": "index_", "period": "24h"},
		}}},
		"compactor": map[string]any{
			"working_directory":    "/loki/compactor",
			"retention_enabled":    true,
			"delete_request_store": "filesystem",
		},
		"limits_config": map[string]any{
			"retention_period":          retention,
			"allow_structured_metadata": true,
			"volume_enabled":            true,
		},
		"analytics": map[string]bool{"reporting_enabled": false},
	}
}

type scrapeConfig struct {
	JobName       string          `yaml:"job_name"`
	Scheme        string          `yaml:"scheme,omitempty"`
	MetricsPath   string          `yaml:"metrics_path,omitempty"`
	TLSConfig     map[string]bool `yaml:"tls_config,omitempty"`
	Authorization *authorization  `yaml:"authorization,omitempty"`
	StaticConfigs []staticConfig  `yaml:"static_configs"`
}

type authorization struct {
	Type            string `yaml:"type"`
	CredentialsFile string `yaml:"credentials_file"`
}

// vllmKeyFile is where Prometheus finds the vLLM API key, which also guards
// vLLM's /metrics. The entrypoint writes it from the environment (.env), so
// the key is in no rendered file. /prometheus is the one path the image's
// user can write.
const vllmKeyFile = "/prometheus/.vllm-api-key"

type staticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels,omitempty"`
}

// prometheusConfig scrapes node_exporter on every node, agentgateway on the
// gateway nodes and the API (rank-0) node of every enabled model.
func (r *renderer) prometheusConfig(interval string) map[string]any {
	node := scrapeConfig{JobName: "node"}
	gw := scrapeConfig{JobName: "agentgateway"}
	for _, n := range r.Cluster.Spec.Nodes {
		node.StaticConfigs = append(node.StaticConfigs, staticConfig{Targets: []string{hostPort(n.Address, config.PortNodeExporter)}, Labels: map[string]string{"node": n.Name}})
		if n.HasRole(config.RoleGateway) {
			gw.StaticConfigs = append(gw.StaticConfigs, staticConfig{Targets: []string{hostPort(n.Address, config.PortGatewayStats)}, Labels: map[string]string{"node": n.Name}})
		}
	}
	vllm := scrapeConfig{JobName: "vllm", Authorization: &authorization{Type: "Bearer", CredentialsFile: vllmKeyFile}}
	for i, d := range r.Cluster.Spec.Models {
		if !d.IsEnabled() {
			continue
		}
		nodes := r.Cluster.PlacementNodes(d.Placement)
		n, _ := r.Cluster.Node(nodes[0])
		vllm.StaticConfigs = append(vllm.StaticConfigs, staticConfig{
			Targets: []string{hostPort(n.Address, r.Cluster.ModelPort(i))},
			Labels:  map[string]string{"node": n.Name, "model": d.Name},
		})
	}
	jobs := []scrapeConfig{{JobName: "prometheus", StaticConfigs: []staticConfig{{Targets: []string{"localhost:9090"}}}}, node}
	if len(gw.StaticConfigs) > 0 {
		jobs = append(jobs, gw)
	}
	if len(vllm.StaticConfigs) > 0 {
		jobs = append(jobs, vllm)
	}
	if m := r.Cluster.Spec.Observability.Metrics; m != nil {
		for _, s := range m.Scrape {
			u, err := url.Parse(s.URL)
			if err != nil {
				continue // validation rejects it first
			}
			job := scrapeConfig{JobName: "app-" + s.Name, Scheme: u.Scheme, MetricsPath: u.Path,
				StaticConfigs: []staticConfig{{Targets: []string{u.Host}, Labels: map[string]string{"app": s.Name}}}}
			if s.InsecureSkipVerify {
				job.TLSConfig = map[string]bool{"insecure_skip_verify": true}
			}
			jobs = append(jobs, job)
		}
	}
	return map[string]any{
		"global":         map[string]string{"scrape_interval": interval, "evaluation_interval": interval},
		"scrape_configs": jobs,
	}
}

func (r *renderer) hasModels() bool {
	for _, d := range r.Cluster.Spec.Models {
		if d.IsEnabled() {
			return true
		}
	}
	return false
}

// nodeExporter runs on every node, on the host network, reading the host's
// root filesystem read-only.
func (r *renderer) nodeExporter(n config.Node) (Project, error) {
	p := Project{Node: n.Name, Name: ProjectNodeExporter}
	img, err := r.image("node-exporter")
	if err != nil {
		return p, err
	}
	s := newService(img)
	s.NetworkMode = "host"
	s.PID = "host"
	s.Command = []string{"--path.rootfs=/host", "--web.listen-address=:" + strconv.Itoa(config.PortNodeExporter)}
	s.Volumes = []string{"/:/host:ro,rslave"}
	return finish(p, r.Versions.Version, &composeFile{Services: map[string]*service{"node-exporter": s}}, newEnvFile())
}

// finish adds compose.yaml, the config files the services mount and, if the
// project uses secrets, .env.
func finish(p Project, bundleVersion string, cf *composeFile, env *envFile, configs ...File) (Project, error) {
	limitMemory(cf.Services)
	labelConfigHash(cf.Services, configs)
	f, err := composeFileOf(bundleVersion, cf)
	if err != nil {
		return p, err
	}
	p.Files = append(p.Files, f)
	if len(env.vars) > 0 {
		e, err := envFileOf(bundleVersion, env)
		if err != nil {
			return p, err
		}
		p.Files = append(p.Files, e)
	}
	p.Files = append(p.Files, configs...)
	return p, nil
}

// ConfigHashLabel carries a hash of the config files a container mounts.
// compose recreates a container only when its own definition changes, and
// a changed mounted file is not one. agentgateway hot-reloads its config but
// keeps the tracing exporter it started with, and Prometheus reloads only on
// SIGHUP, so a changed file must restart the container: the label does that.
const ConfigHashLabel = "dev.openagentfabric.config-hash"

func labelConfigHash(services map[string]*service, configs []File) {
	for _, s := range services {
		h := sha256.New()
		n := 0
		for _, f := range configs {
			for _, v := range s.Volumes {
				if strings.HasPrefix(v, "./"+f.Path+":") {
					fmt.Fprintf(h, "%s\x00%d\x00", f.Path, len(f.Content))
					h.Write(f.Content)
					n++
				}
			}
		}
		if n > 0 {
			if s.Labels == nil {
				s.Labels = map[string]string{}
			}
			s.Labels[ConfigHashLabel] = hex.EncodeToString(h.Sum(nil))[:16]
		}
	}
}

// nodeHeap sizes a Node.js service's V8 heap to 75% of its memory limit.
// Left alone, Node takes about half the cgroup limit, and Langfuse web runs
// out of heap at startup under 1 GiB; the rest is for buffers and code.
func nodeHeap(service string) string {
	return "--max-old-space-size=" + strconv.Itoa(memoryLimitsMiB[service]*3/4)
}

func volumes(names ...string) map[string]map[string]string {
	m := map[string]map[string]string{}
	for _, n := range names {
		m[n] = map[string]string{}
	}
	return m
}

func hostPort(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

func portMap(host, container int) string { return fmt.Sprintf("%d:%d", host, container) }

// registry runs the image registry on the control node, on the host network
// with TLS from OAF's CA; images live under /var/lib/oaf/registry.
func (r *renderer) registry(n config.Node) (Project, error) {
	p := Project{Node: n.Name, Name: ProjectRegistry}
	img, err := r.image("registry")
	if err != nil {
		return p, err
	}
	s := newService(img)
	s.NetworkMode = "host"
	s.Environment = map[string]string{
		"REGISTRY_HTTP_ADDR":                        ":" + strconv.Itoa(config.PortRegistry),
		"REGISTRY_HTTP_TLS_CERTIFICATE":             "/certs/registry.crt",
		"REGISTRY_HTTP_TLS_KEY":                     "/certs/registry.key",
		"REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY": "/var/lib/registry",
		"OTEL_TRACES_EXPORTER":                      "none",
	}
	s.Volumes = []string{"/var/lib/oaf/registry:/var/lib/registry", "./registry.crt:/certs/registry.crt:ro", "./registry.key:/certs/registry.key:ro"}
	return finish(p, r.Versions.Version, &composeFile{Services: map[string]*service{"registry": s}}, newEnvFile(),
		File{Path: "registry.crt", Mode: 0o644, Content: r.Registry.Cert},
		File{Path: "registry.key", Mode: 0o600, Content: r.Registry.Key})
}
