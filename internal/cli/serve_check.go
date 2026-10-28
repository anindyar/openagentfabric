package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"openagentfabric.dev/oaf/bundle"
	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/fabric"
	"openagentfabric.dev/oaf/internal/secrets"
)

func bundleVersions() *config.Versions {
	v, err := config.ParseVersions(bundle.VersionsYAML)
	if err != nil {
		return nil
	}
	return v
}

// renderCheck renders a candidate cluster.yaml the way apply would (images,
// licences, secrets as previews, fabric discovery), without writing it or
// touching any node, so oafd refuses a change that cannot apply.
func renderCheck(ctx context.Context, f *stackFlags, stateDir string, doc []byte) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, ".candidate-*.yaml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(doc); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	cand := *f
	cand.cluster = filepath.Clean(tmp.Name())
	s, err := cand.load(io.Discard)
	if err != nil {
		return err
	}
	_, err = s.render(ctx, secrets.Preview{Store: secrets.Store{Dir: f.secretsDir}})
	// Rails without addresses are expected when the change adds a cable:
	// the apply addresses them, then renders for real before deploying.
	if errors.Is(err, fabric.ErrNotAddressed) {
		return nil
	}
	return err
}
