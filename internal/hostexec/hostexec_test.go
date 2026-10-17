package hostexec

import (
	"context"
	"strings"
	"testing"
)

type nopRunner struct{ calls []string }

func (n *nopRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	n.calls = append(n.calls, CmdLine(name, args...))
	return nil, nil
}

func TestReadOnlyAllowsInspection(t *testing.T) {
	ok := [][]string{
		{"cat", "/proc/meminfo"},
		{"ls", "-1", "/sys/class/net"},
		{"df", "-B1", "--output=avail", "/"},
		{"ip", "-j", "addr", "show"},
		{"docker", "ps", "-a", "--format", "{{json .}}"},
		{"docker", "compose", "version", "--short"},
		{"nvidia-smi", "--query-gpu=name,driver_version", "--format=csv,noheader"},
		{"rdma", "-j", "link", "show"},
	}
	inner := &nopRunner{}
	r := ReadOnly{Inner: inner}
	for _, c := range ok {
		if _, err := r.Run(context.Background(), c[0], c[1:]...); err != nil {
			t.Errorf("%v refused: %v", c, err)
		}
	}
	if len(inner.calls) != len(ok) {
		t.Fatalf("inner called %d times", len(inner.calls))
	}
}

func TestReadOnlyRefusesMutations(t *testing.T) {
	bad := [][]string{
		{"rm", "-rf", "/"},
		{"sh", "-c", "anything"},
		{"ip", "link", "set", "enp1s0f1np1", "mtu", "9000"},
		{"ip", "-j", "addr", "add", "10.0.0.1/30", "dev", "x"},
		{"docker", "stop", "vllm-ornith"},
		{"docker", "rm", "-f", "vllm-ornith"},
		{"docker", "compose", "down"},
		{"docker", "run", "--rm", "alpine"},
		{"nvidia-smi", "-r"},
		{"sudo", "cat", "/etc/shadow"},
		{"cat", "--output=/x"},
		{"ls", "-R", "/"},
	}
	inner := &nopRunner{}
	r := ReadOnly{Inner: inner}
	for _, c := range bad {
		if _, err := r.Run(context.Background(), c[0], c[1:]...); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%v was not refused (err=%v)", c, err)
		}
	}
	if len(inner.calls) != 0 {
		t.Fatalf("refused commands reached the host: %v", inner.calls)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":           "plain",
		"{{json .}}":      `'{{json .}}'`,
		"it's":            `'it'\''s'`,
		"":                "''",
		"/sys/class/net/": "/sys/class/net/",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReplay(t *testing.T) {
	r := Replay{"uname -m": {Stdout: "aarch64\n"}, "which x": {Exit: 1}}
	out, err := r.Run(context.Background(), "uname", "-m")
	if err != nil || string(out) != "aarch64\n" {
		t.Fatalf("got %q, %v", out, err)
	}
	if _, err := r.Run(context.Background(), "which", "x"); err == nil {
		t.Fatal("want exit error")
	}
	if _, err := r.Run(context.Background(), "nope"); err == nil {
		t.Fatal("want not-recorded error")
	}
}
