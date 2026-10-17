// Package hostexec runs commands on a node, locally or over SSH.
//
// Inspection code (preflight, fabric discovery) receives a ReadOnly runner,
// which refuses anything outside a fixed allowlist of read-only commands.
// That guarantees, structurally, that inspecting a production box cannot
// change it.
package hostexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs a command and returns its stdout.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExitError is returned when a command ran but exited non-zero.
type ExitError struct {
	Cmd    string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("%s: exit status %d", e.Cmd, e.Code)
	}
	return fmt.Sprintf("%s: exit status %d: %s", e.Cmd, e.Code, msg)
}

// CmdLine renders a command for messages and fixture keys.
func CmdLine(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// Local runs commands on this machine.
type Local struct{}

// Run implements Runner.
func (Local) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return run(exec.CommandContext(ctx, name, args...), CmdLine(name, args...))
}

// SSH runs commands on a remote host with the system ssh client. It uses a
// shared control connection, so many small reads stay fast.
type SSH struct {
	Target      string // user@host
	ControlPath string // e.g. /tmp/oaf-ssh-%C
}

// Run implements Runner. Arguments are shell-quoted for the remote shell.
func (s SSH) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, shellQuote(name))
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	sshArgs := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if s.ControlPath != "" {
		sshArgs = append(sshArgs, "-o", "ControlMaster=auto", "-o", "ControlPath="+s.ControlPath, "-o", "ControlPersist=60")
	}
	sshArgs = append(sshArgs, s.Target, "--", strings.Join(quoted, " "))
	return run(exec.CommandContext(ctx, "ssh", sshArgs...), CmdLine(name, args...))
}

func run(cmd *exec.Cmd, line string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.Bytes(), &ExitError{Cmd: line, Code: ee.ExitCode(), Stderr: stderr.String()}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", line, err)
	}
	return stdout.Bytes(), nil
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, needsQuoting) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsQuoting(r rune) bool {
	safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r)
	return !safe
}
