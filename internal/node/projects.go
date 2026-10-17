package node

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	nodev1 "openagentfabric.dev/oaf/internal/gen/oaf/node/v1"
)

// Limits on what oafd may push to a node.
const (
	MaxProjectFiles = 64
	MaxProjectBytes = 16 << 20
	ComposeFile     = "compose.yaml"
)

var projectName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// validateName checks a project name.
func validateName(name string) error {
	if !projectName.MatchString(name) {
		return fmt.Errorf("invalid project name %q (lowercase letters, digits and '-', at most 40 characters)", name)
	}
	return nil
}

// validateFiles checks paths and sizes, and that compose.yaml is present.
func validateFiles(files []*nodev1.File) error {
	if len(files) == 0 || len(files) > MaxProjectFiles {
		return fmt.Errorf("a project needs 1 to %d files", MaxProjectFiles)
	}
	total, seen, hasCompose := 0, map[string]bool{}, false
	for _, f := range files {
		p := f.GetPath()
		clean := path.Clean(p)
		if p == "" || clean != p || path.IsAbs(p) || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.Contains(p, "\\") {
			return fmt.Errorf("invalid file path %q (must be a clean relative path inside the project)", p)
		}
		if seen[p] {
			return fmt.Errorf("duplicate file %q", p)
		}
		seen[p] = true
		if p == ComposeFile {
			hasCompose = true
		}
		if f.GetMode()&^0o777 != 0 {
			return fmt.Errorf("file %q: mode %o has bits outside 0777", p, f.GetMode())
		}
		total += len(f.GetContent())
	}
	if total > MaxProjectBytes {
		return fmt.Errorf("project is %d bytes; the limit is %d", total, MaxProjectBytes)
	}
	if !hasCompose {
		return errors.New("project has no " + ComposeFile)
	}
	return nil
}

func fileMode(f *nodev1.File) fs.FileMode {
	if f.GetMode() == 0 {
		return 0o644
	}
	return fs.FileMode(f.GetMode())
}

// digest identifies a desired file set: paths, modes and contents.
func digest(files []*nodev1.File) [32]byte {
	sorted := append([]*nodev1.File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetPath() < sorted[j].GetPath() })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%o\x00%d\x00", f.GetPath(), fileMode(f), len(f.GetContent()))
		h.Write(f.GetContent())
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}

// readDir loads an existing project directory as files, for comparison.
func readDir(dir string) ([]*nodev1.File, error) {
	var files []*nodev1.File
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, &nodev1.File{Path: filepath.ToSlash(rel), Content: b, Mode: uint32(info.Mode().Perm())})
		return nil
	})
	return files, err
}

// writeProject replaces dir with exactly the given files, atomically: the
// new tree is built beside it and swapped in with renames.
func writeProject(root, name string, files []*nodev1.File) error {
	dir := filepath.Join(root, name)
	tmp, err := os.MkdirTemp(root, "."+name+".new-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for _, f := range files {
		p := filepath.Join(tmp, filepath.FromSlash(f.GetPath()))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.GetContent(), fileMode(f)); err != nil {
			return err
		}
		if err := os.Chmod(p, fileMode(f)); err != nil { // not subject to umask
			return err
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	old := filepath.Join(root, "."+name+".old")
	_ = os.RemoveAll(old)
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.Rename(old, dir) // put the previous version back
		return err
	}
	return os.RemoveAll(old)
}

func sameFiles(a, b []*nodev1.File) bool {
	da, db := digest(a), digest(b)
	return bytes.Equal(da[:], db[:])
}
