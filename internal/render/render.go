// Package render turns cluster.yaml, the model catalog and the pinned bundle
// into per-node docker compose projects. Rendering is pure and deterministic:
// the same inputs (including secret values) give byte-identical files, which
// is what makes `oaf apply` idempotent and lets golden tests pin the output.
package render

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
	"openagentfabric.dev/oaf/internal/secrets"
)

// Input is everything a render needs.
type Input struct {
	Cluster  *config.Cluster
	Catalog  *config.Catalog
	Versions *config.Versions
	Secrets  secrets.Source
	// Features granted by valid entitlements.
	Features map[string]bool
	// Fabric is the discovered QSFP state of each node. Only deployments that
	// span nodes need it.
	Fabric map[string][]fabric.Port
}

// File is one file of a project, relative to the project directory.
type File struct {
	Path    string
	Mode    uint32
	Content []byte
}

// Project is one compose project on one node. The node agent runs it as
// oaf-<Name>.
type Project struct {
	Node  string
	Name  string
	Files []File
	// Weights are the model files the project needs on its node: the agent
	// downloads them and starts the project once they are complete.
	Weights *Weights
}

// Weights is a model's weights on one node: a Hugging Face repo pinned at a
// revision, downloaded into Dir.
type Weights struct {
	ModelID, Dir, Repo, Revision string
	Include                      []string
}

// ModelWeights returns where a deployment's weights go on each of its nodes,
// keyed by node; nil when the model is not downloaded from Hugging Face
// (an entitled model arrives with its entitlement).
func ModelWeights(c *config.Cluster, cat *config.Catalog, d config.ModelDeployment) map[string]*Weights {
	m, ok := cat.Lookup(d.CatalogRef)
	if !ok || m.Source.HuggingFace == nil {
		return nil
	}
	out := map[string]*Weights{}
	for _, name := range c.PlacementNodes(d.Placement) {
		n, _ := c.Node(name)
		hf := m.Source.HuggingFace
		out[name] = &Weights{ModelID: m.ID, Dir: path.Join(modelsPath(n), m.ID), Repo: hf.Repo, Revision: hf.Revision,
			Include: append([]string{}, hf.Include...)}
	}
	return out
}

// Project names.
const (
	ProjectNodeExporter = "node-exporter"
	ProjectGateway      = "gateway"
	ProjectRouter       = "router" // vSR + Envoy for model: auto, when routing has active rules
	ProjectLangfuse     = "langfuse"
	ProjectMetrics      = "metrics"
	modelProjectPrefix  = "model-"
)

// Secret names in the secrets store.
const (
	SecretPostgres          = "postgres-password"
	SecretClickHouse        = "clickhouse-password"
	SecretValkey            = "valkey-password"
	SecretS3AccessKey       = "s3-access-key"
	SecretS3SecretKey       = "s3-secret-key"
	SecretLangfuseSalt      = "langfuse-salt"
	SecretLangfuseEncrypt   = "langfuse-encryption-key"
	SecretLangfuseNextAuth  = "langfuse-nextauth-secret"
	SecretLangfusePublicKey = "langfuse-public-key"
	SecretLangfuseSecretKey = "langfuse-secret-key"
	SecretLangfuseAdmin     = "langfuse-admin-password"
	SecretGrafanaAdmin      = "grafana-admin-password"
	SecretVLLMAPIKey        = "vllm-api-key"
)

// DefaultModelsPath is where weights live on a node unless storage.modelsPath says otherwise.
const DefaultModelsPath = "/var/lib/oaf/models"

// Render validates the input and renders every project of the cluster,
// ordered by node and then project name.
func Render(in Input) ([]Project, error) {
	if in.Cluster == nil || in.Catalog == nil || in.Versions == nil || in.Secrets == nil {
		return nil, errors.New("render: cluster, catalog, versions and secrets are required")
	}
	if rep := config.Validate(in.Cluster, in.Catalog, config.Options{Features: in.Features}); !rep.OK() {
		var b strings.Builder
		for _, e := range rep.Errors() {
			fmt.Fprintf(&b, "\n  %s", e)
		}
		return nil, fmt.Errorf("cluster.yaml is not valid:%s", b.String())
	}
	r := &renderer{Input: in, images: map[string]string{}}
	for _, c := range in.Versions.Components {
		r.images[c.Name] = c.Image
	}

	var out []Project
	add := func(p Project, err error) error {
		if err != nil {
			return fmt.Errorf("%s/%s: %w", p.Node, p.Name, err)
		}
		out = append(out, p)
		return nil
	}
	for _, n := range in.Cluster.Spec.Nodes {
		if err := add(r.nodeExporter(n)); err != nil {
			return nil, err
		}
		if n.HasRole(config.RoleGateway) {
			if err := add(r.gateway(n)); err != nil {
				return nil, err
			}
			if rt := r.routes(); rt.Enabled() {
				if err := add(r.router(n, rt)); err != nil {
					return nil, err
				}
			}
		}
		if n.HasRole(config.RoleObservability) {
			if err := add(r.langfuse(n)); err != nil {
				return nil, err
			}
			if err := add(r.metrics(n)); err != nil {
				return nil, err
			}
		}
	}
	models, err := r.models()
	if err != nil {
		return nil, err
	}
	out = append(out, models...)

	if err := r.checkImageLicences(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

type renderer struct {
	Input
	images map[string]string
	used   []string // components rendered, for the licence check
}

// image returns the pinned image of a bundle component.
func (r *renderer) image(component string) (string, error) {
	img, ok := r.images[component]
	if !ok {
		return "", fmt.Errorf("bundle %s pins no %q component", r.Versions.Version, component)
	}
	r.used = append(r.used, component)
	return img, nil
}

func (r *renderer) secret(name string) (string, error) {
	v, err := r.Secrets.Value(name)
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	return v, nil
}

// checkImageLicences refuses to deploy an image whose terms need explicit
// acceptance (licence-audit §3.7) unless cluster.yaml records it.
func (r *renderer) checkImageLicences() error {
	accepted := map[string]bool{}
	for _, a := range r.Cluster.Spec.LicenseAcceptances {
		accepted[a.Subject] = true
	}
	seen := map[string]bool{}
	for _, c := range r.Versions.Components {
		if seen[c.Name] || !contains(r.used, c.Name) {
			continue
		}
		seen[c.Name] = true
		if c.License.NeedsAcceptance() && !accepted[config.ImageAcceptanceSubject(c.Name)] {
			return fmt.Errorf("the %s image includes terms that must be accepted first: read %s, then record %s in spec.licenseAcceptances",
				c.Name, c.License.AcceptURL, config.ImageAcceptanceSubject(c.Name))
		}
	}
	return nil
}

// observabilityNode is the node with the observability role (exactly one, by validation).
func (r *renderer) observabilityNode() (config.Node, bool) {
	for _, n := range r.Cluster.Spec.Nodes {
		if n.HasRole(config.RoleObservability) {
			return n, true
		}
	}
	return config.Node{}, false
}

func (r *renderer) langfuseEnabled() bool {
	lf := r.Cluster.Spec.Observability.Langfuse
	return lf == nil || lf.Enabled == nil || *lf.Enabled
}

func composeFileOf(bundleVersion string, c *composeFile) (File, error) {
	b, err := marshalYAML(bundleVersion, c)
	return File{Path: "compose.yaml", Mode: 0o644, Content: b}, err
}

func yamlFile(bundleVersion, path string, v any) (File, error) {
	b, err := marshalYAML(bundleVersion, v)
	return File{Path: path, Mode: 0o644, Content: b}, err
}

func envFileOf(bundleVersion string, e *envFile) (File, error) {
	b, err := e.render(bundleVersion)
	return File{Path: ".env", Mode: 0o600, Content: b}, err
}

var notSlug = regexp.MustCompile(`[^a-z0-9-]+`)

// modelProjectName maps a served model name (which may hold '.', '_' and '/')
// to a valid project name. The node agent allows 40 characters.
func modelProjectName(served string) string { return ModelProjectName(served) }

// ModelProjectName is the compose project serving a deployment (see
// modelProjectName).
func ModelProjectName(served string) string {
	s := strings.Trim(notSlug.ReplaceAllString(served, "-"), "-")
	name := modelProjectPrefix + s
	if s == served && len(name) <= 40 {
		return name
	}
	// Lossy mapping: add a short hash of the original so two names cannot collide.
	h := fnv32(served)
	if limit := 40 - len(modelProjectPrefix) - 9; len(s) > limit {
		s = strings.TrimRight(s[:limit], "-")
	}
	return fmt.Sprintf("%s%s-%08x", modelProjectPrefix, s, h)
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
