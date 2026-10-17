package node

import (
	"context"
	"encoding/json"
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
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
	Image   string `json:"Image"`
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
