package preflight

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"openagentfabric.dev/oaf/internal/fabric"
)

// checks run in this order and are reported in this order.
var checks = []check{
	{"host", checkHost},
	{"gpu", checkGPU},
	{"memory", checkMemory},
	{"disk", checkDisk},
	{"docker", checkDocker},
	{"nvidia-runtime", checkNvidiaRuntime},
	{"workloads", checkWorkloads},
	{"ports", checkPorts},
	{"fabric", checkFabric},
	{"rdma-tools", checkRDMATools},
	{"nccl", checkNCCL},
	{"time", checkTime},
}

// hostPorts are the host ports the OAF stack listens on. Provisional until
// the compose renderers own the port map (docs/PLAN.md M1).
var hostPorts = map[int]string{
	4000:  "agentgateway (API)",
	15000: "agentgateway (admin)",
	8899:  "vSR",
	3000:  "Langfuse web",
	3001:  "Grafana",
	9090:  "Prometheus",
	9100:  "node_exporter",
	8443:  "oafd",
	7701:  "oaf-node",
}

func trim(b []byte) string { return strings.TrimSpace(string(b)) }

func checkHost(ctx context.Context, e *env) Result {
	arch, err := e.r.Run(ctx, "uname", "-m")
	if err != nil {
		return Result{Status: Fail, Summary: "cannot read architecture: " + err.Error()}
	}
	kernel, _ := e.r.Run(ctx, "uname", "-r")
	osr := keyValues(ctx, e, "/etc/os-release")
	dgx := keyValues(ctx, e, "/etc/dgx-release")
	res := Result{Details: []string{
		"os: " + osr["PRETTY_NAME"],
		"kernel: " + trim(kernel),
	}}
	if dgx["DGX_PRETTY_NAME"] != "" {
		res.Details = append(res.Details, fmt.Sprintf("dgx: %s (build %s)", dgx["DGX_PRETTY_NAME"], dgx["DGX_SWBUILD_DATE"]))
	}
	switch {
	case trim(arch) != "aarch64":
		res.Status, res.Summary = Fail, fmt.Sprintf("architecture %s; OAF v1 targets GB10 (aarch64)", trim(arch))
	case dgx["DGX_NAME"] == "":
		res.Status, res.Summary = Warn, "aarch64, but not DGX OS (no /etc/dgx-release); untested platform"
	default:
		res.Status, res.Summary = Pass, fmt.Sprintf("%s on %s", dgx["DGX_NAME"], osr["PRETTY_NAME"])
	}
	return res
}

func keyValues(ctx context.Context, e *env, file string) map[string]string {
	m := map[string]string{}
	out, err := e.r.Run(ctx, "cat", file)
	if err != nil {
		return m
	}
	for _, l := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok {
			m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return m
}

func checkGPU(ctx context.Context, e *env) Result {
	out, err := e.r.Run(ctx, "nvidia-smi", "--query-gpu=name,driver_version", "--format=csv,noheader")
	if err != nil {
		return Result{Status: Fail, Summary: "nvidia-smi failed: " + err.Error()}
	}
	name, driver, _ := strings.Cut(trim(out), ",")
	name, driver = strings.TrimSpace(name), strings.TrimSpace(driver)
	if !strings.Contains(name, "GB10") {
		return Result{Status: Warn, Summary: fmt.Sprintf("GPU %q (driver %s); OAF v1 targets GB10", name, driver)}
	}
	return Result{Status: Pass, Summary: fmt.Sprintf("%s, driver %s", name, driver)}
}

// checkMemory uses /proc/meminfo: GB10 memory is unified, and nvidia-smi
// does not report its usage.
func checkMemory(ctx context.Context, e *env) Result {
	out, err := e.r.Run(ctx, "cat", "/proc/meminfo")
	if err != nil {
		return Result{Status: Fail, Summary: "cannot read /proc/meminfo"}
	}
	kb := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 {
			v, _ := strconv.ParseFloat(f[1], 64)
			kb[strings.TrimSuffix(f[0], ":")] = v
		}
	}
	total, avail := kb["MemTotal"]/(1<<20), kb["MemAvailable"]/(1<<20)
	res := Result{Summary: fmt.Sprintf("%.1f GiB available of %.1f GiB unified memory", avail, total)}
	if avail < e.opts.MinAvailableMemGiB {
		res.Status = Warn
		res.Details = []string{fmt.Sprintf("below %.0f GiB: other workloads hold most of the memory; any OAF vLLM here needs a small --gpu-memory-utilization, or the memory must be freed first", e.opts.MinAvailableMemGiB)}
		return res
	}
	res.Status = Pass
	return res
}

func checkDisk(ctx context.Context, e *env) Result {
	// The data path may not exist yet; measure the nearest existing parent.
	p := e.opts.DataPath
	for {
		if _, err := e.r.Run(ctx, "ls", "-1", p); err == nil || p == "/" {
			break
		}
		if i := strings.LastIndex(strings.TrimSuffix(p, "/"), "/"); i > 0 {
			p = p[:i]
		} else {
			p = "/"
		}
	}
	out, err := e.r.Run(ctx, "df", "-B1", "--output=avail,size", p)
	if err != nil {
		return Result{Status: Fail, Summary: "df failed: " + err.Error()}
	}
	f := strings.Fields(lastLine(trim(out)))
	if len(f) != 2 {
		return Result{Status: Fail, Summary: "unexpected df output"}
	}
	avail, _ := strconv.ParseFloat(f[0], 64)
	size, _ := strconv.ParseFloat(f[1], 64)
	availGiB, sizeGiB := avail/(1<<30), size/(1<<30)
	res := Result{Summary: fmt.Sprintf("%.0f GiB free of %.0f GiB at %s", availGiB, sizeGiB, p)}
	if availGiB < e.opts.MinFreeDiskGiB {
		res.Status = Warn
		res.Details = []string{fmt.Sprintf("below %.0f GiB: room for few models; every pull checks space first", e.opts.MinFreeDiskGiB)}
	} else {
		res.Status = Pass
	}
	return res
}

func lastLine(s string) string {
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func checkDocker(ctx context.Context, e *env) Result {
	server, err := e.r.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return Result{Status: Fail, Summary: "docker engine not reachable: " + err.Error()}
	}
	compose, err := e.r.Run(ctx, "docker", "compose", "version", "--short")
	if err != nil {
		return Result{Status: Fail, Summary: "docker engine " + trim(server) + ", but the compose plugin is missing"}
	}
	return Result{Status: Pass, Summary: fmt.Sprintf("engine %s, compose %s", trim(server), trim(compose))}
}

// checkNvidiaRuntime accepts either GPU path docker offers: CDI devices
// (docker >= 28 with nvidia-ctk CDI specs, as on DGX OS) or the legacy
// registered "nvidia" runtime.
func checkNvidiaRuntime(ctx context.Context, e *env) Result {
	ctk, _ := e.r.Run(ctx, "nvidia-ctk", "--version")
	details := []string{firstLine(trim(ctk))}

	devs, err := e.r.Run(ctx, "docker", "info", "--format", "{{json .DiscoveredDevices}}")
	if err == nil {
		var dd []struct{ Source, ID string }
		_ = json.Unmarshal(devs, &dd)
		for _, d := range dd {
			if d.Source == "cdi" && d.ID == "nvidia.com/gpu=all" {
				return Result{Status: Pass, Summary: "GPU available to containers via CDI (nvidia.com/gpu=all)", Details: details}
			}
		}
	}
	rts, err := e.r.Run(ctx, "docker", "info", "--format", "{{range $k, $v := .Runtimes}}{{$k}} {{end}}")
	if err != nil {
		return Result{Status: Fail, Summary: "docker info failed: " + err.Error(), Details: details}
	}
	for _, rt := range strings.Fields(trim(rts)) {
		if rt == "nvidia" {
			return Result{Status: Pass, Summary: "GPU available to containers via the nvidia runtime", Details: details}
		}
	}
	return Result{Status: Fail, Summary: "containers cannot get the GPU: no CDI nvidia.com/gpu devices and no nvidia runtime (configure the NVIDIA Container Toolkit)", Details: details}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

type container struct {
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Labels string `json:"Labels"`
}

func (c container) ownedByOAF() bool {
	return strings.Contains(c.Labels, "com.docker.compose.project=oaf-")
}

func (c container) looksLikeInference() bool {
	s := strings.ToLower(c.Names + " " + c.Image)
	for _, k := range []string{"vllm", "sglang", "tensorrt", "trtllm", "ollama", "llama.cpp", "nim"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func checkWorkloads(ctx context.Context, e *env) Result {
	out, err := e.r.Run(ctx, "docker", "ps", "-a", "--format", "{{json .}}")
	if err != nil {
		return Result{Status: Fail, Summary: "cannot list containers: " + err.Error()}
	}
	var running, inference, oaf []string
	for _, l := range strings.Split(trim(out), "\n") {
		if l == "" {
			continue
		}
		var c container
		if json.Unmarshal([]byte(l), &c) != nil || c.State != "running" {
			continue
		}
		desc := fmt.Sprintf("%s (%s, %s)", c.Names, c.Image, c.Status)
		switch {
		case c.ownedByOAF():
			oaf = append(oaf, desc)
		case c.looksLikeInference():
			inference = append(inference, desc)
		default:
			running = append(running, desc)
		}
	}
	res := Result{Status: Pass, Summary: "no other containers running"}
	for _, c := range inference {
		res.Details = append(res.Details, "inference: "+c)
	}
	for _, c := range running {
		res.Details = append(res.Details, "other: "+c)
	}
	for _, c := range oaf {
		res.Details = append(res.Details, "oaf: "+c)
	}
	switch {
	case len(inference) > 0:
		res.Status = Warn
		res.Summary = fmt.Sprintf("%d existing inference container(s) share the GPU and unified memory; OAF will not touch them", len(inference))
	case len(running) > 0:
		res.Status = Info
		res.Summary = fmt.Sprintf("%d other container(s) running; OAF will not touch them", len(running))
	case len(oaf) > 0:
		res.Summary = fmt.Sprintf("%d OAF container(s) running", len(oaf))
	}
	return res
}

func checkPorts(ctx context.Context, e *env) Result {
	out, err := e.r.Run(ctx, "ss", "-Hltn")
	if err != nil {
		return Result{Status: Fail, Summary: "cannot list listening sockets: " + err.Error()}
	}
	busy := map[int]bool{}
	for _, l := range strings.Split(trim(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		if i := strings.LastIndex(local, ":"); i >= 0 {
			if p, err := strconv.Atoi(local[i+1:]); err == nil {
				busy[p] = true
			}
		}
	}
	var clashes []string
	for p, what := range hostPorts {
		if busy[p] {
			clashes = append(clashes, fmt.Sprintf("%d (%s)", p, what))
		}
	}
	sort.Strings(clashes)
	if len(clashes) > 0 {
		return Result{Status: Warn, Summary: fmt.Sprintf("%d port(s) OAF uses are already taken; they will need remapping on this node", len(clashes)), Details: clashes}
	}
	return Result{Status: Pass, Summary: "all OAF host ports are free"}
}

func checkFabric(ctx context.Context, e *env) Result {
	ports, err := fabric.Discover(ctx, e.r)
	if err != nil {
		return Result{Status: Fail, Summary: "fabric discovery failed: " + err.Error()}
	}
	e.ports = ports
	if len(ports) == 0 {
		return Result{Status: Fail, Summary: "no ConnectX (mlx5) QSFP ports found"}
	}
	res := Result{Status: Pass}
	free := 0
	for _, p := range ports {
		state := "no carrier"
		if p.Up() {
			state = "carrier"
		}
		use := "unconfigured"
		if p.InUse() {
			use = "IN USE: OAF will not reconfigure it"
		} else if p.Up() {
			free++
		}
		res.Details = append(res.Details, fmt.Sprintf("port %d: %s, %s", p.Index, state, use))
		for _, h := range p.Halves {
			res.Details = append(res.Details, "  "+describeHalf(h))
		}
		if len(p.Halves) != 2 {
			res.Status = Warn
			res.Details = append(res.Details, fmt.Sprintf("  expected 2 PCIe halves, found %d", len(p.Halves)))
		}
	}
	res.Summary = fmt.Sprintf("%d physical port(s); %d cabled and free for OAF", len(ports), free)
	if free == 0 && res.Status == Pass {
		res.Status = Info
	}
	return res
}

func describeHalf(h fabric.Netdev) string {
	s := fmt.Sprintf("%s (%s) %s mtu %d %dG", h.Name, h.PCI, h.OperState, h.MTU, h.SpeedMbps/1000)
	for _, a := range h.Addrs {
		s += " " + a.String()
		if a.Addr().Is4() {
			if idx, ok := h.RoCEv2GIDFor(a.Addr()); ok {
				s += fmt.Sprintf(" [RoCE v2 GID %d]", idx)
			} else {
				s += " [no RoCE v2 GID]"
			}
		}
	}
	return s
}

func checkRDMATools(ctx context.Context, e *env) Result {
	if _, err := e.r.Run(ctx, "which", "ib_write_bw"); err != nil {
		return Result{Status: Warn, Summary: "ib_write_bw (perftest) not installed; rail bandwidth cannot be measured"}
	}
	return Result{Status: Pass, Summary: "ib_write_bw available"}
}

// checkNCCL looks for the system libnccl that vLLM on GB10 is LD_PRELOADed
// with (CLAUDE.md). Absence is a warning: some vLLM images bundle their own.
func checkNCCL(ctx context.Context, e *env) Result {
	out, err := e.r.Run(ctx, "ldconfig", "-p")
	if err != nil {
		return Result{Status: Warn, Summary: "cannot query the linker cache"}
	}
	var libs []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "libnccl.so") {
			if _, p, ok := strings.Cut(l, "=> "); ok {
				libs = append(libs, strings.TrimSpace(p))
			}
		}
	}
	if len(libs) == 0 {
		return Result{Status: Warn, Summary: "no system libnccl; the LD_PRELOAD workaround needs it unless the vLLM image ships a working NCCL (decided in M2)"}
	}
	return Result{Status: Pass, Summary: "system libnccl present", Details: libs}
}

func checkTime(ctx context.Context, e *env) Result {
	out, err := e.r.Run(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	if err != nil {
		return Result{Status: Warn, Summary: "cannot query time sync"}
	}
	if trim(out) != "yes" {
		return Result{Status: Warn, Summary: "clock not NTP-synchronised; traces and certificates need agreeing clocks"}
	}
	return Result{Status: Pass, Summary: "NTP synchronised"}
}
