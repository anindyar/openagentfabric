package hostexec

import (
	"context"
	"testing"
)

func TestChangesAllowList(t *testing.T) {
	c := Changes{Inner: Replay{
		"netplan apply": {}, "ip addr add 198.18.0.1/24 dev enp1s0f0np0": {}, "ip addr del 198.18.0.1/24 dev enp1s0f0np0": {},
		"systemctl set-default multi-user.target": {}, "systemctl stop display-manager": {},
	}}
	for _, ok := range [][]string{
		{"netplan", "apply"},
		{"ip", "addr", "add", "198.18.0.1/24", "dev", "enp1s0f0np0"},
		{"ip", "addr", "del", "198.18.0.1/24", "dev", "enp1s0f0np0"},
		{"systemctl", "set-default", "multi-user.target"},
		{"systemctl", "stop", "display-manager"},
	} {
		if _, err := c.Run(context.Background(), ok[0], ok[1:]...); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		{"netplan", "generate"},
		{"rm", "-rf", "/"},
		{"ip", "link", "set", "enp1s0f0np0", "down"},
		{"ip", "addr", "flush", "dev", "enp1s0f0np0"},
		{"ip", "addr", "add", "not-an-address", "dev", "enp1s0f0np0"},
		{"ip", "addr", "add", "198.18.0.1/24", "dev", "lo; reboot"},
		{"systemctl", "stop", "sshd"},
		{"systemctl", "set-default", "rescue.target"},
	} {
		if _, err := c.Run(context.Background(), bad[0], bad[1:]...); err == nil {
			t.Errorf("%v allowed", bad)
		}
	}
}
