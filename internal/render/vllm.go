package render

import (
	"fmt"
	"maps"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
)

// models renders one project per enabled deployment per node. A deployment
// that spans a group runs one vLLM per node: rank 0 serves the API, the others
// run headless, and they rendezvous over the QSFP link (torch.distributed,
// no Ray).
func (r *renderer) models() ([]Project, error) {
	var out []Project
	unpinned := UnpinnedLocal(r.Cluster, r.Catalog, r.Versions)
	for i, d := range r.Cluster.Spec.Models {
		if !d.IsEnabled() || unpinned[d.Name] != "" {
			continue
		}
		ps, err := r.model(i, d)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", d.Name, err)
		}
		out = append(out, ps...)
	}
	return out, nil
}

func (r *renderer) model(i int, d config.ModelDeployment) ([]Project, error) {
	v := r.Versions.Version
	m, ok := r.Catalog.Lookup(d.CatalogRef)
	if !ok {
		return nil, fmt.Errorf("no catalog entry %q", d.CatalogRef)
	}
	nodes := r.Cluster.PlacementNodes(d.Placement)
	prof, err := config.Profile(d, m, len(nodes))
	if err != nil {
		return nil, err
	}
	img, err := r.image(prof.EngineComponent())
	if err != nil {
		return nil, err
	}
	var ranks []rankNet
	if len(nodes) > 1 {
		if ranks, err = r.rankNetworks(nodes); err != nil {
			return nil, err
		}
	}
	apiKey, err := r.secret(SecretVLLMAPIKey)
	if err != nil {
		return nil, err
	}

	weights := path.Join("/models", m.ID)
	maxLen := prof.MaxModelLen
	if d.MaxModelLen != 0 {
		maxLen = d.MaxModelLen
	}
	adapters := d.ServedAdapters(r.Features)
	var out []Project
	pulls := ModelWeights(r.Cluster, r.Catalog, d)
	for rank, nodeName := range nodes {
		node, _ := r.Cluster.Node(nodeName)
		env := newEnvFile()

		args := []string{
			weights,
			"--served-model-name", d.Name,
			"--gpu-memory-utilization", strconv.FormatFloat(d.GPUMemoryUtilization, 'f', -1, 64),
			"--tensor-parallel-size", strconv.Itoa(prof.TensorParallel),
		}
		if pp := prof.PP(); pp > 1 {
			args = append(args, "--pipeline-parallel-size", strconv.Itoa(pp))
		}
		if maxLen != 0 {
			args = append(args, "--max-model-len", strconv.Itoa(maxLen))
		}
		if prof.Quantization != "" {
			args = append(args, "--quantization", prof.Quantization)
		}
		if len(nodes) > 1 {
			args = append(args,
				"--nnodes", strconv.Itoa(len(nodes)),
				"--node-rank", strconv.Itoa(rank),
				"--master-addr", ranks[0].ip.String(),
				"--master-port", strconv.Itoa(config.MasterPort(i)))
		}
		if rank == 0 {
			args = append(args, "--host", "0.0.0.0", "--port", strconv.Itoa(r.Cluster.ModelPort(i)))
		} else {
			args = append(args, "--headless")
		}
		args = append(args, prof.Args...)
		args = append(args, d.Args...)
		args = append(args, loraArgs(adapters, args)...)

		cenv := map[string]string{
			// Weights are pulled and verified by oaf (M2), never by vLLM at runtime.
			"HF_HUB_OFFLINE":       "1",
			"TRANSFORMERS_OFFLINE": "1",
		}
		if rank == 0 {
			cenv["VLLM_API_KEY"] = env.ref("VLLM_API_KEY", apiKey)
		}

		s := newService(img)
		s.NetworkMode = "host" // NCCL/RDMA and the API port need the host network
		s.GPUs = "all"         // a CDI device request on DGX OS; never runtime: nvidia
		s.ShmSize = "10gb"
		s.Command = args
		s.Volumes = []string{path.Join(modelsPath(node), m.ID) + ":" + weights + ":ro"}
		for _, a := range adapters {
			s.Volumes = append(s.Volumes, a.Path+":"+adapterMount(a.Name)+":ro")
		}
		if p := r.Cluster.Spec.Fabric.NCCL.LDPreload; p != "" {
			cenv["LD_PRELOAD"] = p
			s.Volumes = append(s.Volumes, p+":"+p+":ro")
		}
		if len(nodes) > 1 {
			maps.Copy(cenv, r.ncclEnv(ranks[rank]))
			s.Ulimits = map[string]int{"memlock": -1}
			if r.transport() == config.TransportIB {
				s.Devices = []string{"/dev/infiniband:/dev/infiniband"}
			}
		}
		maps.Copy(cenv, r.Cluster.Spec.Fabric.NCCL.Env)
		maps.Copy(cenv, prof.Env)
		maps.Copy(cenv, d.Env)
		s.Environment = cenv
		s.StopGracePeriod = "60s"
		if rank == 0 {
			// Large models take many minutes to load; start_period covers that.
			probe := fmt.Sprintf("import urllib.request; urllib.request.urlopen('http://127.0.0.1:%d/health', timeout=5)", r.Cluster.ModelPort(i))
			s.Healthcheck = &healthcheck{Test: []string{"CMD", "python3", "-c", probe}, Interval: "30s", Timeout: "10s", Retries: 3, StartPeriod: "30m"}
		}

		cf := &composeFile{Services: map[string]*service{"vllm": s}}
		applyContainer(cf, s, prof.Container)
		if rank > 0 {
			var prefix []string
			if prof.Container != nil {
				prefix = prof.Container.CommandPrefix
			}
			guardFollower(s, prefix, ranks[0].ip.String(), config.MasterPort(i))
		}
		p, err := finish(Project{Node: nodeName, Name: modelProjectName(d.Name)}, v, cf, env)
		if err != nil {
			return nil, err
		}
		p.Weights = pulls[nodeName]
		out = append(out, p)
	}
	return out, nil
}

// loraRanks are the --max-lora-rank values vLLM accepts (LoRAConfig).
var loraRanks = []int{1, 8, 16, 32, 64, 128, 256, 320, 512}

// loraArgs serves the adapters from the base model's vLLM: LoRA on, buffers
// sized for the largest rank (rounded up to a size vLLM accepts), a slot per
// adapter so one batch can mix them, and each adapter under its own name.
// Flags the catalog profile or the deployment already set are left to them.
func loraArgs(adapters []config.Adapter, have []string) []string {
	if len(adapters) == 0 {
		return nil
	}
	var out []string
	if !hasFlag(have, "--enable-lora") {
		out = append(out, "--enable-lora")
	}
	if !hasFlag(have, "--max-lora-rank") {
		rank := 0
		for _, a := range adapters {
			rank = max(rank, a.Rank)
		}
		for _, r := range loraRanks {
			if r >= rank {
				rank = r
				break
			}
		}
		out = append(out, "--max-lora-rank", strconv.Itoa(rank))
	}
	if len(adapters) > 1 && !hasFlag(have, "--max-loras") {
		out = append(out, "--max-loras", strconv.Itoa(len(adapters)))
	}
	out = append(out, "--lora-modules")
	for _, a := range adapters {
		out = append(out, a.Name+"="+adapterMount(a.Name))
	}
	return out
}

// hasFlag reports whether args set flag, as "--flag value" or "--flag=value".
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// adapterMount is where an adapter is mounted in the vLLM container. Served
// names may hold '/' and '.', so they are slugged like project names.
func adapterMount(name string) string {
	return "/adapters/" + strings.TrimPrefix(ModelProjectName(name), modelProjectPrefix)
}

// applyContainer adds what the engine image needs on top of OAF's defaults.
// Caches become named volumes, so JIT and compile results survive restarts
// (a cold b12x start autotunes for about ten minutes).
func applyContainer(cf *composeFile, s *service, c *config.Container) {
	if c == nil {
		return
	}
	s.Command = append(slices.Clone(c.CommandPrefix), s.Command...)
	s.IPC = c.IPC
	if c.ShmSize != "" {
		s.ShmSize = c.ShmSize
	}
	if len(c.Ulimits) > 0 {
		if s.Ulimits == nil {
			s.Ulimits = map[string]int{}
		}
		maps.Copy(s.Ulimits, c.Ulimits)
	}
	s.SecurityOpt = c.SecurityOpt
	for _, dir := range c.Caches {
		name := cacheVolume(dir)
		if cf.Volumes == nil {
			cf.Volumes = map[string]map[string]string{}
		}
		cf.Volumes[name] = map[string]string{}
		s.Volumes = append(s.Volumes, name+":"+dir)
	}
}

// cacheVolume names the volume for a container path: /root/.cache/b12x →
// cache-root-cache-b12x.
func cacheVolume(dir string) string {
	f := strings.FieldsFunc(dir, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	return "cache-" + strings.Join(f, "-")
}

func modelsPath(n config.Node) string {
	if n.Storage != nil && n.Storage.ModelsPath != "" {
		return n.Storage.ModelsPath
	}
	return DefaultModelsPath
}

// transport resolves fabric.nccl.transport: auto is sockets on a triangle
// (RDMA cannot cross a mesh hop) and RoCE everywhere else.
func (r *renderer) transport() string {
	t := r.Cluster.Spec.Fabric.NCCL.Transport
	if t == "" || t == config.TransportAuto {
		if r.Cluster.Spec.Fabric.Topology == config.TopologyTriangle {
			return config.TransportSocket
		}
		return config.TransportIB
	}
	return t
}

// rankNet is one rank's view of the link it uses: both rails of its port.
type rankNet struct {
	ip      netip.Addr // rail 0 address, the rendezvous/bootstrap address
	netdevs []string
	rdma    []string
	gid     int
}

func (r *renderer) ncclEnv(n rankNet) map[string]string {
	e := map[string]string{
		"VLLM_HOST_IP":       n.ip.String(),
		"NCCL_SOCKET_IFNAME": n.netdevs[0],
		"GLOO_SOCKET_IFNAME": n.netdevs[0],
	}
	if r.transport() == config.TransportSocket {
		e["NCCL_IB_DISABLE"] = "1"
		return e
	}
	e["NCCL_IB_DISABLE"] = "0"
	e["NCCL_IB_HCA"] = strings.Join(n.rdma, ",")
	e["NCCL_IB_GID_INDEX"] = strconv.Itoa(n.gid)
	return e
}

// rankNetworks finds, for a two-node group, the cable between the nodes and
// each end's rails. Larger groups span a triangle and need per-peer routing,
// which is not supported yet.
func (r *renderer) rankNetworks(nodes []string) ([]rankNet, error) {
	if len(nodes) != 2 {
		return nil, fmt.Errorf("deployments across %d nodes are not supported yet (only pairs)", len(nodes))
	}
	var link *config.Link
	for i := range r.Cluster.Spec.Fabric.Links {
		l := &r.Cluster.Spec.Fabric.Links[i]
		if l.A.Node == nodes[0] && l.B.Node == nodes[1] || l.A.Node == nodes[1] && l.B.Node == nodes[0] {
			link = l
			break
		}
	}
	if link == nil {
		return nil, fmt.Errorf("no fabric link between %s and %s", nodes[0], nodes[1])
	}
	out := make([]rankNet, len(nodes))
	for rank, name := range nodes {
		end := link.A
		if link.B.Node == name {
			end = link.B
		}
		rn, err := r.railsOf(name, end, link.Subnets)
		if err != nil {
			return nil, err
		}
		out[rank] = rn
	}
	return out, nil
}

func (r *renderer) railsOf(node string, end config.LinkEnd, subnets []string) (rankNet, error) {
	if end.Port == nil {
		return rankNet{}, fmt.Errorf("the link end on %s has no port; run cabling discovery to fill in fabric.links", node)
	}
	ports, ok := r.Fabric[node]
	if !ok {
		return rankNet{}, fmt.Errorf("no fabric discovery for node %s", node)
	}
	e, err := fabric.ResolveEnd(node, ports, *end.Port, subnets, r.Cluster.Spec.Fabric.RoCE.GIDIndex)
	if err != nil {
		return rankNet{}, err
	}
	ib := r.transport() == config.TransportIB
	rn := rankNet{ip: e.Rails[0].Addr.Addr()}
	for h, rail := range e.Rails {
		if ib {
			if rail.RDMADev == "" {
				return rankNet{}, fmt.Errorf("%s on %s has no RDMA device", rail.Netdev, node)
			}
			if rail.GID < 0 {
				return rankNet{}, fmt.Errorf("%s on %s: no RoCE v2 GID for %s", rail.Netdev, node, rail.Addr.Addr())
			}
			if h == 0 {
				rn.gid = rail.GID
			} else if rail.GID != rn.gid {
				// NCCL takes one GID index for all HCAs.
				return rankNet{}, fmt.Errorf("the rails of port %d on %s have different RoCE v2 GID indexes (%d, %d); set fabric.roce.gidIndex", e.Port, node, rn.gid, rail.GID)
			}
		}
		rn.netdevs = append(rn.netdevs, rail.Netdev)
		rn.rdma = append(rn.rdma, rail.RDMADev)
	}
	return rn, nil
}
