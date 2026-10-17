// Package fabric discovers and (later) configures the switchless QSFP fabric.
//
// A GB10 has two physical QSFP ports. The kernel exposes each one as two
// mlx5 netdevs, one per PCIe domain (port 0 = enp1s0f0np0 + enP2p1s0f0np0).
// Those halves are the port's two rails.
package fabric

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"path"
	"sort"
	"strconv"
	"strings"

	"openagentfabric.dev/oaf/internal/hostexec"
)

// maxGIDIndex bounds the GID table scan. Entries appear per address, so a
// netdev with a few IPs uses well under this many.
const maxGIDIndex = 16

// Port is one physical QSFP port.
type Port struct {
	Index  int      `json:"index"`
	Halves []Netdev `json:"halves"` // the two rails, ordered by PCI address
}

// Up reports whether every half has carrier.
func (p Port) Up() bool {
	for _, h := range p.Halves {
		if h.OperState != "up" {
			return false
		}
	}
	return len(p.Halves) > 0
}

// InUse reports whether any half carries a routable address, which means
// something else configured it. OAF must not reconfigure such a port unless
// told to explicitly.
func (p Port) InUse() bool {
	for _, h := range p.Halves {
		if h.InUse() {
			return true
		}
	}
	return false
}

// Netdev is one mlx5 network device (one PCIe half of a port).
type Netdev struct {
	Name      string         `json:"name"`
	PCI       string         `json:"pci"`
	OperState string         `json:"operState"`
	MTU       int            `json:"mtu"`
	SpeedMbps int            `json:"speedMbps"`
	MAC       string         `json:"mac"`
	Addrs     []netip.Prefix `json:"addrs,omitempty"` // excluding link-local
	RDMADev   string         `json:"rdmaDev,omitempty"`
	GIDs      []GID          `json:"gids,omitempty"`
}

// InUse reports whether the netdev has a routable address.
func (n Netdev) InUse() bool { return len(n.Addrs) > 0 }

// GID is one populated RoCE GID table entry.
type GID struct {
	Index int    `json:"index"`
	Type  string `json:"type"` // "RoCE v2" or "IB/RoCE v1"
	GID   string `json:"gid"`
}

// IPv4 returns the IPv4 address embedded in a v4-mapped GID.
func (g GID) IPv4() (netip.Addr, bool) {
	hex := strings.ReplaceAll(g.GID, ":", "")
	if len(hex) != 32 || !strings.HasPrefix(hex, "00000000000000000000ffff") {
		return netip.Addr{}, false
	}
	var b [4]byte
	for i := range b {
		v, err := strconv.ParseUint(hex[24+2*i:26+2*i], 16, 8)
		if err != nil {
			return netip.Addr{}, false
		}
		b[i] = byte(v)
	}
	return netip.AddrFrom4(b), true
}

// RoCEv2GIDFor returns the RoCE v2 GID index whose address is ip. The index
// is not stable (it depends on how many addresses the netdev has), so it must
// be looked up for the exact rail IP on every bring-up.
func (n Netdev) RoCEv2GIDFor(ip netip.Addr) (int, bool) {
	for _, g := range n.GIDs {
		if a, ok := g.IPv4(); ok && g.Type == "RoCE v2" && a == ip {
			return g.Index, true
		}
	}
	return 0, false
}

// Discover reads the node's QSFP ports. It only reads sysfs and `ip`.
func Discover(ctx context.Context, r hostexec.Runner) ([]Port, error) {
	names, err := lines(ctx, r, "ls", "-1", "/sys/class/net")
	if err != nil {
		return nil, err
	}
	addrs, err := ipAddrs(ctx, r)
	if err != nil {
		return nil, err
	}
	rdma := rdmaByNetdev(ctx, r)

	byPort := map[int][]Netdev{}
	for _, name := range names {
		base := "/sys/class/net/" + name
		drv, err := r.Run(ctx, "readlink", base+"/device/driver")
		if err != nil || path.Base(strings.TrimSpace(string(drv))) != "mlx5_core" {
			continue // not a ConnectX netdev (or a virtual device)
		}
		dev, err := r.Run(ctx, "readlink", base+"/device")
		if err != nil {
			return nil, err
		}
		phys := readTrim(ctx, r, base+"/phys_port_name") // "p0" / "p1"
		idx, err := strconv.Atoi(strings.TrimPrefix(phys, "p"))
		if err != nil || !strings.HasPrefix(phys, "p") {
			continue // representor or unknown layout
		}
		nd := Netdev{
			Name:      name,
			PCI:       path.Base(strings.TrimSpace(string(dev))),
			OperState: readTrim(ctx, r, base+"/operstate"),
			MTU:       atoi(readTrim(ctx, r, base+"/mtu")),
			SpeedMbps: atoi(readTrim(ctx, r, base+"/speed")),
			MAC:       readTrim(ctx, r, base+"/address"),
			Addrs:     addrs[name],
			RDMADev:   rdma[name],
		}
		if nd.RDMADev != "" {
			nd.GIDs = gids(ctx, r, nd.RDMADev)
		}
		byPort[idx] = append(byPort[idx], nd)
	}

	ports := make([]Port, 0, len(byPort))
	for idx, halves := range byPort {
		sort.Slice(halves, func(i, j int) bool { return halves[i].PCI < halves[j].PCI })
		ports = append(ports, Port{Index: idx, Halves: halves})
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Index < ports[j].Index })
	return ports, nil
}

func ipAddrs(ctx context.Context, r hostexec.Runner) (map[string][]netip.Prefix, error) {
	out, err := r.Run(ctx, "ip", "-j", "addr", "show")
	if err != nil {
		return nil, err
	}
	var ifs []struct {
		IfName   string `json:"ifname"`
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
			Scope     string `json:"scope"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &ifs); err != nil {
		return nil, fmt.Errorf("ip -j addr show: %w", err)
	}
	m := map[string][]netip.Prefix{}
	for _, i := range ifs {
		for _, a := range i.AddrInfo {
			if a.Scope == "link" || a.Scope == "host" {
				continue
			}
			ip, err := netip.ParseAddr(a.Local)
			if err != nil {
				continue
			}
			m[i.IfName] = append(m[i.IfName], netip.PrefixFrom(ip, a.PrefixLen))
		}
	}
	return m, nil
}

// rdmaByNetdev maps netdev → RDMA device using sysfs. Missing RDMA support
// is not an error; it shows up as netdevs without GIDs.
func rdmaByNetdev(ctx context.Context, r hostexec.Runner) map[string]string {
	m := map[string]string{}
	devs, err := lines(ctx, r, "ls", "-1", "/sys/class/infiniband")
	if err != nil {
		return m
	}
	for _, d := range devs {
		nets, err := lines(ctx, r, "ls", "-1", "/sys/class/infiniband/"+d+"/device/net")
		if err != nil {
			continue
		}
		for _, n := range nets {
			m[n] = d
		}
	}
	return m
}

func gids(ctx context.Context, r hostexec.Runner, dev string) []GID {
	var out []GID
	base := "/sys/class/infiniband/" + dev + "/ports/1"
	for i := 0; i < maxGIDIndex; i++ {
		// Unpopulated entries fail to read their type.
		t, err := r.Run(ctx, "cat", fmt.Sprintf("%s/gid_attrs/types/%d", base, i))
		if err != nil || strings.TrimSpace(string(t)) == "" {
			continue
		}
		g := readTrim(ctx, r, fmt.Sprintf("%s/gids/%d", base, i))
		out = append(out, GID{Index: i, Type: strings.TrimSpace(string(t)), GID: g})
	}
	return out
}

func lines(ctx context.Context, r hostexec.Runner, name string, args ...string) ([]string, error) {
	out, err := r.Run(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res, nil
}

func readTrim(ctx context.Context, r hostexec.Runner, file string) string {
	out, err := r.Run(ctx, "cat", file)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
