package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.HasPrefix(out.String(), "oaf dev") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestUnknownCommandFails(t *testing.T) {
	root := NewRoot()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"nope"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for an unknown command")
	}
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestValidateExample(t *testing.T) {
	out, err := runCLI(t, "validate", "-f", "../../examples/cluster.pair.yaml", "--catalog", "../../examples/catalog.yaml")
	if err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "valid (2 warning(s))") || !strings.Contains(out, "warning: spec.routing.rules[1]") || !strings.Contains(out, "vsr-encoder") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestValidateReportsErrors(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "cluster.yaml")
	src, err := os.ReadFile("../../examples/cluster.pair.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(string(src), "fast: fast", "fast: missing", 1)
	if err := os.WriteFile(bad, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "validate", "-f", bad, "--catalog", "../../examples/catalog.yaml")
	if err == nil {
		t.Fatalf("expected failure, got:\n%s", out)
	}
	if !strings.Contains(out, `error:   spec.routing.defaults.fast: "missing" is not an enabled deployment`) {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
