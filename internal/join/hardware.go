package join

import (
	"bufio"
	"context"
	"encoding/json"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"openagentfabric.dev/oaf/internal/fabric"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/preflight"
)

// Hardware is what a joining box reports: enough for the control node to
// recommend a topology and to tell the admin how to cable it.
type Hardware struct {
	Hostname        string  `json:"hostname"`
	Arch            string  `json:"arch"`
	Kernel          string  `json:"kernel,omitempty"`
	GPU             string  `json:"gpu,omitempty"`
	Driver          string  `json:"driver,omitempty"`
	Serial          string  `json:"serial,omitempty"`
	MemTotalGiB     float64 `json:"mem_total_gib"`
	MemAvailableGiB float64 `json:"mem_available_gib"`
	DataPath        string  `json:"data_path"`
	DiskFreeGiB     float64 `json:"disk_free_gib"`
	DiskTotalGiB    float64 `json:"disk_total_gib"`
	// Addresses are the box's IPv4 addresses outside the QSFP ports (LAN).
	Addresses []string `json:"addresses,omitempty"`
	// Ports are both QSFP ports with their two rails (netdevs) and carrier.
	Ports []fabric.Port `json:"ports,omitempty"`
	// Neighbors are the link-layer peers each QSFP netdev has seen (IPv6
	// link-local neighbour discovery runs as soon as a cable is up), which
	// is how the control node works out which box is on the other end.
	Neighbors map[string][]Neighbor `json:"neighbors,omitempty"`
	Preflight []preflight.Result    `json:"preflight,omitempty"`
	Worst     preflight.Status      `json:"worst,omitempty"`
}

// Neighbor is one peer seen on a netdev.
type Neighbor struct {
	MAC string `json:"mac"`
	IP  string `json:"ip,omitempty"`
}

// GB10 reports whether the box has the GB10 superchip OAF v1 targets.
func (h Hardware) GB10() bool { return strings.Contains(h.GPU, "GB10") }

// FreePorts are the QSFP ports nothing else configured (no routable address).
func (h Hardware) FreePorts() []int {
	var out []int
	for _, p := range h.Ports {
		if !p.InUse() {
			out = append(out, p.Index)
		}
	}
	return out
}

// Collect inspects this box read-only: preflight, GPU, memory, disk, serial,
// LAN addresses, QSFP ports and their link-layer neighbours.
func Collect(ctx context.Context, r hostexec.Runner, opts preflight.Options) Hardware {
	ro := hostexec.ReadOnly{Inner: r}
	rep := preflight.Run(ctx, r, opts)
	h := Basics(ctx, r, opts.DataPath)
	h.Hostname, h.Ports, h.Preflight, h.Worst = rep.Host, rep.Ports, rep.Results, rep.Worst()
	fabricDev := map[string]bool{}
	for _, p := range h.Ports {
		for _, nd := range p.Halves {
			fabricDev[nd.Name] = true
		}
	}
	if out, err := ro.Run(ctx, "ip", "-j", "addr", "show"); err == nil {
		h.Addresses = lanAddresses(out, fabricDev)
	}
	h.Neighbors = map[string][]Neighbor{}
	for _, p := range h.Ports {
		for _, nd := range p.Halves {
			if out, err := ro.Run(ctx, "ip", "-j", "neigh", "show", "dev", nd.Name); err == nil {
				if ns := neighbors(out); len(ns) > 0 {
					h.Neighbors[nd.Name] = ns
				}
			}
		}
	}
	return h
}

// Basics is the cheap part of Collect (no preflight): architecture, GPU,
// serial, memory and disk. The node agent reports it on every Info.
func Basics(ctx context.Context, r hostexec.Runner, dataPath string) Hardware {
	ro := hostexec.ReadOnly{Inner: r}
	h := Hardware{DataPath: dataPath}
	if out, err := ro.Run(ctx, "uname", "-m"); err == nil {
		h.Arch = strings.TrimSpace(string(out))
	}
	if out, err := ro.Run(ctx, "uname", "-r"); err == nil {
		h.Kernel = strings.TrimSpace(string(out))
	}
	if out, err := ro.Run(ctx, "nvidia-smi", "--query-gpu=name,driver_version", "--format=csv,noheader"); err == nil {
		name, drv, _ := strings.Cut(strings.TrimSpace(string(out)), ",")
		h.GPU, h.Driver = strings.TrimSpace(name), strings.TrimSpace(drv)
	}
	if out, err := ro.Run(ctx, "cat", "/sys/class/dmi/id/product_serial"); err == nil {
		h.Serial = strings.TrimSpace(string(out))
	}
	if out, err := ro.Run(ctx, "cat", "/proc/meminfo"); err == nil {
		kb := meminfo(string(out))
		h.MemTotalGiB, h.MemAvailableGiB = round1(kb["MemTotal"]/(1<<20)), round1(kb["MemAvailable"]/(1<<20))
	}
	if dataPath != "" {
		h.DiskFreeGiB, h.DiskTotalGiB = disk(ctx, ro, dataPath)
	}
	return h
}

func meminfo(s string) map[string]float64 {
	kb := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 {
			v, _ := strconv.ParseFloat(f[1], 64)
			kb[strings.TrimSuffix(f[0], ":")] = v
		}
	}
	return kb
}

func disk(ctx context.Context, r hostexec.Runner, p string) (free, total float64) {
	for {
		if _, err := r.Run(ctx, "ls", "-1", p); err == nil || p == "/" {
			break
		}
		if i := strings.LastIndex(strings.TrimSuffix(p, "/"), "/"); i > 0 {
			p = p[:i]
		} else {
			p = "/"
		}
	}
	out, err := r.Run(ctx, "df", "-B1", "--output=avail,size", p)
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) != 2 {
		return 0, 0
	}
	a, _ := strconv.ParseFloat(f[0], 64)
	s, _ := strconv.ParseFloat(f[1], 64)
	return round1(a / (1 << 30)), round1(s / (1 << 30))
}

func lanAddresses(raw []byte, skip map[string]bool) []string {
	var ifs []struct {
		IfName   string `json:"ifname"`
		AddrInfo []struct {
			Family    string `json:"family"`
			Local     string `json:"local"`
			Prefixlen int    `json:"prefixlen"`
			Scope     string `json:"scope"`
		} `json:"addr_info"`
	}
	if json.Unmarshal(raw, &ifs) != nil {
		return nil
	}
	var out []string
	for _, i := range ifs {
		if skip[i.IfName] || i.IfName == "lo" || strings.HasPrefix(i.IfName, "docker") || strings.HasPrefix(i.IfName, "br-") || strings.HasPrefix(i.IfName, "veth") {
			continue
		}
		for _, a := range i.AddrInfo {
			if a.Family != "inet" || a.Scope != "global" {
				continue
			}
			if ip, err := netip.ParseAddr(a.Local); err == nil && !ip.IsLoopback() {
				out = append(out, a.Local)
			}
		}
	}
	sort.Strings(out)
	return out
}

func neighbors(raw []byte) []Neighbor {
	var ns []struct {
		Dst    string   `json:"dst"`
		LLAddr string   `json:"lladdr"`
		State  []string `json:"state"`
	}
	if json.Unmarshal(raw, &ns) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Neighbor
	for _, n := range ns {
		mac := strings.ToLower(n.LLAddr)
		if mac == "" || seen[mac] {
			continue
		}
		failed := false
		for _, s := range n.State {
			if s == "FAILED" || s == "INCOMPLETE" {
				failed = true
			}
		}
		if failed {
			continue
		}
		seen[mac] = true
		out = append(out, Neighbor{MAC: mac, IP: n.Dst})
	}
	return out
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
