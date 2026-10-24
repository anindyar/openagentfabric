package node

import (
	"fmt"
	"net/netip"

	"openagentfabric.dev/oaf/internal/fabric"
	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
)

// FabricToProto converts discovered ports for the wire.
func FabricToProto(ports []fabric.Port) []*nodev1.FabricPort {
	out := make([]*nodev1.FabricPort, 0, len(ports))
	for _, p := range ports {
		fp := &nodev1.FabricPort{Index: int32(p.Index)}
		for _, h := range p.Halves {
			nd := &nodev1.FabricNetdev{
				Name: h.Name, Pci: h.PCI, OperState: h.OperState,
				Mtu: int32(h.MTU), SpeedMbps: int32(h.SpeedMbps), RdmaDev: h.RDMADev, Mac: h.MAC,
			}
			for _, a := range h.Addrs {
				nd.Addrs = append(nd.Addrs, a.String())
			}
			for _, g := range h.GIDs {
				nd.Gids = append(nd.Gids, &nodev1.Gid{Index: int32(g.Index), Type: g.Type, Gid: g.GID})
			}
			fp.Halves = append(fp.Halves, nd)
		}
		out = append(out, fp)
	}
	return out
}

// FabricFromProto is the inverse of FabricToProto.
func FabricFromProto(ports []*nodev1.FabricPort) ([]fabric.Port, error) {
	out := make([]fabric.Port, 0, len(ports))
	for _, fp := range ports {
		p := fabric.Port{Index: int(fp.GetIndex())}
		for _, nd := range fp.GetHalves() {
			h := fabric.Netdev{
				Name: nd.GetName(), PCI: nd.GetPci(), OperState: nd.GetOperState(),
				MTU: int(nd.GetMtu()), SpeedMbps: int(nd.GetSpeedMbps()), RDMADev: nd.GetRdmaDev(), MAC: nd.GetMac(),
			}
			for _, a := range nd.GetAddrs() {
				pfx, err := netip.ParsePrefix(a)
				if err != nil {
					return nil, fmt.Errorf("%s: address %q: %w", nd.GetName(), a, err)
				}
				h.Addrs = append(h.Addrs, pfx)
			}
			for _, g := range nd.GetGids() {
				h.GIDs = append(h.GIDs, fabric.GID{Index: int(g.GetIndex()), Type: g.GetType(), GID: g.GetGid()})
			}
			p.Halves = append(p.Halves, h)
		}
		out = append(out, p)
	}
	return out, nil
}
