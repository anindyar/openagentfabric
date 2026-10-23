package secrets

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestReplaceRotatesAndSetRestores(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "secrets")}
	orig, err := s.Value("gateway-key-app")
	if err != nil {
		t.Fatal(err)
	}
	newV, oldV, err := s.Replace("gateway-key-app")
	if err != nil {
		t.Fatal(err)
	}
	if oldV != orig || newV == orig || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(newV) {
		t.Fatalf("Replace = %q (old %q), original %q", newV, oldV, orig)
	}
	if got, _ := s.Value("gateway-key-app"); got != newV {
		t.Fatalf("after Replace the store reads %q, want the new value", got)
	}
	fi, _ := os.Stat(filepath.Join(s.Dir, "gateway-key-app"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("replaced secret mode %o, want 600", fi.Mode().Perm())
	}

	// Undoing a rotation whose apply failed.
	if err := s.Set("gateway-key-app", orig); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Value("gateway-key-app"); got != orig {
		t.Fatalf("after Set the store reads %q, want the original", got)
	}
	if err := s.Set("gateway-key-app", " "); err == nil {
		t.Fatal("Set accepted an empty value")
	}
	entries, _ := os.ReadDir(s.Dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1 (temp files must be cleaned up)", len(entries))
	}
}
