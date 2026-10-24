package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openagentfabric.dev/oaf/internal/config"
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

// --admin adds the management API's URL, a stable token and the fingerprint
// of the certificate oafd serves.
func TestAdminEnv(t *testing.T) {
	dir := t.TempDir()
	apiStateDir = filepath.Join(dir, "state")
	defer func() { apiStateDir = DefaultStateDir }()
	f := &stackFlags{cluster: "../../examples/cluster.single.yaml", catalog: "../../examples/catalog.yaml",
		secretsDir: filepath.Join(dir, "secrets"), pkiDir: filepath.Join(dir, "pki")}
	c, err := config.LoadCluster(f.cluster)
	if err != nil {
		t.Fatal(err)
	}
	env, err := adminEnv(f, c, "garrison", "host.docker.internal")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OAF_API_URL=https://host.docker.internal:8443\n", "OAF_API_TOKEN=", "OAF_API_CERT_SHA256="} {
		if !strings.Contains(env, want) {
			t.Fatalf("env lacks %q:\n%s", want, env)
		}
	}
	again, _ := adminEnv(f, c, "garrison", "host.docker.internal")
	if again != env {
		t.Fatal("token or certificate changed on a second issue")
	}
	tok, ok, _ := secrets.Store{Dir: f.secretsDir}.Lookup("oaf-admin-garrison")
	if !ok || !strings.Contains(env, "OAF_API_TOKEN="+tok+"\n") {
		t.Fatal("token not stored as oaf-admin-garrison")
	}
	tlsCfg, fp, err := apiTLS(f.pkiDir, apiStateDir, c)
	if err != nil || tlsCfg == nil || !strings.Contains(env, "OAF_API_CERT_SHA256="+fp+"\n") {
		t.Fatalf("served certificate differs from the printed fingerprint: %v", err)
	}
}
