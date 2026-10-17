// Package secrets keeps the per-install generated secrets: database
// passwords, Langfuse keys, gateway API keys. Each secret is one 0600 file
// in a 0700 directory (default /etc/oaf/secrets) and is generated on first
// use, so an install never ships or commits a secret value.
package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultDir is where an install keeps its secrets.
const DefaultDir = "/etc/oaf/secrets"

// Source provides secret values by name.
type Source interface {
	Value(name string) (string, error)
}

// validName matches the secretRef pattern in schema/cluster.schema.json.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Store is a directory of secret files.
type Store struct {
	Dir string
}

// Value returns the named secret, generating it if it does not exist yet.
// Generated values are 64 lowercase hex characters (256 bits), which are
// safe in URLs, env files and compose interpolation.
func (s Store) Value(name string) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	v, ok, err := s.read(name)
	if err != nil || ok {
		return v, err
	}
	v, err = generate()
	if err != nil {
		return "", err
	}
	if err := write(s.Dir, name, v); err != nil {
		return "", err
	}
	// Re-read so that a concurrent writer that won the race decides the value.
	return s.Value(name)
}

func (s Store) read(name string) (string, bool, error) {
	p := filepath.Join(s.Dir, name)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", false, fmt.Errorf("secret %s is empty", p)
	}
	return v, true, nil
}

// Placeholder stands in for a secret that Preview has not generated.
var Placeholder = strings.Repeat("0", 64)

// Preview reads existing secrets but never creates one: a missing secret
// reads as Placeholder. A dry run uses it so that it changes nothing.
type Preview struct {
	Store Store
}

// Value implements Source.
func (p Preview) Value(name string) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	v, ok, err := p.Store.read(name)
	if err != nil || ok {
		return v, err
	}
	return Placeholder, nil
}

func generate() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// write creates dir/name with value, without replacing an existing file.
func write(dir, name, value string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Link fails if the name exists, so a secret is never overwritten.
	if err := os.Link(tmp.Name(), filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}
