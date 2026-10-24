package config

// Types mirror schema/cluster.schema.json and schema/catalog.schema.json.
// Documents are schema-validated before they are decoded into these types,
// so the types do not repeat structural checks.

// APIVersion is the only supported apiVersion.
const APIVersion = "oaf/v1alpha1"

// Cluster is cluster.yaml.
type Cluster struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   Metadata    `yaml:"metadata"`
	Spec       ClusterSpec `yaml:"spec"`
}

// Metadata names a cluster.
type Metadata struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
}

// ClusterSpec is the desired state.
type ClusterSpec struct {
	Bundle             Bundle              `yaml:"bundle"`
	Nodes              []Node              `yaml:"nodes"`
	Fabric             Fabric              `yaml:"fabric"`
	Groups             []Group             `yaml:"groups,omitempty"`
	Models             []ModelDeployment   `yaml:"models,omitempty"`
	Routing            Routing             `yaml:"routing,omitempty"`
	Observability      Observability       `yaml:"observability,omitempty"`
	Entitlements       []EntitlementRef    `yaml:"entitlements,omitempty"`
	LicenseAcceptances []LicenseAcceptance `yaml:"licenseAcceptances,omitempty"`
	Apps               []map[string]any    `yaml:"apps,omitempty"`
	Agents             []map[string]any    `yaml:"agents,omitempty"`
}

// Bundle selects the pinned component set.
type Bundle struct {
	Version string `yaml:"version"`
	Channel string `yaml:"channel,omitempty"`
}

// Node roles.
const (
	RoleControl       = "control"
	RoleGateway       = "gateway"
	RoleObservability = "observability"
	RoleInference     = "inference"
)

// Node is one GB10 box.
type Node struct {
	Name    string            `yaml:"name"`
	Address string            `yaml:"address"`
	Serial  string            `yaml:"serial,omitempty"`
	Roles   []string          `yaml:"roles"`
	SSH     *SSH              `yaml:"ssh,omitempty"`
	Storage *Storage          `yaml:"storage,omitempty"`
	Labels  map[string]string `yaml:"labels,omitempty"`
}

// HasRole reports whether the node has role r.
func (n Node) HasRole(r string) bool {
	for _, x := range n.Roles {
		if x == r {
			return true
		}
	}
	return false
}

// SSH is used only for bootstrap.
type SSH struct {
	User string `yaml:"user,omitempty"`
	Port int    `yaml:"port,omitempty"`
}

// Storage holds per-node paths and disk headroom.
type Storage struct {
	ModelsPath string   `yaml:"modelsPath,omitempty"`
	DataPath   string   `yaml:"dataPath,omitempty"`
	MinFreeGiB *float64 `yaml:"minFreeGiB,omitempty"`
}

// Topologies.
const (
	TopologySingle   = "single"
	TopologyPair     = "pair"
	TopologyTriangle = "triangle"
	TopologyPairs    = "pairs"
	// TopologyPairPlusSingle is one pair over ConnectX-7 (a TP=2 group) plus
	// a node reached over the LAN only, which takes no fabric link.
	TopologyPairPlusSingle = "pair-plus-single"
)

// NCCL transports.
const (
	TransportAuto   = "auto"
	TransportIB     = "ib"
	TransportSocket = "socket"
)

// Fabric describes the switchless QSFP fabric.
type Fabric struct {
	Topology   string            `yaml:"topology"`
	MTU        int               `yaml:"mtu,omitempty"`
	Links      []Link            `yaml:"links,omitempty"`
	RoCE       RoCE              `yaml:"roce,omitempty"`
	NCCL       NCCL              `yaml:"nccl,omitempty"`
	Acceptance *FabricAcceptance `yaml:"acceptance,omitempty"`
}

// RoCE settings. GIDIndex is "auto" or an integer, kept as a string.
type RoCE struct {
	GIDIndex string `yaml:"gidIndex,omitempty"`
}

// NCCL settings.
type NCCL struct {
	Transport string            `yaml:"transport,omitempty"`
	LDPreload string            `yaml:"ldPreload,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
}

// FabricAcceptance holds bandwidth thresholds.
type FabricAcceptance struct {
	MinGbpsPerRail  *float64 `yaml:"minGbpsPerRail,omitempty"`
	MinGbpsDualRail *float64 `yaml:"minGbpsDualRail,omitempty"`
}

// Link is one QSFP cable between physical ports. Each port is exposed as two
// netdevs (one per PCIe domain), which are the link's two rails.
type Link struct {
	A       LinkEnd  `yaml:"a"`
	B       LinkEnd  `yaml:"b"`
	Subnets []string `yaml:"subnets,omitempty"`
}

// LinkEnd is one end of a cable.
type LinkEnd struct {
	Node string `yaml:"node"`
	Port *int   `yaml:"port,omitempty"`
}

// Group is a placement group for multi-node deployments.
type Group struct {
	Name  string   `yaml:"name"`
	Nodes []string `yaml:"nodes"`
}

// ModelDeployment is one served model.
type ModelDeployment struct {
	Name                 string            `yaml:"name"`
	CatalogRef           string            `yaml:"catalogRef"`
	Profile              string            `yaml:"profile,omitempty"`
	Enabled              *bool             `yaml:"enabled,omitempty"`
	Placement            Placement         `yaml:"placement"`
	GPUMemoryUtilization float64           `yaml:"gpuMemoryUtilization"`
	MaxModelLen          int               `yaml:"maxModelLen,omitempty"`
	Port                 int               `yaml:"port,omitempty"`
	Guardrails           []string          `yaml:"guardrails,omitempty"`
	Args                 []string          `yaml:"args,omitempty"`
	Env                  map[string]string `yaml:"env,omitempty"`
}

// IsEnabled applies the schema default (true).
func (m ModelDeployment) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// Placement is exactly one of Node or Group.
type Placement struct {
	Node  string `yaml:"node,omitempty"`
	Group string `yaml:"group,omitempty"`
}

// Routing modes.
const (
	ModeObserve = "observe"
	ModeActive  = "active"
)

// Routing is the OAF routing abstraction rendered into agentgateway and vSR.
type Routing struct {
	Mode       string            `yaml:"mode,omitempty"`
	Gateway    *Gateway          `yaml:"gateway,omitempty"`
	Keys       []APIKey          `yaml:"keys,omitempty"`
	Defaults   RoutingDefaults   `yaml:"defaults,omitempty"`
	Rules      []RoutingRule     `yaml:"rules,omitempty"`
	Providers  []Provider        `yaml:"providers,omitempty"`
	Guardrails []GuardrailPolicy `yaml:"guardrails,omitempty"`
}

// Provider types (spec.routing.providers[].type).
const (
	ProviderOpenAI           = "openai"
	ProviderAnthropic        = "anthropic"
	ProviderGemini           = "gemini"
	ProviderAzureOpenAI      = "azure-openai"
	ProviderOpenAICompatible = "openai-compatible"
)

// Provider is an external model provider whose models the gateway serves
// next to the local ones. Prompts sent to it leave the appliance.
type Provider struct {
	Name        string          `yaml:"name"`
	Type        string          `yaml:"type"`
	Endpoint    string          `yaml:"endpoint,omitempty"`
	Azure       *AzureSettings  `yaml:"azure,omitempty"`
	SecretRef   string          `yaml:"secretRef,omitempty"`
	AllowEgress bool            `yaml:"allowEgress,omitempty"`
	Guardrails  []string        `yaml:"guardrails,omitempty"`
	Models      []ProviderModel `yaml:"models"`
}

// AzureSettings name an Azure OpenAI resource.
type AzureSettings struct {
	ResourceName string `yaml:"resourceName"`
	APIVersion   string `yaml:"apiVersion,omitempty"`
}

// ProviderModel is one model a provider serves: the name clients send and
// the provider's own id.
type ProviderModel struct {
	Name     string `yaml:"name"`
	Upstream string `yaml:"upstream,omitempty"`
}

// UpstreamModel is the provider's id for m (its name when unset).
func (m ProviderModel) UpstreamModel() string {
	if m.Upstream != "" {
		return m.Upstream
	}
	return m.Name
}

// GuardrailPolicy is a named set of agentgateway prompt guards, attached to
// providers and model deployments.
type GuardrailPolicy struct {
	Name        string  `yaml:"name"`
	Description string  `yaml:"description,omitempty"`
	Request     []Guard `yaml:"request,omitempty"`
	Response    []Guard `yaml:"response,omitempty"`
	Message     string  `yaml:"message,omitempty"`
}

// Guard is one check: exactly one of its fields is set.
type Guard struct {
	Regex   *RegexGuard   `yaml:"regex,omitempty"`
	Webhook *WebhookGuard `yaml:"webhook,omitempty"`
}

// RegexGuard matches built-in detectors and custom patterns.
type RegexGuard struct {
	Action   string   `yaml:"action"` // reject | mask | audit
	Builtins []string `yaml:"builtins,omitempty"`
	Patterns []string `yaml:"patterns,omitempty"`
}

// WebhookGuard asks an external guardrail service.
type WebhookGuard struct {
	Host string `yaml:"host"`
}

// GuardrailBuiltins are the built-in detectors of the pinned agentgateway
// (v1.6.0 llm.models[].guardrails regex builtin enum).
var GuardrailBuiltins = []string{"ssn", "creditCard", "phoneNumber", "email", "caSin"}

// Gateway listener settings.
type Gateway struct {
	Port int         `yaml:"port,omitempty"`
	TLS  *GatewayTLS `yaml:"tls,omitempty"`
}

// GatewayTLS settings.
type GatewayTLS struct {
	Enabled       bool   `yaml:"enabled,omitempty"`
	CertSecretRef string `yaml:"certSecretRef,omitempty"`
}

// APIKey references a generated key secret.
type APIKey struct {
	Name              string   `yaml:"name"`
	SecretRef         string   `yaml:"secretRef"`
	Models            []string `yaml:"models,omitempty"`
	RequestsPerMinute int      `yaml:"requestsPerMinute,omitempty"`
}

// RoutingDefaults are the fast and fallback targets.
type RoutingDefaults struct {
	Fast     string `yaml:"fast,omitempty"`
	Fallback string `yaml:"fallback,omitempty"`
}

// RoutingRule is one ordered routing rule.
type RoutingRule struct {
	Name     string        `yaml:"name"`
	Match    RuleMatch     `yaml:"match"`
	Target   string        `yaml:"target"`
	Requires *RuleRequires `yaml:"requires,omitempty"`
}

// RuleMatch fires if any signal fires.
type RuleMatch struct {
	Signals []Signal `yaml:"signals,omitempty"`
}

// Signal is a vSR classification signal.
type Signal struct {
	Type          string   `yaml:"type"`
	Threshold     *float64 `yaml:"threshold,omitempty"`
	Domains       []string `yaml:"domains,omitempty"`
	Keywords      []string `yaml:"keywords,omitempty"`
	CaseSensitive bool     `yaml:"caseSensitive,omitempty"`
	// MinTokens: a context signal fires for prompts at least this long.
	MinTokens int `yaml:"minTokens,omitempty"`
}

// RuleRequires gates a rule on an entitlement feature.
type RuleRequires struct {
	Entitlement string `yaml:"entitlement,omitempty"`
}

// Observability settings.
type Observability struct {
	Langfuse *LangfuseSettings `yaml:"langfuse,omitempty"`
	Metrics  *MetricsSettings  `yaml:"metrics,omitempty"`
	Logs     *LogsSettings     `yaml:"logs,omitempty"`
	Grafana  *GrafanaSettings  `yaml:"grafana,omitempty"`
}

// LangfuseSettings control tracing.
type LangfuseSettings struct {
	Enabled        *bool    `yaml:"enabled,omitempty"`
	SampleRate     *float64 `yaml:"sampleRate,omitempty"`
	CaptureContent *bool    `yaml:"captureContent,omitempty"`
}

// MetricsSettings control Prometheus.
type MetricsSettings struct {
	Retention      string `yaml:"retention,omitempty"`
	ScrapeInterval string `yaml:"scrapeInterval,omitempty"`
	// RemoteWrite lets agents on other hosts push metrics to Prometheus
	// (/api/v1/write), e.g. an app's endpoint collectors.
	RemoteWrite bool `yaml:"remoteWrite,omitempty"`
	// Scrape adds metrics endpoints of apps running next to OAF.
	Scrape []ScrapeTarget `yaml:"scrape,omitempty"`
}

// ScrapeTarget is an app's /metrics endpoint for Prometheus to scrape.
type ScrapeTarget struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// InsecureSkipVerify accepts a self-signed certificate (an appliance's own CA).
	InsecureSkipVerify bool `yaml:"insecureSkipVerify,omitempty"`
}

// LogsSettings control Loki, the log store. It is off unless enabled.
type LogsSettings struct {
	Enabled   bool   `yaml:"enabled,omitempty"`
	Retention string `yaml:"retention,omitempty"`
}

// LogsEnabled reports whether Loki is part of the stack.
func (o Observability) LogsEnabled() bool { return o.Logs != nil && o.Logs.Enabled }

// GrafanaSettings control Grafana.
type GrafanaSettings struct {
	// AnonymousViewer lets anyone who reaches Grafana view dashboards
	// without signing in, so an app can embed them.
	AnonymousViewer bool `yaml:"anonymousViewer,omitempty"`
	// RootURL is the public URL Grafana is reached at when an app serves it
	// through its own reverse proxy, e.g. https://console/grafana/. A path
	// makes Grafana serve from that sub path (also on its own port).
	RootURL string `yaml:"rootUrl,omitempty"`
}

// EntitlementRef points to a signed entitlement file.
type EntitlementRef struct {
	File string `yaml:"file"`
}

// LicenseAcceptance records that a user accepted an upstream licence.
type LicenseAcceptance struct {
	Subject    string `yaml:"subject"`
	URL        string `yaml:"url"`
	AcceptedBy string `yaml:"acceptedBy"`
	AcceptedAt string `yaml:"acceptedAt"`
}

// Catalog is catalog/models.yaml.
type Catalog struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Models     []CatalogModel `yaml:"models"`
}

// Lookup returns the catalog entry with the given id.
func (c *Catalog) Lookup(id string) (*CatalogModel, bool) {
	for i := range c.Models {
		if c.Models[i].ID == id {
			return &c.Models[i], true
		}
	}
	return nil, false
}

// CatalogModel is one model the catalog knows how to serve.
type CatalogModel struct {
	ID          string       `yaml:"id"`
	DisplayName string       `yaml:"displayName,omitempty"`
	Description string       `yaml:"description,omitempty"`
	Source      ModelSource  `yaml:"source"`
	License     ModelLicense `yaml:"license"`
	Entitlement *Entitlement `yaml:"entitlement,omitempty"`
	SizeGiB     float64      `yaml:"sizeGiB"`
	Classes     []string     `yaml:"classes"`
	// EmbeddingDimensions is the vector size of an embedding-class model.
	EmbeddingDimensions int           `yaml:"embeddingDimensions,omitempty"`
	Profiles            []ServingProf `yaml:"profiles"`
}

// ClassEmbedding marks a model that serves /v1/embeddings, not chat.
const ClassEmbedding = "embedding"

// IsEmbedding reports whether the model serves embeddings.
func (m *CatalogModel) IsEmbedding() bool {
	for _, c := range m.Classes {
		if c == ClassEmbedding {
			return true
		}
	}
	return false
}

// Entitlement names the feature that unlocks a model.
type Entitlement struct {
	Feature string `yaml:"feature"`
}

// ModelSource is exactly one of HuggingFace or Entitled.
type ModelSource struct {
	HuggingFace *HFSource       `yaml:"huggingface,omitempty"`
	Entitled    *EntitledSource `yaml:"entitled,omitempty"`
}

// HFSource pins a Hugging Face repo at a commit.
type HFSource struct {
	Repo     string   `yaml:"repo"`
	Revision string   `yaml:"revision"`
	Include  []string `yaml:"include,omitempty"`
}

// EntitledSource is delivered with an entitlement.
type EntitledSource struct {
	SHA256 string `yaml:"sha256"`
}

// ModelLicense carries the facts the bundler and acceptance flow need.
type ModelLicense struct {
	SPDX            string `yaml:"spdx,omitempty"`
	Name            string `yaml:"name,omitempty"`
	URL             string `yaml:"url,omitempty"`
	AcceptURL       string `yaml:"acceptUrl,omitempty"`
	Gated           bool   `yaml:"gated"`
	Redistributable bool   `yaml:"redistributable"`
}

// ServingProf is one way to serve a model on a GB10 topology.
type ServingProf struct {
	Name                 string            `yaml:"name"`
	Engine               string            `yaml:"engine,omitempty"`
	Nodes                int               `yaml:"nodes"`
	TensorParallel       int               `yaml:"tensorParallel"`
	PipelineParallel     int               `yaml:"pipelineParallel,omitempty"`
	GPUMemoryUtilization float64           `yaml:"gpuMemoryUtilization"`
	MaxModelLen          int               `yaml:"maxModelLen,omitempty"`
	Quantization         string            `yaml:"quantization,omitempty"`
	Args                 []string          `yaml:"args,omitempty"`
	Env                  map[string]string `yaml:"env,omitempty"`
	Container            *Container        `yaml:"container,omitempty"`
	Baseline             *Baseline         `yaml:"baseline,omitempty"`
}

// EngineComponent is the bundle component whose image serves the profile.
func (p ServingProf) EngineComponent() string {
	if p.Engine == "" {
		return "vllm"
	}
	return p.Engine
}

// Container is what an engine image needs beyond OAF's container defaults.
type Container struct {
	CommandPrefix []string       `yaml:"commandPrefix,omitempty"`
	IPC           string         `yaml:"ipc,omitempty"`
	ShmSize       string         `yaml:"shmSize,omitempty"`
	Ulimits       map[string]int `yaml:"ulimits,omitempty"`
	SecurityOpt   []string       `yaml:"securityOpt,omitempty"`
	Caches        []string       `yaml:"caches,omitempty"`
}

// PP applies the schema default (1).
func (p ServingProf) PP() int {
	if p.PipelineParallel == 0 {
		return 1
	}
	return p.PipelineParallel
}

// Baseline is the reference performance for a profile.
type Baseline struct {
	TokPerSec float64  `yaml:"tokPerSec"`
	TTFTMsP50 float64  `yaml:"ttftMsP50,omitempty"`
	Tolerance *float64 `yaml:"tolerance,omitempty"`
	Workload  Workload `yaml:"workload"`
	Measured  Measured `yaml:"measured"`
}

// Workload is the benchmark shape.
type Workload struct {
	InputTokens  int `yaml:"inputTokens"`
	OutputTokens int `yaml:"outputTokens"`
	Concurrency  int `yaml:"concurrency"`
}

// Measured records provenance of a baseline.
type Measured struct {
	Date          string `yaml:"date"`
	BundleVersion string `yaml:"bundleVersion"`
	Notes         string `yaml:"notes,omitempty"`
}

// Versions is bundle/versions.yaml: the pinned component set of one release.
type Versions struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Version    string      `yaml:"version"`
	Components []Component `yaml:"components"`
	Artifacts  []Artifact  `yaml:"artifacts,omitempty"`
}

// Artifact is a model file set a component loads itself (e.g. vSR's
// encoder), pinned by revision and fetched by OAF, never at run time.
type Artifact struct {
	Name        string          `yaml:"name"`
	ThirdParty  string          `yaml:"thirdParty"`
	UsedBy      string          `yaml:"usedBy"`
	MountAs     string          `yaml:"mountAs"`
	HuggingFace HFSource        `yaml:"huggingface"`
	SizeMiB     int             `yaml:"sizeMiB"`
	MemoryMiB   int             `yaml:"memoryMiB"`
	License     ArtifactLicense `yaml:"license"`
}

// ArtifactLicense is an artifact's licence.
type ArtifactLicense struct {
	SPDX string `yaml:"spdx"`
	URL  string `yaml:"url"`
}

// Artifact returns the artifact called name.
func (v *Versions) Artifact(name string) (Artifact, bool) {
	for _, a := range v.Artifacts {
		if a.Name == name {
			return a, true
		}
	}
	return Artifact{}, false
}

// Component is one pinned upstream image.
type Component struct {
	Name       string           `yaml:"name"`
	ThirdParty string           `yaml:"thirdParty"`
	Image      string           `yaml:"image"`
	Platforms  []string         `yaml:"platforms"`
	Upstream   Upstream         `yaml:"upstream"`
	License    ComponentLicense `yaml:"license"`
}

// Upstream is the source repo and tag an image is built from.
type Upstream struct {
	Repo string `yaml:"repo"`
	Ref  string `yaml:"ref"`
}

// LicenseFileURL is where the licence file can be fetched at the pinned ref.
func (c Component) LicenseFileURL() string {
	return "https://raw.githubusercontent.com/" + c.Upstream.Repo + "/" + c.Upstream.Ref + "/" + c.License.File
}

// ComponentLicense records the licence and its pinned file hash.
type ComponentLicense struct {
	SPDX          string `yaml:"spdx"`
	File          string `yaml:"file"`
	SHA256        string `yaml:"sha256"`
	IncludedTerms []Term `yaml:"includedTerms,omitempty"`
	AcceptURL     string `yaml:"acceptUrl,omitempty"`
}

// Term is third-party terms shipped inside an image.
type Term struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}
