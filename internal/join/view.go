package join

import (
	"strings"

	"openagentfabric.dev/oaf/internal/fabric"
)

// NodeView is one node as the console shows it (GET /api/v1/nodes):
// joined nodes from cluster.yaml and pending ones that joined but wait for a
// topology. Times are epoch seconds.
type NodeView struct {
	Name      string          `json:"name"`
	Address   string          `json:"address"`
	Roles     []string        `json:"roles"`
	State     string          `json:"state"` // joined | pending | joining | offline
	Health    string          `json:"health,omitempty"`
	Hardware  *HardwareView   `json:"hardware,omitempty"`
	Ports     []PortView      `json:"ports,omitempty"`
	Preflight []PreflightView `json:"preflight,omitempty"`
	JoinedAt  int64           `json:"joined_at,omitempty"`
}

// HardwareView is the hardware summary.
type HardwareView struct {
	GPU           string  `json:"gpu,omitempty"`
	MemoryGiB     float64 `json:"memory_gib"`
	MemoryFreeGiB float64 `json:"memory_free_gib"`
	DiskFreeGiB   float64 `json:"disk_free_gib"`
	Serial        string  `json:"serial,omitempty"`
	OS            string  `json:"os,omitempty"`
}

// PortView is one QSFP port.
type PortView struct {
	Port      int        `json:"port"`
	Link      string     `json:"link"` // up | down
	Peer      string     `json:"peer,omitempty"`
	SpeedGbps int        `json:"speed_gbps,omitempty"`
	Rails     []RailView `json:"rails,omitempty"`
}

// RailView is one rail (netdev) of a port.
type RailView struct {
	Netdev string  `json:"netdev"`
	IP     string  `json:"ip,omitempty"`
	BWGbps float64 `json:"bw_gbps,omitempty"`
	Status string  `json:"status,omitempty"`
}

// PreflightView is one preflight result.
type PreflightView struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // pass | warn | fail (info shows as pass)
	Summary string `json:"summary"`
}

// HardwareSummary converts a hardware report for the console.
func HardwareSummary(h Hardware) *HardwareView {
	osName := h.Kernel
	if osName != "" {
		osName = "Linux " + osName
	}
	return &HardwareView{GPU: h.GPU, MemoryGiB: h.MemTotalGiB, MemoryFreeGiB: h.MemAvailableGiB, DiskFreeGiB: h.DiskFreeGiB, Serial: h.Serial, OS: osName}
}

// PortsView converts QSFP ports; peers maps "port index" to the box seen on
// the other end (from topology.ObservedCables), when known.
func PortsView(ports []fabric.Port, peers map[int]string) []PortView {
	out := make([]PortView, 0, len(ports))
	for _, p := range ports {
		v := PortView{Port: p.Index, Link: "down", Peer: peers[p.Index]}
		if p.Up() {
			v.Link = "up"
		}
		for _, nd := range p.Halves {
			r := RailView{Netdev: nd.Name, Status: nd.OperState}
			for _, a := range nd.Addrs {
				if a.Addr().Is4() {
					r.IP = a.Addr().String()
					break
				}
			}
			v.SpeedGbps += nd.SpeedMbps / 1000
			v.Rails = append(v.Rails, r)
		}
		out = append(out, v)
	}
	return out
}

// PreflightSummary converts preflight results.
func PreflightSummary(h Hardware) []PreflightView {
	out := make([]PreflightView, 0, len(h.Preflight))
	for _, r := range h.Preflight {
		st := strings.ToLower(string(r.Status))
		if st == "info" {
			st = "pass"
		}
		out = append(out, PreflightView{Name: r.ID, Status: st, Summary: r.Summary})
	}
	return out
}

// PendingView is a pending node for the console.
func PendingView(p PendingNode) NodeView {
	return NodeView{
		Name: p.Name, Address: p.Address, Roles: []string{}, State: "pending",
		Hardware: HardwareSummary(p.Hardware), Ports: PortsView(p.Hardware.Ports, nil),
		Preflight: PreflightSummary(p.Hardware), JoinedAt: p.EnrolledAt.Unix(),
	}
}
