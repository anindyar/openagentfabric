package node

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"openagentfabric.dev/oaf/internal/hostexec"
)

// The desktop GUI of a DGX OS box: GNOME under gdm, started by
// display-manager.service when the default target is graphical. On GB10 the
// GPU memory is the system memory, so the display stack holds memory a model
// could use. Headless = boot to multi-user.target and stop the display
// manager; graphical = the reverse. Only systemd is used; nothing reboots.

// Desktop modes.
const (
	DesktopGraphical = "graphical"
	DesktopHeadless  = "headless"
)

// desktopProcess matches (by command-name prefix) the display stack and the
// desktop session's own services, whose memory headless gives back. Apps a
// user opened on the desktop (browsers, terminals) close too but are not
// counted: the estimate is what the desktop itself holds.
var desktopPrefixes = []string{"gdm", "Xorg", "Xwayland", "gnome-", "gsd-", "mutter", "evolution", "tracker-",
	"ibus", "xdg-desktop-", "xdg-document", "xdg-permission", "gvfs", "at-spi", "goa-", "gjs", "snapd-desktop", "mozc"}

func desktopProcess(comm string) bool {
	if strings.HasPrefix(comm, "gnome-terminal") {
		return false // a user app, not the desktop
	}
	for _, p := range desktopPrefixes {
		if strings.HasPrefix(comm, p) {
			return true
		}
	}
	return false
}

// DesktopState is a node's desktop as the agent sees it.
type DesktopState struct {
	Mode                 string
	DisplayManagerActive bool
	EstimatedMiB         int64
	Changed              bool
	Warnings             []string
}

// DesktopReport reads the desktop state with read-only commands.
func DesktopReport(ctx context.Context, read hostexec.Runner) (DesktopState, error) {
	ro := hostexec.ReadOnly{Inner: read}
	var st DesktopState
	out, err := ro.Run(ctx, "systemctl", "get-default")
	if err != nil {
		return st, err
	}
	st.Mode = DesktopHeadless
	if strings.TrimSpace(string(out)) == "graphical.target" {
		st.Mode = DesktopGraphical
	}
	// is-active exits non-zero for inactive or missing units; the word is
	// what matters.
	out, _ = ro.Run(ctx, "systemctl", "is-active", "display-manager")
	st.DisplayManagerActive = strings.TrimSpace(string(out)) == "active"
	if out, err := ro.Run(ctx, "ps", "-eo", "comm=,rss="); err == nil {
		var kib int64
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			// comm may contain spaces; RSS is the last field.
			comm := strings.Join(f[:len(f)-1], " ")
			if n, err := strconv.ParseInt(f[len(f)-1], 10, 64); err == nil && desktopProcess(comm) {
				kib += n
			}
		}
		st.EstimatedMiB = kib / 1024
	}
	return st, nil
}

// seatSessions counts logged-in sessions on a seat (someone at the screen).
func seatSessions(ctx context.Context, read hostexec.Runner) int {
	out, err := hostexec.ReadOnly{Inner: read}.Run(ctx, "loginctl", "list-sessions", "--no-legend")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "seat") {
				n++
				break
			}
		}
	}
	return n
}

// SetDesktop switches the desktop. It is idempotent: a node already in the
// wanted mode is not touched. A desktop session in use is a warning, not a
// refusal: the owner asked for it.
func SetDesktop(ctx context.Context, read, host hostexec.Runner, headless, dryRun bool) (DesktopState, error) {
	st, err := DesktopReport(ctx, read)
	if err != nil {
		return st, err
	}
	want, target, verb := DesktopGraphical, "graphical.target", "start"
	if headless {
		want, target, verb = DesktopHeadless, "multi-user.target", "stop"
	}
	modeOK := st.Mode == want
	runningOK := st.DisplayManagerActive == !headless
	if modeOK && runningOK {
		return st, nil
	}
	if headless && st.DisplayManagerActive {
		if n := seatSessions(ctx, read); n > 0 {
			st.Warnings = append(st.Warnings, "someone is logged in on the local desktop; their session ends")
		}
	}
	st.Changed = true
	if dryRun {
		st.Mode = want
		st.DisplayManagerActive = !headless
		return st, nil
	}
	if host == nil {
		return st, errors.New("this agent does not change host settings")
	}
	if !modeOK {
		if _, err := host.Run(ctx, "systemctl", "set-default", target); err != nil {
			return st, err
		}
	}
	if !runningOK {
		if _, err := host.Run(ctx, "systemctl", verb, "display-manager"); err != nil {
			return st, err
		}
	}
	after, err := DesktopReport(ctx, read)
	if err != nil {
		return st, err
	}
	after.Changed, after.Warnings = true, st.Warnings
	return after, nil
}
