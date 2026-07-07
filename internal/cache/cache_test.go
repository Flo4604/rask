package cache

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCachePolicy protects module sharding, deletion, and module-scoped clean behavior.
func TestCachePolicy(t *testing.T) {
	rootDir := t.TempDir()
	cacheA := openCacheAt(t, rootDir, "example.com/m")
	cacheB := openCacheAt(t, rootDir, "example.com/m")
	cacheOther := openCacheAt(t, rootDir, "example.com/other")
	digest := digestForTest("a")
	if err := cacheA.Put(context.Background(), digest, entryForTest()); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, found := cacheB.Get(context.Background(), digest); !found {
		t.Fatalf("same module shard must share entries")
	}
	if err := cacheOther.Put(context.Background(), digest, entryForTest()); err != nil {
		t.Fatalf("put other: %v", err)
	}
	cacheA.Delete(context.Background(), digest)
	if _, found := cacheB.Get(context.Background(), digest); found {
		t.Fatalf("failed suites must delete stale entries")
	}
	if _, found := cacheOther.Get(context.Background(), digest); !found {
		t.Fatalf("other module shard must remain")
	}
	if err := cacheA.Put(context.Background(), digest, entryForTest()); err != nil {
		t.Fatalf("put again: %v", err)
	}
	if err := cacheA.RemoveModule(); err != nil {
		t.Fatalf("remove module: %v", err)
	}
	if _, found := cacheB.Get(context.Background(), digest); found {
		t.Fatalf("module-scoped clean must remove the module shard")
	}
	if _, found := cacheOther.Get(context.Background(), digest); !found {
		t.Fatalf("module-scoped clean must leave other modules alone")
	}
}

// TestCacheSelfHealsPrunesAndReadBackMissIsError protects corrupt-object and clean-race behavior.
func TestCacheSelfHealsPrunesAndReadBackMissIsError(t *testing.T) {
	resultCache := openCacheAt(t, t.TempDir(), "example.com/m")
	digest := digestForTest("b")
	path := filepath.Join(resultCache.store.(*diskStore).rootDir, filepath.FromSlash(resultCache.objectKey(digest)))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if _, found := resultCache.Get(context.Background(), digest); found {
		t.Fatalf("corrupt object must miss")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt object must be deleted")
	}
	if err := newResultCache(&disappearingStore{}, "example.com/m").Put(context.Background(), digest, entryForTest()); err == nil {
		t.Fatalf("read-back miss must be an error")
	}
}

// TestDiskStoreRejectsNonCacheDirectory protects hostile RASK_CACHE_DIR safety.
func TestDiskStoreRejectsNonCacheDirectory(t *testing.T) {
	rootDir := t.TempDir()
	writeFile(t, rootDir, "foreign.txt", "keep me\n")
	if _, err := newDiskStore(rootDir); err == nil {
		t.Fatalf("non-empty untagged root must be rejected")
	}
	if got := readFile(t, rootDir, "foreign.txt"); got != "keep me\n" {
		t.Fatalf("foreign file changed to %q", got)
	}
	if err := os.RemoveAll(rootDir); err != nil {
		t.Fatalf("remove root: %v", err)
	}
	if _, err := newDiskStore(rootDir); err != nil {
		t.Fatalf("new root should be created and tagged: %v", err)
	}
}

// TestDiskStoreCleanAllRemovesOnlyRaskOwnedEntries protects whole-store clean safety.
func TestDiskStoreCleanAllRemovesOnlyRaskOwnedEntries(t *testing.T) {
	rootDir := t.TempDir()
	store, err := newDiskStore(rootDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	writeFile(t, rootDir, "foreign.txt", "keep me\n")
	writeFile(t, filepath.Join(rootDir, "v1"), "owned.json", "{}")
	writeFile(t, filepath.Join(rootDir, "v2"), "owned.json", "{}")
	writeFile(t, filepath.Join(rootDir, "vX"), "foreign.json", "{}")
	if err := store.RemovePrefix(""); err != nil {
		t.Fatalf("clean all: %v", err)
	}
	assertMissing(t, filepath.Join(rootDir, "v1"))
	assertMissing(t, filepath.Join(rootDir, "v2"))
	assertMissing(t, filepath.Join(rootDir, "CACHEDIR.TAG"))
	assertExists(t, filepath.Join(rootDir, "vX"))
	if got := readFile(t, rootDir, "foreign.txt"); got != "keep me\n" {
		t.Fatalf("foreign file changed to %q", got)
	}
}

// TestDiskStoreCleanRefusesGenericCacheDirTag protects delete-site tag proof.
func TestDiskStoreCleanRefusesGenericCacheDirTag(t *testing.T) {
	for name, tag := range map[string]string{
		"signature-only":  "Signature: 8a477f597d28d172789f06886806bc55\n",
		"foreign-comment": "Signature: 8a477f597d28d172789f06886806bc55\n# Written by another tool.\n",
	} {
		t.Run(name, func(t *testing.T) {
			rootDir := t.TempDir()
			writeFile(t, rootDir, "CACHEDIR.TAG", tag)
			writeFile(t, filepath.Join(rootDir, "v1"), "owned.json", "{}")
			resultCache := newResultCache(&diskStore{rootDir: rootDir}, "example.com/m")
			if err := resultCache.RemoveModule(); err == nil {
				t.Fatalf("module clean accepted a generic CACHEDIR.TAG")
			}
			if err := resultCache.store.RemovePrefix(""); err == nil {
				t.Fatalf("whole-store clean accepted a generic CACHEDIR.TAG")
			}
			assertExists(t, filepath.Join(rootDir, "CACHEDIR.TAG"))
			assertExists(t, filepath.Join(rootDir, "v1", "owned.json"))
		})
	}
}

// TestDiskStoreCleanRefusesTagRemovedAfterOpen protects delete-time validation.
func TestDiskStoreCleanRefusesTagRemovedAfterOpen(t *testing.T) {
	rootDir := t.TempDir()
	resultCache := openCacheAt(t, rootDir, "example.com/m")
	digest := digestForTest("c")
	if err := resultCache.Put(context.Background(), digest, entryForTest()); err != nil {
		t.Fatalf("put: %v", err)
	}
	store := resultCache.store.(*diskStore)
	objectPath := store.keyPath(resultCache.objectKey(digest))
	if err := os.Remove(filepath.Join(rootDir, "CACHEDIR.TAG")); err != nil {
		t.Fatalf("remove tag: %v", err)
	}
	if err := resultCache.RemoveModule(); err == nil {
		t.Fatalf("module clean accepted a missing CACHEDIR.TAG")
	}
	assertExists(t, objectPath)
	if err := store.RemovePrefix(""); err == nil {
		t.Fatalf("whole-store clean accepted a missing CACHEDIR.TAG")
	}
	assertExists(t, objectPath)
}

// TestCachePruneRemovesOnlyOldTemporaryObjects protects crash orphan cleanup.
func TestCachePruneRemovesOnlyOldTemporaryObjects(t *testing.T) {
	resultCache := openCacheAt(t, t.TempDir(), "example.com/m")
	oldTmp := resultCache.objectKey(digestForTest("d")) + ".tmp.123"
	freshTmp := resultCache.objectKey(digestForTest("e")) + ".tmp.456"
	store := resultCache.store.(*diskStore)
	writeKey(t, store, oldTmp, "{}")
	writeKey(t, store, freshTmp, "{}")
	old := time.Now().Add(-2 * ageMax)
	if err := os.Chtimes(store.keyPath(oldTmp), old, old); err != nil {
		t.Fatalf("chtime old tmp: %v", err)
	}
	if err := resultCache.prune(time.Now(), ageMax, objectCountPerModuleMax); err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertMissing(t, store.keyPath(oldTmp))
	assertExists(t, store.keyPath(freshTmp))
}

// openCacheAt opens a disk-backed result cache at rootDir for modulePath.
func openCacheAt(t *testing.T, rootDir string, modulePath string) *Cache {
	t.Helper()
	store, err := newDiskStore(rootDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return newResultCache(store, modulePath)
}

// digestForTest returns a cache-shaped digest from a repeated seed.
func digestForTest(seed string) string {
	return strings.Repeat(seed, hashHexLength)
}

// entryForTest returns a valid passing cache entry.
func entryForTest() Entry {
	return Entry{
		ImportPath: "example.com/m/pkg", Passed: 1, Ignored: 1,
		ElapsedSeconds: 0.25, CreatedAt: time.Now(),
	}
}

func entryJSONForTest(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(entryForTest())
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	return body
}

// writeFile creates a fixture file for disk-store tests.
func writeFile(t *testing.T, dir string, name string, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// readFile returns one fixture file body.
func readFile(t *testing.T, dir string, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// writeKey writes one object-store key directly for prune tests.
func writeKey(t *testing.T, store *diskStore, key string, content string) {
	t.Helper()
	path := store.keyPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir key: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// assertExists fails when path is absent.
func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s should exist: %v", path, err)
	}
}

// assertMissing fails when path still exists.
func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists or stat failed: %v", path, err)
	}
}

// disappearingStore simulates a concurrent clean between Put and read-back verification.
type disappearingStore struct{}

// Get always reports a missing object.
func (s *disappearingStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

// Put accepts writes without retaining them.
func (s *disappearingStore) Put(context.Context, string, []byte) error {
	return nil
}

// Delete accepts deletes without retaining state.
func (s *disappearingStore) Delete(context.Context, string) error {
	return nil
}

// Touch accepts touches without retaining state.
func (s *disappearingStore) Touch(context.Context, string) error {
	return nil
}

// List reports no objects.
func (s *disappearingStore) List(string) ([]storedObject, error) {
	return nil, nil
}

// RemovePrefix accepts prefix removal without retaining state.
func (s *disappearingStore) RemovePrefix(string) error {
	return nil
}
