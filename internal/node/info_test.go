package node

import (
	"context"
	"testing"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/preflight"
)

// Info carries the box's hardware, so a node in the cluster shows memory,
// disk and GPU like a box that just joined.
func TestInfoReportsHardware(t *testing.T) {
	s := &Server{
		Inspect: hostexec.Replay{
			"uname -m": {Stdout: "aarch64\n"},
			"nvidia-smi --query-gpu=name,driver_version --format=csv,noheader": {Stdout: "NVIDIA GB10, 580.178.04\n"},
			"cat /sys/class/dmi/id/product_serial":                             {Stdout: "SERIAL0001\n"},
			"cat /proc/meminfo":                                                {Stdout: "MemTotal:       127598548 kB\nMemAvailable:   15387704 kB\n"},
			"ls -1 /var/lib/oaf":                                               {Stdout: "models\n"},
			"df -B1 --output=avail,size /var/lib/oaf":                          {Stdout: "Avail Size\n1099511627776 3994319585280\n"},
		},
		PreflightOptions: preflight.Options{DataPath: "/var/lib/oaf"},
	}
	resp, err := s.Info(context.Background(), &nodev1.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetGpu() != "NVIDIA GB10" || resp.GetSerial() != "SERIAL0001" {
		t.Fatalf("gpu/serial: %q %q", resp.GetGpu(), resp.GetSerial())
	}
	if resp.GetMemTotalGib() < 121 || resp.GetMemTotalGib() > 122 || resp.GetMemAvailableGib() < 14 || resp.GetMemAvailableGib() > 15 {
		t.Fatalf("memory: %v / %v", resp.GetMemTotalGib(), resp.GetMemAvailableGib())
	}
	if resp.GetDiskFreeGib() != 1024 {
		t.Fatalf("disk free: %v", resp.GetDiskFreeGib())
	}
}
