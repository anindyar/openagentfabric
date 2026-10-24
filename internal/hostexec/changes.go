package hostexec

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
)

// Changes wraps the runner the node agent changes the host with. It allows
// only the host changes OAF makes, with their arguments checked, so a bug
// or a bad request cannot turn the agent into a general root shell:
//
//   - netplan apply (after OAF wrote its own netplan file)
//   - ip addr add|del <IPv4 prefix> dev <netdev> (cabling probes)
//   - systemctl set-default graphical.target|multi-user.target
//   - systemctl start|stop display-manager
type Changes struct{ Inner Runner }

// Run implements Runner.
func (c Changes) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if !changeAllowed(name, args) {
		return nil, fmt.Errorf("refused: %q is not a host change the agent makes", CmdLine(name, args...))
	}
	return c.Inner.Run(ctx, name, args...)
}

func changeAllowed(name string, args []string) bool {
	switch name {
	case "netplan":
		return len(args) == 1 && args[0] == "apply"
	case "ip":
		// ip addr add|del <prefix> dev <netdev>
		if len(args) != 5 || args[0] != "addr" || (args[1] != "add" && args[1] != "del") || args[3] != "dev" {
			return false
		}
		p, err := netip.ParsePrefix(args[2])
		return err == nil && p.Addr().Is4() && netdevName.MatchString(args[4])
	case "systemctl":
		if len(args) != 2 {
			return false
		}
		switch args[0] {
		case "set-default":
			return args[1] == "graphical.target" || args[1] == "multi-user.target"
		case "start", "stop":
			return args[1] == "display-manager"
		}
	}
	return false
}

// netdevName is a plausible Linux netdev name for an Ethernet port: the
// agent checks separately that it is one of this box's QSFP netdevs.
var netdevName = regexp.MustCompile(`^en[A-Za-z0-9]{1,13}$`)
