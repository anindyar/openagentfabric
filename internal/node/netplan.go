package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"openagentfabric.dev/oaf/internal/fabric"
	"openagentfabric.dev/oaf/internal/hostexec"
)

// Fabric addressing (backlog G1). OAF owns exactly one netplan file and
// nothing else in /etc/netplan: each rail is a QSFP netdev with one /30
// address, MTU 9000, no DHCP and no IPv6 link-local (which keeps the RoCE
// GID table small). The file has the shape the user guide told people to
// write by hand, so a hand-written one at the same path is adopted.

// NetplanFile is OAF's netplan file.
const NetplanFile = "/etc/netplan/40-oaf-fabric.yaml"

// DefaultFabricMTU is the rail MTU (CLAUDE.md: MTU 9000).
const DefaultFabricMTU = 9000

// RailSpec is one rail as ConfigureFabric is asked for it.
type RailSpec struct {
	Netdev string
	Addr   netip.Prefix
	MTU    int
}

// RailStatus is one rail after ConfigureFabric.
type RailStatus struct {
	Netdev    string
	Addr      netip.Prefix
	Present   bool
	MTU       int
	OperState string
}

// FabricResult is what ConfigureFabric did.
type FabricResult struct {
	Changed  bool
	Rails    []RailStatus
	Warnings []string
}

type netplanDoc struct {
	Network struct {
		Version   int                        `yaml:"version"`
		Renderer  string                     `yaml:"renderer,omitempty"`
		Ethernets map[string]netplanEthernet `yaml:"ethernets"`
	} `yaml:"network"`
}

type netplanEthernet struct {
	DHCP4     bool     `yaml:"dhcp4"`
	DHCP6     bool     `yaml:"dhcp6"`
	LinkLocal []string `yaml:"link-local"`
	MTU       int      `yaml:"mtu,omitempty"`
	Addresses []string `yaml:"addresses,omitempty"`
}

// renderNetplan is OAF's netplan file for rails (sorted, so the same rails
// always give the same bytes).
func renderNetplan(rails []RailSpec) []byte {
	sorted := append([]RailSpec(nil), rails...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Netdev < sorted[j].Netdev })
	var b bytes.Buffer
	b.WriteString("# Written by OAF (oaf-node): the QSFP fabric rails in cluster.yaml's fabric.links.\n")
	b.WriteString("# Do not edit: `oaf apply` rewrites this file. Other netplan files are never touched.\n")
	b.WriteString("network:\n  version: 2\n  renderer: NetworkManager\n  ethernets:\n")
	for _, r := range sorted {
		mtu := r.MTU
		if mtu == 0 {
			mtu = DefaultFabricMTU
		}
		fmt.Fprintf(&b, "    %s: {dhcp4: false, dhcp6: false, link-local: [], mtu: %d, addresses: [%s]}\n", r.Netdev, mtu, r.Addr)
	}
	return b.Bytes()
}

// parseNetplan reads the addresses OAF's file assigns, by netdev.
func parseNetplan(b []byte) (map[string][]netip.Prefix, error) {
	var d netplanDoc
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	out := map[string][]netip.Prefix{}
	for nd, e := range d.Network.Ethernets {
		for _, a := range e.Addresses {
			p, err := netip.ParsePrefix(a)
			if err != nil {
				return nil, fmt.Errorf("%s: address %q: %w", nd, a, err)
			}
			out[nd] = append(out[nd], p)
		}
	}
	return out, nil
}

func (s *Server) netplanPath() string {
	if s.NetplanFile != "" {
		return s.NetplanFile
	}
	return NetplanFile
}

func (s *Server) discoverPorts(ctx context.Context) ([]fabric.Port, error) {
	if s.DiscoverFabric != nil {
		return s.DiscoverFabric(ctx)
	}
	return fabric.Discover(ctx, s.readOnly())
}

// ConfigureFabricRails makes this node's rails exactly rails (an empty set
// removes OAF's file). It refuses a netdev that is not a QSFP netdev of
// this box, or that carries an address neither OAF's file nor the request
// gives it: something else configured that port. Unchanged rails that are
// already live mean no `netplan apply`.
func (s *Server) ConfigureFabricRails(ctx context.Context, rails []RailSpec, dryRun bool) (FabricResult, error) {
	var res FabricResult
	if len(rails) == 0 {
		return s.removeFabric(ctx, dryRun)
	}
	ports, err := s.discoverPorts(ctx)
	if err != nil {
		return res, fmt.Errorf("fabric discovery: %w", err)
	}
	byName := map[string]fabric.Netdev{}
	for _, p := range ports {
		for _, h := range p.Halves {
			byName[h.Name] = h
		}
	}
	path := s.netplanPath()
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	owned := map[string][]netip.Prefix{}
	if len(current) > 0 {
		if owned, err = parseNetplan(current); err != nil {
			return res, fmt.Errorf("%s: %w", path, err)
		}
	}
	seen := map[string]bool{}
	for i, r := range rails {
		nd, ok := byName[r.Netdev]
		switch {
		case !ok:
			return res, fmt.Errorf("%s is not a QSFP netdev of this box", r.Netdev)
		case seen[r.Netdev]:
			return res, fmt.Errorf("%s is given twice", r.Netdev)
		case !r.Addr.IsValid() || !r.Addr.Addr().Is4():
			return res, fmt.Errorf("%s: %q is not an IPv4 address with its prefix", r.Netdev, r.Addr)
		case r.MTU != 0 && (r.MTU < 1280 || r.MTU > 9216):
			return res, fmt.Errorf("%s: MTU %d is out of range", r.Netdev, r.MTU)
		}
		seen[r.Netdev] = true
		if r.MTU == 0 {
			rails[i].MTU = DefaultFabricMTU
		}
		for _, a := range nd.Addrs {
			if a == r.Addr || containsPrefix(owned[r.Netdev], a) {
				continue
			}
			return res, fmt.Errorf("%s carries %s, which OAF did not assign: something else configured this port; remove that address or leave the port out of fabric.links", r.Netdev, a)
		}
	}
	want := []byte(nil)
	if len(rails) > 0 {
		want = renderNetplan(rails)
	}
	live := true
	for _, r := range rails {
		nd := byName[r.Netdev]
		if !containsPrefix(nd.Addrs, r.Addr) || nd.MTU != railMTU(r) {
			live = false
		}
	}
	fileSame := bytes.Equal(current, want)
	if fileSame && live {
		res.Rails = railStatuses(rails, byName)
		return res, nil
	}
	res.Changed = true
	if dryRun {
		for _, r := range rails {
			res.Rails = append(res.Rails, RailStatus{Netdev: r.Netdev, Addr: r.Addr, Present: true, MTU: r.MTU, OperState: byName[r.Netdev].OperState})
		}
		return res, nil
	}
	if s.Host == nil {
		return res, errors.New("this agent does not change host settings")
	}
	if !fileSame {
		if len(want) == 0 {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return res, err
			}
		} else if err := writeFileAtomic(path, want, 0o600); err != nil {
			return res, err
		}
	}
	if _, err := s.Host.Run(ctx, "netplan", "apply"); err != nil {
		return res, fmt.Errorf("netplan apply: %w", err)
	}
	// netplan apply returns before NetworkManager has the addresses up.
	deadline := time.Now().Add(s.fabricWait())
	for {
		ports, err = s.discoverPorts(ctx)
		if err == nil {
			byName = map[string]fabric.Netdev{}
			for _, p := range ports {
				for _, h := range p.Halves {
					byName[h.Name] = h
				}
			}
			res.Rails = railStatuses(rails, byName)
			done := true
			for _, st := range res.Rails {
				if !st.Present {
					done = false
				}
			}
			if done {
				break
			}
		}
		if time.Now().After(deadline) {
			for _, st := range res.Rails {
				if !st.Present {
					res.Warnings = append(res.Warnings, fmt.Sprintf("%s does not have %s yet (operstate %s)", st.Netdev, st.Addr, st.OperState))
				}
			}
			break
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	for _, st := range res.Rails {
		if st.Present && st.OperState != "up" {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s has its address but no carrier (is the cable in?)", st.Netdev))
		}
	}
	return res, nil
}

func (s *Server) fabricWait() time.Duration {
	if s.FabricWait > 0 {
		return s.FabricWait
	}
	return 30 * time.Second
}

func railMTU(r RailSpec) int {
	if r.MTU == 0 {
		return DefaultFabricMTU
	}
	return r.MTU
}

func railStatuses(rails []RailSpec, byName map[string]fabric.Netdev) []RailStatus {
	out := make([]RailStatus, 0, len(rails))
	for _, r := range rails {
		nd := byName[r.Netdev]
		out = append(out, RailStatus{Netdev: r.Netdev, Addr: r.Addr, Present: containsPrefix(nd.Addrs, r.Addr), MTU: nd.MTU, OperState: nd.OperState})
	}
	return out
}

func containsPrefix(xs []netip.Prefix, x netip.Prefix) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// --- cabling probe and ping ------------------------------------------------------

// ProbeAddr adds or removes a temporary (not persisted) address on a QSFP
// netdev of this box. Only a netdev without addresses, or with only the
// ones OAF's file gives it, may carry a probe.
func (s *Server) ProbeAddr(ctx context.Context, netdev string, addr netip.Prefix, remove bool) error {
	if s.Host == nil {
		return errors.New("this agent does not change host settings")
	}
	if !addr.IsValid() || !addr.Addr().Is4() {
		return fmt.Errorf("%q is not an IPv4 address with its prefix", addr)
	}
	ports, err := s.discoverPorts(ctx)
	if err != nil {
		return err
	}
	var nd *fabric.Netdev
	for _, p := range ports {
		for i := range p.Halves {
			if p.Halves[i].Name == netdev {
				nd = &p.Halves[i]
			}
		}
	}
	if nd == nil {
		return fmt.Errorf("%s is not a QSFP netdev of this box", netdev)
	}
	if remove {
		if !containsPrefix(nd.Addrs, addr) {
			return nil // already gone (NetworkManager may drop it first)
		}
		_, err := s.Host.Run(ctx, "ip", "addr", "del", addr.String(), "dev", netdev)
		return err
	}
	owned := map[string][]netip.Prefix{}
	if b, err := os.ReadFile(s.netplanPath()); err == nil {
		owned, _ = parseNetplan(b)
	}
	for _, a := range nd.Addrs {
		if a != addr && !containsPrefix(owned[netdev], a) {
			return fmt.Errorf("%s carries %s, which OAF did not assign: not probing it", netdev, a)
		}
	}
	if containsPrefix(nd.Addrs, addr) {
		return nil
	}
	_, err = s.Host.Run(ctx, "ip", "addr", "add", addr.String(), "dev", netdev)
	return err
}

// PingResult is one Ping.
type PingResult struct {
	OK          bool
	RTTms       float64
	NeighborMAC string
}

// PingFrom pings target out of netdev and reports the MAC that answered ARP
// for it there: on a point-to-point cable that is the peer port's netdev,
// whichever of the peer's addresses was asked for.
func (s *Server) PingFrom(ctx context.Context, netdev string, target netip.Addr, count, size int, dontFragment bool) (PingResult, error) {
	var res PingResult
	if !netdevOK(netdev) || !target.Is4() {
		return res, fmt.Errorf("bad ping request")
	}
	if count <= 0 || count > 10 {
		count = 2
	}
	args := []string{"-c", fmt.Sprint(count), "-W", "1", "-q", "-I", netdev}
	if size > 0 {
		if size > 9000 {
			return res, fmt.Errorf("size %d too large", size)
		}
		args = append(args, "-s", fmt.Sprint(size))
	}
	if dontFragment {
		args = append(args, "-M", "do")
	}
	args = append(args, target.String())
	ro := s.readOnly()
	out, err := ro.Run(ctx, "ping", args...)
	res.OK = err == nil
	res.RTTms = pingRTT(string(out))
	if nb, err := ro.Run(ctx, "ip", "-j", "neigh", "show", "dev", netdev); err == nil {
		res.NeighborMAC = neighborMAC(nb, target)
	}
	return res, nil
}

func netdevOK(n string) bool {
	if n == "" || len(n) > 15 {
		return false
	}
	for _, r := range n {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// pingRTT reads the average from ping's summary
// ("rtt min/avg/max/mdev = 0.276/0.458/0.641/0.182 ms").
func pingRTT(out string) float64 {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "= "); i >= 0 && strings.Contains(line, "min/avg/max") {
			f := strings.Split(strings.TrimSpace(line[i+2:]), "/")
			if len(f) >= 2 {
				var v float64
				if _, err := fmt.Sscanf(f[1], "%g", &v); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

// neighborMAC is the link-layer address `ip -j neigh show dev X` lists for
// target, if it resolved.
func neighborMAC(raw []byte, target netip.Addr) string {
	var ns []struct {
		Dst    string   `json:"dst"`
		LLAddr string   `json:"lladdr"`
		State  []string `json:"state"`
	}
	if json.Unmarshal(raw, &ns) != nil {
		return ""
	}
	for _, n := range ns {
		if n.Dst == target.String() && n.LLAddr != "" {
			failed := false
			for _, st := range n.State {
				if st == "FAILED" || st == "INCOMPLETE" {
					failed = true
				}
			}
			if !failed {
				return strings.ToLower(n.LLAddr)
			}
		}
	}
	return ""
}

func (s *Server) readOnly() hostexec.ReadOnly { return hostexec.ReadOnly{Inner: s.Inspect} }

// removeFabric removes OAF's netplan file (no rails left on this node).
func (s *Server) removeFabric(ctx context.Context, dryRun bool) (FabricResult, error) {
	var res FabricResult
	path := s.netplanPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return res, nil
	} else if err != nil {
		return res, err
	}
	res.Changed = true
	if dryRun {
		return res, nil
	}
	if s.Host == nil {
		return res, errors.New("this agent does not change host settings")
	}
	if err := os.Remove(path); err != nil {
		return res, err
	}
	if _, err := s.Host.Run(ctx, "netplan", "apply"); err != nil {
		return res, fmt.Errorf("netplan apply: %w", err)
	}
	return res, nil
}
