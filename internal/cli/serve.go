package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/config"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/oafd"
	"openagentfabric.dev/oaf/internal/pki"
	"openagentfabric.dev/oaf/internal/reconcile"
	"openagentfabric.dev/oaf/internal/render"
	"openagentfabric.dev/oaf/internal/secrets"
)

// APICertName is the management API's certificate in the PKI directory.
const APICertName = "oaf-api"

// DefaultStateDir is where oafd keeps its small state.
const DefaultStateDir = "/var/lib/oaf"

// apiStateDir is where `oaf keys issue --admin` finds or makes oafd's
// self-signed certificate when the cluster CA's key is not here; tests
// point it elsewhere.
var apiStateDir = DefaultStateDir

func newServeCmd() *cobra.Command {
	var f stackFlags
	var listen, stateDir string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run oafd, the management API apps use to run the platform (oaf-api.service)",
		Long: "Serves the management API (ADR-0002) over HTTPS on the control node: status, config,\n" +
			"catalog, models, external providers, guardrails, routing and keys. Every change is a plan\n" +
			"(a cluster.yaml diff with validation findings) that is applied separately. Requests need a\n" +
			"management token from `oaf keys issue <app> --admin`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			c, err := config.LoadCluster(f.cluster)
			if err != nil {
				return err
			}
			tlsCfg, fp, err := apiTLS(f.pkiDir, stateDir, c)
			if err != nil {
				return err
			}
			srv := oafd.New(oafd.Config{
				ClusterPath: f.cluster, CatalogPath: f.catalog, Secrets: secrets.Store{Dir: f.secretsDir}, StateDir: stateDir,
				Apply:         func(ctx context.Context, w io.Writer) error { return runApply(ctx, w, &f, false) },
				Status:        func(ctx context.Context) (any, error) { return statusView(ctx, &f) },
				GatewayConfig: func(ctx context.Context) ([]byte, error) { return renderedGatewayConfig(ctx, &f) },
				Keys:          f.keyService(cmd.ErrOrStderr()),
				Log:           log,
				Versions:      bundleVersions(),
				Check:         func(ctx context.Context, doc []byte) error { return renderCheck(ctx, &f, stateDir, doc) },
				Prefetch:      func(ctx context.Context, c *config.Cluster, d config.ModelDeployment) { prefetch(ctx, &f, c, d, log) },
			})
			if err := mountGrowth(srv, &f, c, stateDir, log); err != nil {
				log.Warn("adding boxes is unavailable", "err", err)
			}
			hs := &http.Server{Addr: listen, Handler: srv.Handler(), TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			errc := make(chan error, 1)
			go func() { errc <- hs.ListenAndServeTLS("", "") }()
			log.Info("oafd listening", "addr", listen, "cert_sha256", fp)
			select {
			case err := <-errc:
				return err
			case <-ctx.Done():
				shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := hs.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			}
		},
	}
	f.register(cmd, 45*time.Minute)
	cmd.Flags().StringVar(&listen, "listen", ":"+strconv.Itoa(config.PortOAFD), "listen address")
	cmd.Flags().StringVar(&stateDir, "state-dir", DefaultStateDir, "directory for oafd's state")
	return cmd
}

// apiTLS returns the API's TLS config and its certificate's SHA-256. The
// certificate is issued by the cluster CA when its key is here (oaf pki
// init), for the control node's addresses and the names apps on the node
// use; otherwise a self-signed one is kept in the state dir. Apps can pin
// the fingerprint (oaf keys issue --admin prints it).
func apiTLS(pkiDir, stateDir string, c *config.Cluster) (*tls.Config, string, error) {
	pair, err := apiCert(pkiDir, stateDir, c)
	if err != nil {
		return nil, "", err
	}
	cert, err := tls.X509KeyPair(pair.CertPEM, pair.KeyPEM)
	if err != nil {
		return nil, "", err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, certFingerprint(pair.CertPEM), nil
}

func apiCert(pkiDir, stateDir string, c *config.Cluster) (pki.Pair, error) {
	if p, err := pki.Load(pkiDir, APICertName); err == nil {
		return p, nil
	}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, n := range c.Spec.Nodes {
		if n.HasRole(config.RoleControl) {
			if ip := net.ParseIP(n.Address); ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	dns := []string{"localhost", "host.docker.internal"}
	if h, err := os.Hostname(); err == nil {
		dns = append(dns, h)
	}
	if ca, err := pki.Load(pkiDir, "ca"); err == nil {
		p, err := pki.Issue(ca, pki.LeafOptions{CommonName: APICertName, DNSNames: dns, IPs: ips, Server: true})
		if err != nil {
			return pki.Pair{}, err
		}
		return p, pki.Save(pkiDir, APICertName, p)
	}
	if p, err := pki.Load(stateDir, APICertName); err == nil {
		return p, nil
	}
	p, err := selfSigned(dns, ips)
	if err != nil {
		return pki.Pair{}, err
	}
	return p, pki.Save(stateDir, APICertName, p)
}

func selfSigned(dns []string, ips []net.IP) (pki.Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return pki.Pair{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: APICertName, Organization: []string{"openagentfabric"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(pki.LeafValidity),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: dns, IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return pki.Pair{}, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return pki.Pair{}, err
	}
	return pki.Pair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
	}, nil
}

func certFingerprint(certPEM []byte) string {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return ""
	}
	h := sha256.Sum256(b.Bytes)
	return hex.EncodeToString(h[:])
}

// statusView is `oaf status` as the API's JSON: nodes with their agent and
// ports, deployments with the state of their projects, and the other
// components.
func statusView(ctx context.Context, f *stackFlags) (oafd.StatusView, error) {
	s, err := f.load(io.Discard)
	if err != nil {
		return oafd.StatusView{}, err
	}
	v := oafd.StatusView{Cluster: s.cluster.Metadata.Name, Topology: s.cluster.Spec.Fabric.Topology,
		Nodes: []oafd.NodeView{}, Groups: []oafd.GroupView{}, Deployments: []oafd.DeploymentView{}, Components: []oafd.ComponentView{}}
	for _, g := range s.cluster.Spec.Groups {
		v.Groups = append(v.Groups, oafd.GroupView{Name: g.Name, Nodes: g.Nodes})
	}
	v.LicensesNeeded = config.NeededAcceptances(s.cluster, s.catalog, s.versions)
	projects, _ := s.render(ctx, secrets.Preview{Store: secrets.Store{Dir: f.secretsDir}})
	states := reconcile.Status(ctx, s.nodes, projects)

	byProject := map[string][]reconcile.ProjectState{}
	nodeBad := map[string]bool{}
	for _, st := range states {
		byProject[st.Project] = append(byProject[st.Project], st)
		if !st.Healthy() {
			nodeBad[st.Node] = true
		}
	}
	for _, n := range s.cluster.Spec.Nodes {
		nv := oafd.NodeView{Name: n.Name, Address: n.Address, Roles: n.Roles, State: "offline"}
		if cl, ok := s.nodes[n.Name]; ok {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if info, err := cl.Info(cctx, &nodev1.InfoRequest{}); err == nil {
				nv.State, nv.Version = "joined", info.GetVersion()
				serial := info.GetSerial()
				if serial == "" {
					serial = n.Serial
				}
				nv.Hardware = &oafd.HardwareView{Arch: info.GetArch(), Serial: serial, GPU: info.GetGpu(),
					MemoryGiB: info.GetMemTotalGib(), MemoryFreeGiB: info.GetMemAvailableGib(), DiskFreeGiB: info.GetDiskFreeGib()}
				nv.Health = "healthy"
				if nodeBad[n.Name] {
					nv.Health = "degraded"
				}
				if fab, err := cl.Fabric(cctx, &nodev1.FabricRequest{}); err == nil {
					nv.Ports = portViews(fab.GetPorts())
				}
			}
			cancel()
		}
		v.Nodes = append(v.Nodes, nv)
	}
	cat := s.catalog
	v.Downloads = downloads(ctx, s.nodes)
	byDownload := map[string]*oafd.DownloadView{}
	for i, d := range v.Downloads {
		byDownload[d.Node+"/"+d.ModelID] = &v.Downloads[i]
	}
	deployed := map[string]bool{}
	unpinned := render.UnpinnedLocal(s.cluster, cat, s.versions)
	for _, d := range s.cluster.Spec.Models {
		dl := func(node string) *oafd.DownloadView { return byDownload[node+"/"+d.CatalogRef] }
		dv := deploymentState(s.cluster, cat, d, byProject[render.ModelProjectName(d.Name)], dl)
		if comp := unpinned[d.Name]; comp != "" {
			dv.Phase, dv.Detail = PhaseFailed, ""
			dv.Error = fmt.Sprintf("%s has no published image: push it to the control node's registry with `oaf images push <image> --component %s`; it starts after the next apply", comp, comp)
		}
		v.Deployments = append(v.Deployments, dv)
		deployed[render.ModelProjectName(d.Name)] = true
	}
	// A model removed from cluster.yaml whose containers an apply has not
	// taken down yet.
	removing := map[string][]string{}
	for _, st := range states {
		if reconcile.IsModelProject(st.Project) && !deployed[st.Project] && st.Exists {
			removing[st.Project] = append(removing[st.Project], st.Node)
		}
	}
	gone := make([]string, 0, len(removing))
	for p := range removing {
		gone = append(gone, p)
	}
	sort.Strings(gone)
	for _, p := range gone {
		v.Deployments = append(v.Deployments, oafd.DeploymentView{Name: strings.TrimPrefix(p, reconcile.ModelProjectPrefix),
			Nodes: removing[p], Guardrails: []string{}, State: PhaseRemoving, Phase: PhaseRemoving, Detail: "Stopping and removing its containers"})
	}
	for _, st := range states {
		if strings.HasPrefix(st.Project, "model-") {
			continue
		}
		if st.Err != nil {
			v.Components = append(v.Components, oafd.ComponentView{Name: st.Project, Node: st.Node, State: "unreachable", Health: st.Err.Error()})
			continue
		}
		if !st.Exists {
			v.Components = append(v.Components, oafd.ComponentView{Name: st.Project, Node: st.Node, State: "missing"})
			continue
		}
		for _, sv := range st.Services {
			v.Components = append(v.Components, oafd.ComponentView{Name: st.Project + "/" + sv.GetName(), Node: st.Node, State: sv.GetState(), Health: sv.GetHealth()})
		}
	}
	return v, nil
}

func deploymentState(c *config.Cluster, cat *config.Catalog, d config.ModelDeployment, sts []reconcile.ProjectState, dl func(string) *oafd.DownloadView) oafd.DeploymentView {
	nodes := c.PlacementNodes(d.Placement)
	if nodes == nil {
		nodes = []string{}
	}
	dv := oafd.DeploymentView{Name: d.Name, CatalogRef: d.CatalogRef, Profile: d.Profile, Nodes: nodes,
		Placement: oafd.PlacementView{Node: d.Placement.Node, Group: d.Placement.Group}, Enabled: d.IsEnabled(),
		GPUMemoryUtilization: d.GPUMemoryUtilization, MaxModelLen: d.MaxModelLen, Guardrails: append([]string{}, d.Guardrails...)}
	if m, ok := cat.Lookup(d.CatalogRef); ok {
		dv.Task = "chat"
		if m.IsEmbedding() {
			dv.Task = "embedding"
		}
	}
	if d.IsEnabled() {
		modelPhase(&dv, sts, dl)
	} else {
		dv.Phase = PhaseDisabled
	}
	switch {
	case !d.IsEnabled():
		dv.State = "disabled"
	case len(sts) == 0:
		dv.State = "pending"
	default:
		dv.State, dv.Health = "running", "healthy"
		for _, st := range sts {
			switch {
			case st.Err != nil:
				dv.State, dv.Health = "unreachable", st.Err.Error()
			case !st.Exists:
				dv.State, dv.Health = "missing", ""
			case !st.Healthy():
				dv.Health = "starting"
				for _, sv := range st.Services {
					if sv.GetState() != "running" {
						dv.State = sv.GetState()
					} else if h := sv.GetHealth(); h != "" && h != "healthy" {
						dv.Health = h
					}
				}
			}
		}
	}
	return dv
}

func portViews(ports []*nodev1.FabricPort) []oafd.PortView {
	var out []oafd.PortView
	for _, p := range ports {
		pv := oafd.PortView{Port: int(p.GetIndex()), Link: "down"}
		for _, h := range p.GetHalves() {
			rv := oafd.RailView{Netdev: h.GetName(), Status: h.GetOperState(), RDMADev: h.GetRdmaDev()}
			if len(h.GetAddrs()) > 0 {
				rv.IP = h.GetAddrs()[0]
			}
			if h.GetOperState() == "up" {
				pv.Link = "up"
				pv.SpeedGbps += float64(h.GetSpeedMbps()) / 1000
			}
			pv.Rails = append(pv.Rails, rv)
		}
		out = append(out, pv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// renderedGatewayConfig renders the gateway's config.yaml without
// generating secrets, for read-only display.
func renderedGatewayConfig(ctx context.Context, f *stackFlags) ([]byte, error) {
	s, err := f.load(io.Discard)
	if err != nil {
		return nil, err
	}
	projects, err := s.render(ctx, secrets.Preview{Store: secrets.Store{Dir: f.secretsDir}})
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Name != render.ProjectGateway {
			continue
		}
		for _, file := range p.Files {
			if file.Path == "config.yaml" {
				return file.Content, nil
			}
		}
	}
	return nil, fmt.Errorf("no gateway in the rendered stack")
}

// adminEnv is what an app needs to call the management API: the URL as the
// app reaches the control node, its token, and the certificate's SHA-256.
func adminEnv(f *stackFlags, c *config.Cluster, app, host string) (string, error) {
	store := secrets.Store{Dir: f.secretsDir}
	tok, err := store.Value(oafd.AdminSecretPrefix + app)
	if err != nil {
		return "", err
	}
	if host == "" {
		for _, n := range c.Spec.Nodes {
			if n.HasRole(config.RoleControl) {
				host = n.Address
				break
			}
		}
	}
	// The same certificate oafd serves (created now if oafd has not run yet).
	pair, err := apiCert(f.pkiDir, apiStateDir, c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "OAF_API_URL=https://%s\n", joinHostPort(host, config.PortOAFD))
	fmt.Fprintf(&b, "OAF_API_TOKEN=%s\n", tok)
	fmt.Fprintf(&b, "OAF_API_CERT_SHA256=%s\n", certFingerprint(pair.CertPEM))
	return b.String(), nil
}
