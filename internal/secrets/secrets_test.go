package secrets

import (
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
)

func TestValueGeneratesOnceAndPersists(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "secrets")}
	v1, err := s.Value("postgres-password")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(v1) {
		t.Fatalf("generated %q, want 64 hex chars", v1)
	}
	v2, err := s.Value("postgres-password")
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Fatal("second read returned a different value")
	}
	other, _ := s.Value("valkey-password")
	if other == v1 {
		t.Fatal("two secrets got the same value")
	}

	fi, err := os.Stat(filepath.Join(s.Dir, "postgres-password"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode %o, want 600", fi.Mode().Perm())
	}
	di, _ := os.Stat(s.Dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o, want 700", di.Mode().Perm())
	}
	entries, _ := os.ReadDir(s.Dir)
	if len(entries) != 2 {
		t.Fatalf("dir has %d entries, want 2 (temp files must be cleaned up)", len(entries))
	}
}

func TestValueKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gateway-key-default"), []byte("user-chosen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := Store{Dir: dir}.Value("gateway-key-default")
	if err != nil || v != "user-chosen" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestValueRejectsBadNamesAndEmpty(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}
	for _, n := range []string{"", "../x", "A", "a/b", ".hidden"} {
		if _, err := s.Value(n); err == nil {
			t.Errorf("name %q accepted", n)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "empty"), []byte("\n"), 0o600)
	if _, err := s.Value("empty"); err == nil {
		t.Error("empty secret accepted")
	}
}

func TestValueConcurrentCallersAgree(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	var wg sync.WaitGroup
	got := make([]string, 16)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.Value("race")
			if err != nil {
				t.Error(err)
			}
			got[i] = v
		}()
	}
	wg.Wait()
	for _, v := range got {
		if v != got[0] {
			t.Fatal("concurrent callers saw different values")
		}
	}
}
