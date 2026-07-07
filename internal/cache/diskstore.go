package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chronark/rask/internal/assert"
)

const (
	// cacheDirTag is the two-line CACHEDIR.TAG payload that proves rask owns the
	// store. Destructive paths validate both lines immediately before deletion.
	cacheDirTag = "Signature: 8a477f597d28d172789f06886806bc55\n" +
		"# This directory is a result cache written by rask.\n"
)

// cacheVersionDirPattern identifies top-level cache generations owned by rask.
var cacheVersionDirPattern = regexp.MustCompile(`^v[0-9]+$`)

// storeKeyPattern allows only relative object-store keys.
var storeKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9._/-]*$`)

// diskStore stores objects as files under one root directory.
type diskStore struct {
	rootDir string
}

// openObjectStore wires the always-on disk store to any configured remote tier.
func openObjectStore(cfg Config) (objectStore, error) {
	disk, err := newDiskStore(cfg.DiskDir)
	if err != nil {
		return nil, err
	}
	remote, err := openS3ObjectStore(cfg.Remote)
	if err != nil {
		return nil, err
	}
	if remote == nil {
		return disk, nil
	}
	return newTieredStore(disk, remote), nil
}

// newDiskStore opens a disk store rooted at rootDir and writes CACHEDIR.TAG.
func newDiskStore(rootDir string) (*diskStore, error) {
	absolute, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolving cache dir: %w", err)
	}
	if absolute == string(filepath.Separator) {
		return nil, errors.New("refusing to root the cache at the filesystem root")
	}
	created, err := ensureStoreRoot(absolute)
	if err != nil {
		return nil, err
	}
	store := &diskStore{rootDir: absolute}
	if err := store.ensureCacheDirTag(created); err != nil {
		return nil, err
	}
	return store, nil
}

// Get returns the object body and true, or nil and false when the object is absent.
//
// The context is ignored because these local file operations are microsecond
// scale and not meaningfully cancellable through the os package; it exists so
// the tiered store can pass one cancellation-aware contract to every tier.
func (s *diskStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	body, err := os.ReadFile(s.keyPath(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading object %s: %w", key, err)
	}
	return body, true, nil
}

// Put writes body to key through a temporary file followed by rename.
//
// The context is ignored because local write-then-rename has no useful
// cancellation seam and should remain atomic once started.
func (s *diskStore) Put(_ context.Context, key string, body []byte) error {
	path := s.keyPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating object dir: %w", err)
	}
	temporary := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(temporary, body, 0o644); err != nil {
		return fmt.Errorf("writing object: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("committing object: %w", err)
	}
	return nil
}

// Delete validates rask ownership before removing key and treats a miss as success.
//
// The context is ignored because local deletion is bounded by one filesystem
// operation after the ownership proof has been read.
func (s *diskStore) Delete(_ context.Context, key string) error {
	if err := validateOwnTag(s.rootDir); err != nil {
		return fmt.Errorf("refusing object delete: %w", err)
	}
	err := os.Remove(s.keyPath(key))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("deleting object %s: %w", key, err)
}

// Touch updates key's modification time to mark it recently used.
//
// The context is ignored because updating local timestamps has no cancellable
// syscall boundary exposed by the standard library.
func (s *diskStore) Touch(_ context.Context, key string) error {
	now := time.Now()
	if err := os.Chtimes(s.keyPath(key), now, now); err != nil {
		return fmt.Errorf("touching object %s: %w", key, err)
	}
	return nil
}

// List returns objects under prefix with modification times used for pruning.
func (s *diskStore) List(prefix string) ([]storedObject, error) {
	assertStoreKey(prefix)
	prefixPath := filepath.Join(s.rootDir, filepath.FromSlash(prefix))
	if _, err := os.Stat(prefixPath); os.IsNotExist(err) {
		return nil, nil
	}
	objects := make([]storedObject, 0, 256)
	walkErr := filepath.WalkDir(prefixPath, func(path string, entry fs.DirEntry, err error) error {
		// Objects vanishing mid-walk are concurrent cleans, not listing failures.
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		relative, err := filepath.Rel(s.rootDir, path)
		assert.NoError(err)
		objects = append(objects, storedObject{Key: filepath.ToSlash(relative), LastUsedAt: info.ModTime()})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("listing %s: %w", prefix, walkErr)
	}
	return objects, nil
}

// RemovePrefix validates rask ownership before removing prefix, or rask-owned
// roots for whole-store clean.
func (s *diskStore) RemovePrefix(prefix string) error {
	assertStoreKey(prefix)
	if prefix == "" {
		return s.removeEverything()
	}
	if err := validateOwnTag(s.rootDir); err != nil {
		return fmt.Errorf("refusing prefix delete: %w", err)
	}
	target := filepath.Join(s.rootDir, filepath.FromSlash(prefix))
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("removing %s: %w", prefix, err)
	}
	return nil
}

// ensureCacheDirTag admits only empty, newly-created, or already-tagged roots.
func (s *diskStore) ensureCacheDirTag(created bool) error {
	tagPath := filepath.Join(s.rootDir, "CACHEDIR.TAG")
	_, err := os.ReadFile(tagPath)
	if err == nil {
		return validateOwnTag(s.rootDir)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("reading cache tag: %w", err)
	}
	if !created && !s.rootEmpty() {
		return fmt.Errorf("cache dir %s lacks rask CACHEDIR.TAG; refusing non-empty directory", s.rootDir)
	}
	return os.WriteFile(tagPath, []byte(cacheDirTag), 0o644)
}

// removeEverything re-validates rask's two-line CACHEDIR.TAG immediately before
// deleting only rask-owned top-level entries from the store.
func (s *diskStore) removeEverything() error {
	if err := validateOwnTag(s.rootDir); err != nil {
		return fmt.Errorf("refusing whole-store clean: %w", err)
	}
	entries, err := os.ReadDir(s.rootDir)
	if err != nil {
		return fmt.Errorf("listing store root: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != "CACHEDIR.TAG" && !cacheVersionDirPattern.MatchString(entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.rootDir, entry.Name())); err != nil {
			return fmt.Errorf("removing %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// rootEmpty reports whether the store root has no entries at all.
func (s *diskStore) rootEmpty() bool {
	entries, err := os.ReadDir(s.rootDir)
	return err == nil && len(entries) == 0
}

// keyPath validates key and maps it to a path under the store root.
func (s *diskStore) keyPath(key string) string {
	assertStoreKey(key)
	path := filepath.Join(s.rootDir, filepath.FromSlash(key))
	assert.True(strings.HasPrefix(path, s.rootDir+string(filepath.Separator)))
	return path
}

// ensureStoreRoot creates rootDir and reports whether this invocation created it.
func ensureStoreRoot(rootDir string) (bool, error) {
	_, err := os.Stat(rootDir)
	if err == nil {
		return false, nil
	}
	if !os.IsNotExist(err) {
		return false, fmt.Errorf("checking cache dir: %w", err)
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return false, fmt.Errorf("creating cache dir: %w", err)
	}
	return true, nil
}

// validateOwnTag requires the exact two-line rask CACHEDIR.TAG at rootDir.
func validateOwnTag(rootDir string) error {
	body, err := os.ReadFile(filepath.Join(rootDir, "CACHEDIR.TAG"))
	if err != nil {
		return fmt.Errorf("cache dir %s lacks rask CACHEDIR.TAG: %w", rootDir, err)
	}
	if string(body) != cacheDirTag {
		return fmt.Errorf("cache dir %s has non-rask CACHEDIR.TAG", rootDir)
	}
	return nil
}

// assertStoreKey rejects absolute, traversal, and malformed object keys.
func assertStoreKey(key string) {
	assert.True(storeKeyPattern.MatchString(key), "malformed store key %q", key)
	assert.False(strings.Contains(key, ".."), "store key %q must not traverse", key)
	assert.False(strings.HasPrefix(key, "/"), "store key %q must be relative", key)
}
