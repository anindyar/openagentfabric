package join

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// PendingNode is a box that joined but is not in cluster.yaml yet: it waits
// for the admin to choose a topology.
type PendingNode struct {
	Name       string    `json:"name"`
	Address    string    `json:"address"` // the LAN address the control node reaches its agent on
	Hardware   Hardware  `json:"hardware"`
	EnrolledAt time.Time `json:"enrolled_at"`
	TokenID    string    `json:"token_id,omitempty"`
}

// PendingStore keeps pending nodes in a file under the state dir.
type PendingStore struct {
	Path string
	mu   sync.Mutex
}

func (s *PendingStore) load() ([]PendingNode, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ns []PendingNode
	return ns, json.Unmarshal(b, &ns)
}

func (s *PendingStore) save(ns []PendingNode) error {
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	b, err := json.MarshalIndent(ns, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Put adds a pending node or replaces the one with its name (a re-join).
func (s *PendingStore) Put(n PendingNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, err := s.load()
	if err != nil {
		return err
	}
	out := ns[:0]
	for _, x := range ns {
		if x.Name != n.Name {
			out = append(out, x)
		}
	}
	return s.save(append(out, n))
}

// List returns the pending nodes by name.
func (s *PendingStore) List() ([]PendingNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Remove drops a pending node (it was added to cluster.yaml, or rejected).
// It reports whether one was removed.
func (s *PendingStore) Remove(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, err := s.load()
	if err != nil {
		return false, err
	}
	out := ns[:0]
	for _, x := range ns {
		if x.Name != name {
			out = append(out, x)
		}
	}
	if len(out) == len(ns) {
		return false, nil
	}
	return true, s.save(out)
}
