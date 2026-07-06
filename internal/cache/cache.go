// Package cache implements rask's content-addressed result store.
//
// The package owns the local disk object store, result entry validation,
// self-healing reads, pruning, module-scoped clean, and full-store clean. It
// does not know which suites should be cached; callers decide that policy and
// should only write passing suites.
//
// [Cache] operations are safe for concurrent use when the underlying
// filesystem is safe for concurrent file operations. The disk store uses
// write-then-rename for object writes, and cache misses caused by concurrent
// clean operations are reported according to each method's contract.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chronark/rask/internal/assert"
	appconfig "github.com/chronark/rask/internal/config"
)

const (
	// formatVersion separates incompatible cache object layouts.
	formatVersion = "v1"

	// ageMax is the maximum idle age kept by default pruning.
	ageMax = 30 * 24 * time.Hour

	// objectCountPerModuleMax is the default per-module object count after pruning.
	objectCountPerModuleMax = 4096

	// hashHexLength is the length of a SHA-256 digest rendered as lowercase hex.
	hashHexLength = 64

	// moduleKeyHexLength is the number of SHA-256 hex characters used for module shards.
	moduleKeyHexLength = 16

	// objectBytesMax bounds one JSON cache object.
	objectBytesMax = 16 << 10
)

// Config carries cache construction inputs resolved by the command edge.
type Config struct {
	// ModulePath keeps cache keys stable for one Go module.
	ModulePath string

	// DiskDir is explicit so cache construction never falls back to a user directory.
	DiskDir string

	// Remote is nil unless the process-edge config opted into the shared tier.
	Remote *appconfig.Remote
}

// Cache stores passing suite summaries under a module-specific prefix.
//
// Cache has no channels because cache lookups are request/response operations
// owned by orchestration policy. Its methods may be used while Run is waiting.
// Run exists for lifecycle uniformity: it blocks until ctx is cancelled, then
// performs one best-effort final prune before returning the prune error, if any.
type Cache struct {
	store        objectStore
	modulePrefix string
	remote       bool
}

// RemoteStats is a snapshot of successful remote cache traffic.
type RemoteStats struct {
	// Reads is the number of validated cache hits served by the remote tier.
	Reads int

	// Writes is the number of successful writes to the remote tier.
	Writes int
}

// Open constructs a module cache from already-resolved process configuration.
func Open(cfg Config) (*Cache, error) {
	assert.True(cfg.ModulePath != "", "cache module path is required")
	assert.True(cfg.DiskDir != "", "cache disk dir is required")
	store, err := openObjectStore(cfg)
	if err != nil {
		return nil, err
	}
	return newResultCache(store, cfg.ModulePath), nil
}

// Run waits for ctx cancellation and then prunes this module shard once.
//
// Cache owns no channels. Its error is written by the command service wrapper
// and observed only after the producer-service WaitGroup has completed.
func (c *Cache) Run(ctx context.Context) error {
	<-ctx.Done()
	return c.prune(time.Now(), ageMax, objectCountPerModuleMax)
}

// Get returns a valid cached entry and true, or a zero entry and false.
//
// Read errors, missing objects, oversized bodies, corrupt JSON, and invalid
// entries all behave as misses. Corrupt and invalid objects are deleted so
// future reads can heal without caller involvement.
func (c *Cache) Get(digest string) (Entry, bool) {
	key := c.objectKey(digest)
	body, found, err := c.store.Get(key)
	if err != nil || !found {
		return Entry{}, false
	}
	var entry Entry
	corrupt := len(body) > objectBytesMax ||
		json.Unmarshal(body, &entry) != nil ||
		!entryValid(entry)
	if corrupt {
		c.Delete(digest)
		return Entry{}, false
	}
	_ = c.store.Touch(key)
	return entry, true
}

// Put writes entry for digest and verifies the object by reading it back.
//
// A missing read-back is returned as an error because a concurrent clean is an
// external race. Treating it as success would let callers believe a result was
// cached when it was already removed.
func (c *Cache) Put(digest string, entry Entry) error {
	assert.True(entryValid(entry))
	body, err := json.Marshal(entry)
	assert.NoError(err)
	assert.LessOrEqual(len(body), objectBytesMax)
	if err := c.store.Put(c.objectKey(digest), body); err != nil {
		return err
	}
	stored, found := c.Get(digest)
	if !found {
		return fmt.Errorf("cache object disappeared during write verification")
	}
	assert.Equal(stored.Passed, entry.Passed)
	assert.Equal(stored.Ignored, entry.Ignored)
	return nil
}

// Delete removes the object for digest and ignores deletion errors.
func (c *Cache) Delete(digest string) {
	_ = c.store.Delete(c.objectKey(digest))
}

// RemoveModule re-validates rask's two-line CACHEDIR.TAG immediately before
// deleting this module shard, so every disk delete proves store ownership.
func (c *Cache) RemoveModule() error {
	if store, ok := c.store.(*diskStore); ok {
		if err := validateOwnTag(store.rootDir); err != nil {
			return fmt.Errorf("refusing module clean: %w", err)
		}
	}
	return c.store.RemovePrefix(c.modulePrefix)
}

// HasRemoteTier reports whether this cache also writes to a remote tier.
func (c *Cache) HasRemoteTier() bool {
	return c.remote
}

// RemoteStats returns successful remote cache traffic for this cache.
func (c *Cache) RemoteStats() RemoteStats {
	if store, ok := c.store.(*tieredStore); ok {
		return store.RemoteStats()
	}
	return RemoteStats{}
}

// Entry records the passing result summary stored for one suite digest.
type Entry struct {
	// ImportPath is the package import path that produced this cached result.
	ImportPath string

	// Passed is the number of passing tests in the suite.
	Passed int

	// Ignored is the number of skipped tests in the suite.
	Ignored int

	// ElapsedSeconds is the suite runtime reported by `go test -json`.
	ElapsedSeconds float64

	// CreatedAt is the time the cache object was created.
	CreatedAt time.Time
}

// CleanAll removes rask-owned version directories and the cache tag from one disk root.
func CleanAll(diskDir string) error {
	assert.True(diskDir != "", "cache disk dir is required")
	store, err := newDiskStore(diskDir)
	if err != nil {
		return err
	}
	return store.RemovePrefix("")
}

// store is the minimal storage contract used by cache pruning and test fakes.
type store interface {
	// Get returns a copied body, whether the key was found, and storage errors.
	Get(key string) ([]byte, bool, error)

	// Put stores body at key.
	Put(key string, body []byte) error

	// Delete removes key and should treat a missing object as success.
	Delete(key string) error

	// List returns objects under prefix with last-used timestamps.
	List(prefix string) ([]storedObject, error)
}

// objectStore is the internal storage contract used by [Cache].
type objectStore interface {
	store

	// Touch updates key's last-used timestamp.
	Touch(key string) error

	// RemovePrefix removes every object under prefix.
	RemovePrefix(prefix string) error
}

// storedObject keeps listing metadata package-local so storage details do not leak.
type storedObject struct {
	// Key is the slash-separated object key relative to the store root.
	Key string

	// LastUsedAt is the filesystem modification time used as the LRU timestamp.
	LastUsedAt time.Time
}

// newResultCache returns a module-sharded cache over store.
func newResultCache(store objectStore, modulePath string) *Cache {
	assert.NotNil(store)
	moduleKey := moduleKeyForPath(modulePath)
	return &Cache{
		store:        store,
		modulePrefix: formatVersion + "/" + moduleKey,
		remote:       storeHasRemote(store),
	}
}

// objectKey returns the object-store key for a suite digest.
func (c *Cache) objectKey(digest string) string {
	assert.True(validDigest(digest), "malformed digest %q", digest)
	parts := strings.Split(c.modulePrefix, "/")
	assert.Equal(len(parts), 2)
	return objectKeyForParts(parts[1], digest)
}

// prune removes old and excess objects from this module's cache shard.
func (c *Cache) prune(now time.Time, ageMax time.Duration, countMax int) error {
	return pruneStorePrefix(c.store, c.modulePrefix, now, ageMax, countMax)
}

// pruneStorePrefix applies age and count bounds to one module shard.
func pruneStorePrefix(
	store store,
	prefix string,
	now time.Time,
	ageMax time.Duration,
	countMax int,
) error {
	objects, err := store.List(prefix)
	if err != nil {
		return err
	}
	kept := pruneStoreByAge(store, objects, now.Add(-ageMax))
	pruneStoreByCount(store, kept, countMax)
	return nil
}

// pruneStoreByAge deletes stale JSON and crash-orphaned temp objects before cutoff.
func pruneStoreByAge(store store, objects []storedObject, cutoff time.Time) []storedObject {
	kept := make([]storedObject, 0, len(objects))
	for _, object := range objects {
		if temporaryObject(object.Key) {
			if object.LastUsedAt.Before(cutoff) {
				_ = store.Delete(object.Key)
			}
			continue
		}
		if !strings.HasSuffix(object.Key, ".json") {
			continue
		}
		if object.LastUsedAt.Before(cutoff) {
			_ = store.Delete(object.Key)
			continue
		}
		kept = append(kept, object)
	}
	return kept
}

// pruneStoreByCount deletes oldest objects until at most countMax remain.
func pruneStoreByCount(store store, objects []storedObject, countMax int) {
	if len(objects) <= countMax {
		return
	}
	sort.Slice(objects, func(a, b int) bool {
		return objects[a].LastUsedAt.Before(objects[b].LastUsedAt)
	})
	for _, object := range objects[:len(objects)-countMax] {
		_ = store.Delete(object.Key)
	}
}

// entryValid reports whether entry can be trusted as a passing suite summary.
func entryValid(entry Entry) bool {
	if entry.ImportPath == "" {
		return false
	}
	if entry.Passed < 0 || entry.Ignored < 0 {
		return false
	}
	return entry.ElapsedSeconds >= 0
}

// temporaryObject reports crash-orphaned write-then-rename cache files.
func temporaryObject(key string) bool {
	return strings.Contains(filepath.Base(key), ".tmp.")
}

// moduleKeyForPath returns the stable module shard used by every backend.
func moduleKeyForPath(modulePath string) string {
	digest := sha256.Sum256([]byte(modulePath))
	return hex.EncodeToString(digest[:])[:moduleKeyHexLength]
}

// objectKeyForParts returns the disk object key for validated module and digest parts.
func objectKeyForParts(moduleKey string, digest string) string {
	assert.True(validModuleKey(moduleKey), "malformed module key %q", moduleKey)
	assert.True(validDigest(digest), "malformed digest %q", digest)
	return formatVersion + "/" + moduleKey + "/" + digest[:2] + "/" + digest + ".json"
}

// validModuleKey reports whether key is a cache-safe module shard.
func validModuleKey(key string) bool {
	return validHex(key, moduleKeyHexLength)
}

// validDigest reports whether digest is a cache-safe suite digest.
func validDigest(digest string) bool {
	return validHex(digest, hashHexLength)
}

// validHex accepts only fixed-width lowercase hex without path metacharacters.
func validHex(value string, length int) bool {
	if len(value) != length || strings.ContainsAny(value, "/\\.") {
		return false
	}
	for _, r := range value {
		if !('0' <= r && r <= '9') && !('a' <= r && r <= 'f') {
			return false
		}
	}
	return true
}

// storeHasRemote reports whether store includes the optional remote tier.
func storeHasRemote(store objectStore) bool {
	_, ok := store.(*tieredStore)
	return ok
}
