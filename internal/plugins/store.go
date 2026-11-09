package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
)

// Limits of one plugin's tree (I268).
const (
	maxTreeFiles = 20000
	maxTreeBytes = 1 << 30
)

// treeEntry is one regular file of a plugin tree.
type treeEntry struct {
	rel        string // slash-separated, relative to the tree's root
	executable bool
	size       int64
}

// scanTree lists a tree, refusing anything that is not a directory or a plain file. A
// symlink could point anywhere, and a setuid bit has no business in a plugin.
func scanTree(root string) ([]treeEntry, []string, error) {
	var files []treeEntry
	var dirs []string
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if rel != "." {
				dirs = append(dirs, rel)
			}
			return nil
		case !info.Mode().IsRegular():
			return errs.Newf(errs.CodeValidationFailed, "plugin file %q is not a regular file: symlinks and devices are refused", rel)
		}
		if info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
			return errs.Newf(errs.CodeValidationFailed, "plugin file %q has a setuid, setgid or sticky bit", rel)
		}
		total += info.Size()
		if total > maxTreeBytes {
			return errs.New(errs.CodeValidationFailed, "plugin tree is larger than the limit")
		}
		files = append(files, treeEntry{rel: rel, executable: info.Mode().Perm()&0o111 != 0, size: info.Size()})
		if len(files) > maxTreeFiles {
			return errs.New(errs.CodeValidationFailed, "plugin tree has more files than the limit")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(a, b int) bool { return files[a].rel < files[b].rel })
	return files, dirs, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// treeDigest covers every file's path, executable bit and content, so a changed script
// module or manifest is caught, and not only a changed entrypoint (I267).
func treeDigest(root string) (string, error) {
	files, _, err := scanTree(root)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, f := range files {
		sum, err := hashFile(filepath.Join(root, filepath.FromSlash(f.rel)))
		if err != nil {
			return "", err
		}
		exec := "0"
		if f.executable {
			exec = "1"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\n", f.rel, exec, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// entrypointPath resolves a manifest's entrypoint inside a tree.
func entrypointPath(root string, m plugin.Manifest) string {
	return filepath.Join(root, filepath.FromSlash(m.Entrypoint.Command))
}

// checkTree applies the filesystem half of validation: the entrypoint exists inside
// the tree, is executable, and matches the manifest's hash when it declares one.
func checkTree(root string, m plugin.Manifest) error {
	entry := entrypointPath(root, m)
	rel, err := filepath.Rel(root, entry)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errs.New(errs.CodeValidationFailed, "entrypoint command must stay inside the plugin directory")
	}
	info, err := os.Lstat(entry)
	if err != nil {
		return errs.Newf(errs.CodeValidationFailed, "entrypoint %q does not exist", m.Entrypoint.Command)
	}
	if !info.Mode().IsRegular() {
		return errs.Newf(errs.CodeValidationFailed, "entrypoint %q is not a regular file", m.Entrypoint.Command)
	}
	if info.Mode().Perm()&0o100 == 0 {
		return errs.Newf(errs.CodeValidationFailed, "entrypoint %q is not executable", m.Entrypoint.Command)
	}
	if m.SHA256 != "" {
		sum, err := hashFile(entry)
		if err != nil {
			return fmt.Errorf("hash entrypoint: %w", err)
		}
		if sum != m.SHA256 {
			return errs.New(errs.CodeValidationFailed, "entrypoint does not match the manifest's sha256")
		}
	}
	return nil
}

// store is the private copy of installed plugins. Trees in it are read-only, so a
// sandboxed plugin can never change one, and never move under a running process.
type store struct {
	dir string
}

func (s *store) path(id, version, digest string) string {
	return filepath.Join(s.dir, id, version+"-"+digest[:12])
}

// installed is the result of bringing a directory into the store.
type installed struct {
	manifest plugin.Manifest
	raw      []byte
	digest   string
	path     string
}

// load validates a source tree without changing anything.
func loadTree(src string) (installed, error) {
	raw, err := os.ReadFile(filepath.Join(src, plugin.ManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return installed{}, errs.Newf(errs.CodeValidationFailed, "no %s in the plugin directory", plugin.ManifestFile)
	}
	if err != nil {
		return installed{}, errs.Wrap(errs.CodeValidationFailed, err, "cannot read the plugin manifest")
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return installed{}, err
	}
	if err := Validate(m); err != nil {
		return installed{}, err
	}
	if err := checkTree(src, m); err != nil {
		return installed{}, err
	}
	digest, err := treeDigest(src)
	if err != nil {
		return installed{}, err
	}
	return installed{manifest: m, raw: raw, digest: digest}, nil
}

// add copies a validated tree into the store and returns its final path. It verifies the
// copy, so a source changing during the copy cannot be installed under the wrong digest.
func (s *store) add(src string, in installed) (string, error) {
	target := s.path(in.manifest.ID, in.manifest.Version, in.digest)
	if _, err := os.Stat(target); err == nil {
		digest, err := treeDigest(target)
		if err == nil && digest == in.digest {
			return target, nil
		}
		// A leftover that does not verify is replaced.
		if err := removeTree(target); err != nil {
			return "", fmt.Errorf("replace stale plugin copy: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", fmt.Errorf("create plugin store: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(target), ".install-*")
	if err != nil {
		return "", fmt.Errorf("create plugin store: %w", err)
	}
	defer func() { _ = removeTree(tmp) }()
	files, dirs, err := scanTree(src)
	if err != nil {
		return "", err
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(tmp, filepath.FromSlash(dir)), 0o750); err != nil {
			return "", fmt.Errorf("copy plugin: %w", err)
		}
	}
	for _, f := range files {
		if err := copyFile(filepath.Join(src, filepath.FromSlash(f.rel)), filepath.Join(tmp, filepath.FromSlash(f.rel)), f.executable); err != nil {
			return "", fmt.Errorf("copy plugin: %w", err)
		}
	}
	digest, err := treeDigest(tmp)
	if err != nil {
		return "", err
	}
	if digest != in.digest {
		return "", errs.New(errs.CodeConflict, "the plugin directory changed during install")
	}
	// Subdirectories go read-only first: renaming a read-only directory to a new parent
	// is refused by some systems, so the top one follows the rename.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(filepath.Join(tmp, filepath.FromSlash(dirs[i])), 0o555); err != nil {
			return "", fmt.Errorf("protect plugin copy: %w", err)
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", fmt.Errorf("publish plugin copy: %w", err)
	}
	if err := os.Chmod(target, 0o555); err != nil {
		return "", fmt.Errorf("protect plugin copy: %w", err)
	}
	return target, nil
}

func copyFile(src, dst string, executable bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	mode := os.FileMode(0o444)
	if executable {
		mode = 0o555
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// removeTree deletes a tree whose directories may be read-only.
func removeTree(root string) error {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o750)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// errIntegrity marks a hash mismatch, which takes the plugin out of service.
var errIntegrity = errors.New("plugin integrity check failed")

// verify recomputes both hashes of an installed plugin (I267).
func (s *store) verify(id, version, digest string, m plugin.Manifest) (string, error) {
	root := s.path(id, version, digest)
	got, err := treeDigest(root)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errIntegrity, err)
	}
	if got != digest {
		return "", fmt.Errorf("%w: the installed files changed", errIntegrity)
	}
	if err := checkTree(root, m); err != nil {
		return "", fmt.Errorf("%w: %v", errIntegrity, err)
	}
	return root, nil
}
