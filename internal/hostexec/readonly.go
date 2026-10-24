package hostexec

import (
	"context"
	"fmt"
	"strings"
)

// readOnly lists the commands an inspection may run. For multi-purpose tools
// the allowed leading arguments are listed too; anything else is refused.
var readOnly = map[string][][]string{
	"cat":         nil, // file reads (sysfs, procfs, /etc)
	"ls":          nil,
	"readlink":    nil,
	"df":          nil,
	"uname":       nil,
	"which":       nil,
	"nvidia-smi":  {{"--query-gpu"}, {"--query-compute-apps"}},
	"nvidia-ctk":  {{"--version"}},
	"ldconfig":    {{"-p"}},
	"ss":          {{"-Hltn"}},
	"timedatectl": {{"show"}},
	"ip":          {{"-j", "addr", "show"}, {"-j", "link", "show"}, {"-j", "neigh", "show"}},
	"rdma":        {{"-j", "link", "show"}},
	"docker": {
		{"version", "--format"},
		{"info", "--format"},
		{"ps", "--format"},
		{"ps", "-a", "--format"},
		{"compose", "version"},
	},
}

// ReadOnly wraps a Runner and refuses any command outside the allowlist.
type ReadOnly struct{ Inner Runner }

// Run implements Runner.
func (r ReadOnly) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if !allowed(name, args) {
		return nil, fmt.Errorf("refused: %q is not a read-only inspection command", CmdLine(name, args...))
	}
	return r.Inner.Run(ctx, name, args...)
}

func allowed(name string, args []string) bool {
	prefixes, ok := readOnly[name]
	if !ok {
		return false
	}
	if prefixes == nil {
		// Plain readers: refuse option-looking arguments that could write
		// (e.g. none exist for cat/ls/readlink/df/uname/which, but be strict).
		for _, a := range args {
			if strings.HasPrefix(a, "-") && !safeReaderFlag(name, a) {
				return false
			}
		}
		return true
	}
	for _, p := range prefixes {
		if hasPrefix(args, p) {
			return true
		}
	}
	return false
}

func safeReaderFlag(name, flag string) bool {
	switch name {
	case "ls":
		return flag == "-1"
	case "df":
		return flag == "-B1" || strings.HasPrefix(flag, "--output=")
	case "uname":
		return flag == "-r" || flag == "-m"
	}
	return false
}

func hasPrefix(args, prefix []string) bool {
	if len(args) < len(prefix) {
		return false
	}
	for i := range prefix {
		if !strings.HasPrefix(args[i], prefix[i]) || (i < len(prefix)-1 && args[i] != prefix[i]) {
			return false
		}
	}
	return true
}
