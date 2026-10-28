package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"openagentfabric.dev/oaf/internal/hostexec"
	"openagentfabric.dev/oaf/internal/node"
)

// resetUnits are the systemd units the installer and `oaf join` create.
var resetUnits = []string{"oaf-api.service", "oaf-keys.service", "oaf-node.service"}

func newResetCmd() *cobra.Command {
	var yes, keepWeights, keepImages bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Return this box to the state before OAF was installed",
		Long: "Removes everything OAF put on THIS box: every oaf-* container with its volumes and networks,\n" +
			"the images they used, the systemd units, the fabric netplan file, the registry CA, /etc/oaf,\n" +
			"/var/lib/oaf (model weights included unless --keep-weights), /opt/oaf and the oaf binary.\n" +
			"Other containers are never touched. Run it on every box; a joined box does not need the\n" +
			"control node. The desktop mode is left as it is. This cannot be undone.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("reset deletes OAF and its data from this box; re-run with --yes to confirm")
			}
			if os.Geteuid() != 0 {
				return errors.New("reset changes system files; run it with sudo")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
			defer cancel()
			r := resetter{Run: hostexec.Local{}, Root: "/", Out: cmd.OutOrStdout(), KeepWeights: keepWeights, KeepImages: keepImages}
			return r.reset(ctx)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm")
	cmd.Flags().BoolVar(&keepWeights, "keep-weights", false, "keep /var/lib/oaf/models (downloaded model weights)")
	cmd.Flags().BoolVar(&keepImages, "keep-images", false, "keep the container images")
	return cmd
}

// resetter removes OAF from one box. Root prefixes every path (tests).
type resetter struct {
	Run         hostexec.Runner
	Root        string
	Out         io.Writer
	KeepWeights bool
	KeepImages  bool
}

func (r resetter) path(p string) string { return filepath.Join(r.Root, p) }

func (r resetter) say(format string, a ...any) { _, _ = fmt.Fprintf(r.Out, format+"\n", a...) }

// reset keeps going after a failed step, so one stuck container does not
// leave the rest of the box half removed; it reports every failure.
func (r resetter) reset(ctx context.Context) error {
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	keep(r.units(ctx))
	images, err := r.containers(ctx)
	keep(err)
	keep(r.volumesAndNetworks(ctx))
	if !r.KeepImages {
		keep(r.images(ctx, images))
	}
	keep(r.fabric(ctx))
	keep(r.files())
	if len(errs) == 0 {
		r.say("done: OAF is removed from this box")
	}
	return errors.Join(errs...)
}

// units stops the agents first, so nothing recreates what is removed next.
func (r resetter) units(ctx context.Context) error {
	var errs []error
	removed := false
	for _, u := range resetUnits {
		p := r.path("/etc/systemd/system/" + u)
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		_, _ = r.Run.Run(ctx, "systemctl", "disable", "--now", u)
		if err := os.Remove(p); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = true
		r.say("removed   %s", u)
	}
	if removed {
		if _, err := r.Run.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			errs = append(errs, fmt.Errorf("systemctl daemon-reload: %w", err))
		}
	}
	return errors.Join(errs...)
}

// containers removes every container of an oaf-* compose project and
// returns the IDs of the images they ran (docker ps shows a digest-pinned
// image by repository only, which `docker image rm` cannot resolve).
func (r resetter) containers(ctx context.Context) ([]string, error) {
	out, err := r.Run.Run(ctx, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project",
		"--format", `{{.ID}} {{.Label "com.docker.compose.project"}}`)
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	var ids []string
	images := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !strings.HasPrefix(f[1], node.ProjectPrefix) {
			continue
		}
		ids = append(ids, f[0])
	}
	if len(ids) > 0 {
		out, err := r.Run.Run(ctx, "docker", append([]string{"inspect", "--format", "{{.Image}}"}, ids...)...)
		if err != nil {
			return nil, fmt.Errorf("docker inspect: %w", err)
		}
		for _, id := range strings.Fields(string(out)) {
			images[id] = true
		}
		if _, err := r.Run.Run(ctx, "docker", append([]string{"rm", "--force", "--volumes"}, ids...)...); err != nil {
			return nil, fmt.Errorf("docker rm: %w", err)
		}
		r.say("removed   %d containers", len(ids))
	}
	return sortedSet(images), nil
}

func (r resetter) volumesAndNetworks(ctx context.Context) error {
	var errs []error
	for _, kind := range []string{"volume", "network"} {
		out, err := r.Run.Run(ctx, "docker", kind, "ls", "--filter", "label=com.docker.compose.project",
			"--format", `{{.Name}} {{.Label "com.docker.compose.project"}}`)
		if err != nil {
			errs = append(errs, fmt.Errorf("docker %s ls: %w", kind, err))
			continue
		}
		var names []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if f := strings.Fields(line); len(f) == 2 && strings.HasPrefix(f[1], node.ProjectPrefix) {
				names = append(names, f[0])
			}
		}
		if len(names) == 0 {
			continue
		}
		if _, err := r.Run.Run(ctx, "docker", append([]string{kind, "rm"}, names...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker %s rm: %w", kind, err))
			continue
		}
		r.say("removed   %d %ss", len(names), kind)
	}
	return errors.Join(errs...)
}

// images removes the images the containers ran, with every tag they have
// (a digest-pinned image may also be tagged, e.g. by `oaf images push`).
// An image another (non-OAF) container still uses stays, with a note.
func (r resetter) images(ctx context.Context, images []string) error {
	n := 0
	for _, id := range images {
		out, err := r.Run.Run(ctx, "docker", "image", "inspect", "--format", "{{range .RepoTags}}{{.}} {{end}}", id)
		if err != nil {
			continue // already gone
		}
		refs := append(strings.Fields(string(out)), id)
		if _, err := r.Run.Run(ctx, "docker", append([]string{"image", "rm"}, refs...)...); err != nil {
			r.say("kept      image %s (%v)", strings.Join(refs[:len(refs)-1], " "), err)
			continue
		}
		n++
	}
	if n > 0 {
		r.say("removed   %d images", n)
	}
	_, _ = r.Run.Run(ctx, "docker", "image", "prune", "--force")
	return nil
}

// fabric takes the rails down by removing OAF's netplan file.
func (r resetter) fabric(ctx context.Context) error {
	p := r.path("/etc/netplan/40-oaf-fabric.yaml")
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	if _, err := r.Run.Run(ctx, "netplan", "apply"); err != nil {
		return fmt.Errorf("netplan apply: %w", err)
	}
	r.say("removed   fabric addresses (/etc/netplan/40-oaf-fabric.yaml)")
	return nil
}

// files removes OAF's directories, the registry CA it trusted and, last,
// the binary itself (the running process keeps its copy).
func (r resetter) files() error {
	var errs []error
	rm := func(p string) {
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return
		}
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
			return
		}
		r.say("removed   %s", strings.TrimPrefix(p, strings.TrimSuffix(r.Root, "/")))
	}
	if ca, err := r.registryCAs(); err == nil {
		for _, d := range ca {
			rm(d)
		}
	}
	rm(r.path("/etc/oaf"))
	lib := r.path("/var/lib/oaf")
	if r.KeepWeights {
		entries, _ := os.ReadDir(lib)
		for _, e := range entries {
			if e.Name() != "models" {
				rm(filepath.Join(lib, e.Name()))
			}
		}
	} else {
		rm(lib)
	}
	rm(r.path("/opt/oaf"))
	rm(r.path("/usr/local/bin/oaf"))
	return errors.Join(errs...)
}

// registryCAs are the /etc/docker/certs.d entries that hold the cluster CA:
// OAF trusts its own registry there (TrustRegistry). Others are left alone.
func (r resetter) registryCAs() ([]string, error) {
	cluster, err := os.ReadFile(r.path("/etc/oaf/pki/ca.crt"))
	if err != nil {
		return nil, err
	}
	dir := r.path(node.DockerCertsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		ca, err := os.ReadFile(filepath.Join(dir, e.Name(), "ca.crt"))
		if err == nil && strings.TrimSpace(string(ca)) == strings.TrimSpace(string(cluster)) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
