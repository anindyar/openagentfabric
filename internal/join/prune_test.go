package join

import (
	"path/filepath"
	"testing"
)

// A box placed by a topology plan stops being listed as waiting.
func TestPruneDropsPlacedBoxes(t *testing.T) {
	ps := &PendingStore{Path: filepath.Join(t.TempDir(), "pending.json")}
	for _, n := range []string{"box-a", "box-b", "box-c"} {
		if err := ps.Put(PendingNode{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ps.Prune(map[string]bool{"box-a": true, "box-b": true, "control": true}); err != nil {
		t.Fatal(err)
	}
	ns, err := ps.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 || ns[0].Name != "box-c" {
		t.Fatalf("waiting = %+v, want only box-c", ns)
	}
	s := &Service{Pending: ps, Taken: func() map[string]bool { return map[string]bool{"box-c": true} }}
	if ns, _ := s.waiting(); len(ns) != 0 {
		t.Fatalf("waiting after box-c was placed = %+v", ns)
	}
}
