package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/keys"
	"openagentfabric.dev/oaf/internal/render"
	"openagentfabric.dev/oaf/internal/secrets"
)

// Apps get Langfuse settings only when Langfuse's keys exist, and asking
// never generates them.
func TestAppEnvLangfuse(t *testing.T) {
	dir := t.TempDir()
	f := &stackFlags{cluster: "../../examples/cluster.single.yaml", catalog: "../../examples/catalog.yaml", secretsDir: dir}

	env, err := appEnv(f, keys.Credentials{Key: "k"}, "host.docker.internal")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(env, "LANGFUSE_") {
		t.Fatalf("Langfuse settings without Langfuse keys:\n%s", env)
	}
	if _, err := os.Stat(filepath.Join(dir, render.SecretLangfusePublicKey)); err == nil {
		t.Fatal("appEnv generated a Langfuse key")
	}

	store := secrets.Store{Dir: dir}
	if err := store.Set(render.SecretLangfusePublicKey, "pub"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(render.SecretLangfuseSecretKey, "sec"); err != nil {
		t.Fatal(err)
	}
	env, err = appEnv(f, keys.Credentials{Key: "k"}, "host.docker.internal")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"LANGFUSE_HOST=http://host.docker.internal:3000\n", "LANGFUSE_PUBLIC_KEY=pk-lf-pub\n", "LANGFUSE_SECRET_KEY=sk-lf-sec\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
}
