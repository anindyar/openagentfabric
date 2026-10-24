package reconcile

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/node"
)

type desktopHost struct {
	graphical, dm bool
	changes       int
}

func (d *desktopHost) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	switch hostexec.CmdLine(name, args...) {
	case "systemctl get-default":
		if d.graphical {
			return []byte("graphical.target\n"), nil
		}
		return []byte("multi-user.target\n"), nil
	case "systemctl is-active display-manager":
		if d.dm {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), nil
	case "ps -eo comm=,rss=":
		if d.dm {
			return []byte("gnome-shell 512000\n"), nil
		}
		return nil, nil
	case "loginctl list-sessions --no-legend":
		return nil, nil
	case "systemctl set-default multi-user.target":
		d.graphical = false
	case "systemctl set-default graphical.target":
		d.graphical = true
	case "systemctl stop display-manager":
		d.dm = false
	case "systemctl start display-manager":
		d.dm = true
	}
	d.changes++
	return nil, nil
}

// apply switches only nodes that set headless, only when they differ, and
// a dry run changes nothing.
func TestDesktopReconcile(t *testing.T) {
	yes := true
	a, b := &desktopHost{graphical: true, dm: true}, &desktopHost{graphical: true, dm: true}
	ns := Nodes{
		"a": &node.Server{StateDir: t.TempDir(), Inspect: a, Host: a},
		"b": &node.Server{StateDir: t.TempDir(), Inspect: b, Host: b},
	}
	c := &config.Cluster{Spec: config.ClusterSpec{Nodes: []config.Node{{Name: "a", Headless: &yes}, {Name: "b"}}}}
	var out bytes.Buffer
	ctx := context.Background()
	if err := Desktop(ctx, ns, c, true, &out); err != nil {
		t.Fatal(err)
	}
	if a.changes != 0 || !strings.Contains(out.String(), "update    a/desktop  headless (was graphical, about 500 MiB back)") {
		t.Fatalf("dry run: changes %d\n%s", a.changes, out.String())
	}
	out.Reset()
	if err := Desktop(ctx, ns, c, false, &out); err != nil {
		t.Fatal(err)
	}
	if a.graphical || a.dm || b.changes != 0 || !strings.Contains(out.String(), "updated   a/desktop  headless") {
		t.Fatalf("apply: a=%+v b changes=%d\n%s", a, b.changes, out.String())
	}
	out.Reset()
	before := a.changes
	if err := Desktop(ctx, ns, c, false, &out); err != nil || a.changes != before || !strings.Contains(out.String(), "unchanged a/desktop  headless") {
		t.Fatalf("second apply must not touch a: %v %d\n%s", err, a.changes-before, out.String())
	}
}
