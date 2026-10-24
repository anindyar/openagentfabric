package node

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"openagentfabric.dev/oaf/internal/hostexec"
)

// ProjectPrefix namespaces every compose project the agent manages. The
// agent only ever addresses projects with this prefix, so it cannot touch
// containers it did not create.
const ProjectPrefix = "oaf-"

// Compose runs docker compose for one OAF project directory.
type Compose struct {
	Runner hostexec.Runner
}

func (c Compose) base(name, dir string) []string {
	return []string{"compose", "--project-name", ProjectPrefix + name, "--project-directory", dir, "--file", dir + "/compose.yaml"}
}

// Up creates or updates the project's containers.
func (c Compose) Up(ctx context.Context, name, dir string) error {
	_, err := c.Runner.Run(ctx, "docker", append(c.base(name, dir), "up", "--detach", "--remove-orphans", "--quiet-pull")...)
	return err
}

// Down stops and removes the project's containers (and volumes if asked).
func (c Compose) Down(ctx context.Context, name, dir string, volumes bool) error {
	args := append(c.base(name, dir), "down", "--remove-orphans")
	if volumes {
		args = append(args, "--volumes")
	}
	_, err := c.Runner.Run(ctx, "docker", args...)
	return err
}

// ServiceState is one container's state.
type ServiceState struct {
	Name     string `json:"Name"` // the container
	Service  string `json:"Service"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	Image    string `json:"Image"`
	ExitCode int    `json:"ExitCode"`
}

// Started returns when one of the project's containers last started
// (RFC 3339) and how often it restarted.
func (c Compose) Started(ctx context.Context, container string) (string, int, error) {
	if !strings.HasPrefix(container, ProjectPrefix) {
		return "", 0, fmt.Errorf("%q is not an OAF container", container)
	}
	out, err := c.Runner.Run(ctx, "docker", "inspect", "--format", "{{.State.StartedAt}} {{.RestartCount}}", container)
	if err != nil {
		return "", 0, err
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return "", 0, fmt.Errorf("docker inspect: unexpected %q", out)
	}
	n, err := strconv.Atoi(f[1])
	return f[0], n, err
}

// Logs returns a service's last log lines.
func (c Compose) Logs(ctx context.Context, name, service string, lines int) ([]string, error) {
	out, err := c.Runner.Run(ctx, "docker", "compose", "--project-name", ProjectPrefix+name, "logs",
		"--no-color", "--no-log-prefix", "--tail", strconv.Itoa(lines), service)
	if err != nil {
		return nil, err
	}
	return splitLogLines(string(out)), nil
}

// splitLogLines splits log output into lines; progress bars redraw with
// carriage returns, so only each line's last redraw is kept.
func splitLogLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r ")
		if i := strings.LastIndex(l, "\r"); i >= 0 {
			l = l[i+1:]
		}
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// PS lists the project's containers.
func (c Compose) PS(ctx context.Context, name string) ([]ServiceState, error) {
	out, err := c.Runner.Run(ctx, "docker", "compose", "--project-name", ProjectPrefix+name, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parsePS(out)
}

// parsePS accepts both formats compose has used: a JSON array, or one JSON
// object per line.
func parsePS(out []byte) ([]ServiceState, error) {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "[") {
		var all []ServiceState
		err := json.Unmarshal([]byte(s), &all)
		return all, err
	}
	var all []ServiceState
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		var st ServiceState
		if err := json.Unmarshal([]byte(l), &st); err != nil {
			return nil, err
		}
		all = append(all, st)
	}
	return all, nil
}
