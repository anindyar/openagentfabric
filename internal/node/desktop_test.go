package node

import (
	"context"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/hostexec"
)

// fakeSystemd answers the desktop commands and changes state like systemd.
type fakeSystemd struct {
	target   string // graphical.target | multi-user.target
	dm       bool
	sessions string
	ran      []string
}

func (f *fakeSystemd) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := hostexec.CmdLine(name, args...)
	f.ran = append(f.ran, line)
	switch {
	case line == "systemctl get-default":
		return []byte(f.target + "\n"), nil
	case line == "systemctl is-active display-manager":
		if f.dm {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), &hostexec.ExitError{Cmd: line, Code: 3}
	case line == "ps -eo comm=,rss=":
		if f.dm { // 600 MiB of desktop; firefox and dockerd do not count
			return []byte("gnome-shell 307200\nXorg 102400\ngsd-color 102400\nevolution-alarm 51200\ngdm3 51200\nfirefox 800000\ndockerd 120000\ngnome-terminal- 50000\nIsolated Web Co 1000\n"), nil
		}
		return []byte("dockerd 120000\n"), nil
	case line == "loginctl list-sessions --no-legend":
		return []byte(f.sessions), nil
	case line == "systemctl set-default multi-user.target":
		f.target = "multi-user.target"
	case line == "systemctl set-default graphical.target":
		f.target = "graphical.target"
	case line == "systemctl stop display-manager":
		f.dm = false
	case line == "systemctl start display-manager":
		f.dm = true
	default:
		return nil, &hostexec.ExitError{Cmd: line, Code: 127}
	}
	return nil, nil
}

func (f *fakeSystemd) mutations() []string {
	var out []string
	for _, l := range f.ran {
		if strings.HasPrefix(l, "systemctl set-default") || strings.HasPrefix(l, "systemctl stop") || strings.HasPrefix(l, "systemctl start") {
			out = append(out, l)
		}
	}
	return out
}

func TestDesktopHeadlessAndBack(t *testing.T) {
	ctx := context.Background()
	f := &fakeSystemd{target: "graphical.target", dm: true, sessions: "2 1000 ana seat0 tty2\n"}

	st, err := DesktopReport(ctx, f)
	if err != nil || st.Mode != DesktopGraphical || !st.DisplayManagerActive || st.EstimatedMiB != 600 {
		t.Fatalf("report = %+v, %v", st, err)
	}

	// Dry run changes nothing but says what would.
	st, err = SetDesktop(ctx, f, f, true, true)
	if err != nil || !st.Changed || st.Mode != DesktopHeadless || len(f.mutations()) != 0 {
		t.Fatalf("dry run = %+v, %v, ran %v", st, err, f.mutations())
	}

	st, err = SetDesktop(ctx, f, f, true, false)
	if err != nil || !st.Changed || st.Mode != DesktopHeadless || st.DisplayManagerActive {
		t.Fatalf("headless = %+v, %v", st, err)
	}
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "logged in") {
		t.Fatalf("a seat session must warn: %v", st.Warnings)
	}
	if got := f.mutations(); len(got) != 2 || got[0] != "systemctl set-default multi-user.target" || got[1] != "systemctl stop display-manager" {
		t.Fatalf("commands %v", got)
	}

	// Idempotent: already headless, nothing runs.
	f.ran = nil
	if st, _ = SetDesktop(ctx, f, f, true, false); st.Changed || len(f.mutations()) != 0 {
		t.Fatalf("second headless changed something: %+v %v", st, f.mutations())
	}

	f.sessions = ""
	st, err = SetDesktop(ctx, f, f, false, false)
	if err != nil || st.Mode != DesktopGraphical || !st.DisplayManagerActive || len(st.Warnings) != 0 {
		t.Fatalf("back to graphical = %+v, %v", st, err)
	}
	if got := f.mutations(); len(got) != 2 || got[1] != "systemctl start display-manager" {
		t.Fatalf("commands %v", got)
	}

	// An agent without host access reports but refuses to change.
	f2 := &fakeSystemd{target: "graphical.target", dm: true}
	if _, err := SetDesktop(ctx, f2, nil, true, false); err == nil {
		t.Fatal("a change without a host runner must fail")
	}
}

// Reporting stays inside the read-only allowlist.
func TestDesktopReportIsReadOnly(t *testing.T) {
	for _, cmd := range [][]string{{"systemctl", "get-default"}, {"systemctl", "is-active", "display-manager"},
		{"loginctl", "list-sessions", "--no-legend"}, {"ps", "-eo", "comm=,rss="}} {
		if _, err := (hostexec.ReadOnly{Inner: hostexec.Replay{}}).Run(context.Background(), cmd[0], cmd[1:]...); err != nil && strings.Contains(err.Error(), "refused") {
			t.Errorf("%v refused", cmd)
		}
	}
	if _, err := (hostexec.ReadOnly{Inner: hostexec.Replay{}}).Run(context.Background(), "systemctl", "stop", "display-manager"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Error("systemctl stop must not be read-only")
	}
}
