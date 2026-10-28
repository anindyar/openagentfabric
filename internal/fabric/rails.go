package fabric

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
)

// ErrNotAddressed: a rail has no address (yet). Fabric bring-up gives it
// one, so a plan that adds a cable expects it before the first apply.
var ErrNotAddressed = errors.New("run fabric bring-up")

// GIDAuto asks for the RoCE v2 GID that carries the rail address.
const GIDAuto = "auto"

// End is one end of a cable after bring-up: the port's rails, ready to use.
type End struct {
	Node  string
	Port  int
	Rails []Rail // one per PCIe half, in PCI order
}

// Rail is one netdev of a port with the address and GID that traffic uses.
type Rail struct {
	Netdev  string
	RDMADev string // empty without RDMA
	MTU     int
	Addr    netip.Prefix // the rail address with its prefix
	GID     int          // RoCE v2 GID index; -1 when it cannot be resolved
}

// ResolveEnd finds port's rails on a node from its discovery. subnets[h],
// when set, selects rail h's address (a netdev can carry several); otherwise
// the first IPv4 address is used. gidIndex is fabric.roce.gidIndex: "auto"
// (or empty) looks up the RoCE v2 GID of the rail address, which moves as
// addresses are added; an integer is used as is.
//
// The port must have carrier and every rail an IPv4 address. A missing RDMA
// device or GID is not an error here (sockets do not need one): GID is -1.
func ResolveEnd(node string, ports []Port, port int, subnets []string, gidIndex string) (End, error) {
	i := slices.IndexFunc(ports, func(p Port) bool { return p.Index == port })
	if i < 0 {
		return End{}, fmt.Errorf("node %s has no QSFP port %d", node, port)
	}
	p := ports[i]
	if !p.Up() {
		return End{}, fmt.Errorf("port %d on %s has no carrier", port, node)
	}
	fixed := -1
	if gidIndex != "" && gidIndex != GIDAuto {
		n, err := strconv.Atoi(gidIndex)
		if err != nil {
			return End{}, fmt.Errorf("fabric.roce.gidIndex %q: %w", gidIndex, err)
		}
		fixed = n
	}
	end := End{Node: node, Port: port}
	for h, nd := range p.Halves {
		var subnet netip.Prefix
		if h < len(subnets) && subnets[h] != "" {
			var err error
			if subnet, err = netip.ParsePrefix(subnets[h]); err != nil {
				return End{}, fmt.Errorf("fabric subnet %q: %w", subnets[h], err)
			}
		}
		addr, ok := railAddr(nd, subnet)
		if !ok {
			in := ""
			if subnet.IsValid() {
				in = " in " + subnet.String()
			}
			return End{}, fmt.Errorf("%s on %s has no IPv4 address%s; %w", nd.Name, node, in, ErrNotAddressed)
		}
		gid := fixed
		if gid < 0 {
			if g, ok := nd.RoCEv2GIDFor(addr.Addr()); ok {
				gid = g
			}
		}
		end.Rails = append(end.Rails, Rail{Netdev: nd.Name, RDMADev: nd.RDMADev, MTU: nd.MTU, Addr: addr, GID: gid})
	}
	return end, nil
}

// railAddr is the netdev's IPv4 address in subnet, or its first IPv4
// address when no subnet is given.
func railAddr(nd Netdev, subnet netip.Prefix) (netip.Prefix, bool) {
	for _, a := range nd.Addrs {
		if a.Addr().Is4() && (!subnet.IsValid() || subnet.Contains(a.Addr())) {
			return a, true
		}
	}
	return netip.Prefix{}, false
}
