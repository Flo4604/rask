package cache

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	appconfig "github.com/chronark/rask/internal/config"
)

// TestOpenObjectStoreConfig protects always-on disk and bucket opt-in startup.
func TestOpenObjectStoreConfig(t *testing.T) {
	t.Run("disk-only default", func(t *testing.T) {
		resultCache, err := Open(Config{
			ModulePath: "example.com/m",
			DiskDir:    t.TempDir(),
			Remote:     nil,
		})
		if err != nil {
			t.Fatalf("open disk cache: %v", err)
		}
		if _, ok := resultCache.store.(*diskStore); !ok || resultCache.HasRemoteTier() {
			t.Fatalf("store=%T remote=%v, want plain disk", resultCache.store, resultCache.HasRemoteTier())
		}
	})
	t.Run("invalid endpoint fails loudly", func(t *testing.T) {
		_, err := Open(Config{
			ModulePath: "example.com/m",
			DiskDir:    t.TempDir(),
			Remote:     remoteConfigForTest("bucket", "", "not-a-url"),
		})
		if err == nil ||
			!strings.Contains(err.Error(), "URL must include scheme and host") {
			t.Fatalf("Open err=%v, want endpoint error", err)
		}
	})
	t.Run("bucket creates tier", func(t *testing.T) {
		resultCache, err := Open(Config{
			ModulePath: "example.com/m",
			DiskDir:    t.TempDir(),
			Remote:     remoteConfigForTest("bucket", "prefix", "http://rask.test"),
		})
		if err != nil {
			t.Fatalf("open tiered cache: %v", err)
		}
		if !resultCache.HasRemoteTier() {
			t.Fatalf("remote bucket must create a tiered cache")
		}
	})
}

// TestTieredStoreGetSemantics protects local hits, remote backfill, and misses.
func TestTieredStoreGetSemantics(t *testing.T) {
	local, remote, requests := newTieredCacheForTest(t)
	localDigest := digestForTest("1")
	if err := newResultCache(local, "example.com/m").Put(localDigest, entryForTest()); err != nil {
		t.Fatalf("seed local: %v", err)
	}
	requests.Reset()
	if _, found := newResultCache(newTieredStore(local, remote), "example.com/m").Get(localDigest); !found {
		t.Fatalf("local object must hit")
	}
	if requests.Total() != 0 {
		t.Fatalf("local hit made %d remote requests, want zero", requests.Total())
	}
	assertRemoteBackfill(t, local, remote, requests)
	assertTieredMiss(t, local, remote, requests)
}

// TestTieredStorePutAndDelete protects write-through and best-effort eviction.
func TestTieredStorePutAndDelete(t *testing.T) {
	local, remote, requests := newTieredCacheForTest(t)
	resultCache := newResultCache(newTieredStore(local, remote), "example.com/m")
	digest := digestForTest("4")
	if err := resultCache.Put(digest, entryForTest()); err != nil {
		t.Fatalf("put: %v", err)
	}
	assertCacheHit(t, newResultCache(local, "example.com/m"), digest)
	assertRemoteHit(t, remote, resultCache.objectKey(digest))
	requests.Reset()
	resultCache.Delete(digest)
	assertCacheMiss(t, newResultCache(local, "example.com/m"), digest)
	assertRemoteMiss(t, remote, resultCache.objectKey(digest))
	if requests.Count(http.MethodDelete) != 1 {
		t.Fatalf("remote deletes=%d, want 1", requests.Count(http.MethodDelete))
	}
}

// TestTieredStoreRemoteStats protects successful remote traffic accounting.
func TestTieredStoreRemoteStats(t *testing.T) {
	local, remote, _ := newTieredCacheForTest(t)
	store := newTieredStore(local, remote)
	resultCache := newResultCache(store, "example.com/m")
	remoteDigest := digestForTest("8")
	key := objectKeyForParts(moduleKeyForPath("example.com/m"), remoteDigest)
	if err := remote.Put(key, entryJSONForTest(t)); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	assertCacheHit(t, resultCache, remoteDigest)
	assertCacheMiss(t, resultCache, digestForTest("9"))
	if err := resultCache.Put(digestForTest("a"), entryForTest()); err != nil {
		t.Fatalf("put: %v", err)
	}
	assertRemoteStats(t, store, 1, 1)
}

// TestTieredStoreRemoteStatsIgnoreFailedPuts protects success-only write counts.
func TestTieredStoreRemoteStatsIgnoreFailedPuts(t *testing.T) {
	local, err := newDiskStore(t.TempDir())
	if err != nil {
		t.Fatalf("local store: %v", err)
	}
	store := newTieredStore(local, &failingRemoteStore{putErr: errors.New("refused")})
	resultCache := newResultCache(store, "example.com/m")
	if err := resultCache.Put(digestForTest("b"), entryForTest()); err != nil {
		t.Fatalf("put: %v", err)
	}
	assertRemoteStats(t, store, 0, 0)
}

// TestTieredStoreRemoteStatsConcurrentPuts protects atomic write counting.
func TestTieredStoreRemoteStatsConcurrentPuts(t *testing.T) {
	local, remote, _ := newTieredCacheForTest(t)
	store := newTieredStore(local, remote)
	resultCache := newResultCache(store, "example.com/m")
	var wg sync.WaitGroup
	const puts = 64
	for index := range puts {
		digest := fmt.Sprintf("%064x", index+32)
		wg.Go(func() {
			if err := resultCache.Put(digest, entryForTest()); err != nil {
				t.Errorf("put %s: %v", digest, err)
			}
		})
	}
	wg.Wait()
	assertRemoteStats(t, store, 0, puts)
}

// TestTieredStoreRemoteFailuresMatchDisk protects dead-remote parity.
func TestTieredStoreRemoteFailuresMatchDisk(t *testing.T) {
	for name, remote := range map[string]remoteStore{
		"get": &failingRemoteStore{getErr: errors.New("refused")},
		"put": &failingRemoteStore{putErr: errors.New("refused")},
	} {
		t.Run(name, func(t *testing.T) {
			diskOnly := openCacheAt(t, t.TempDir(), "example.com/m")
			tiered := openCacheAt(t, t.TempDir(), "example.com/m")
			runCacheMatrix(t, diskOnly, newResultCache(newTieredStore(tiered.store, remote), "example.com/m"))
		})
	}
}

func assertRemoteBackfill(t *testing.T, local *diskStore, remote *memoryRemoteStore, requests *requestCounts) {
	t.Helper()
	remoteDigest := digestForTest("2")
	key := objectKeyForParts(moduleKeyForPath("example.com/m"), remoteDigest)
	if err := remote.Put(key, entryJSONForTest(t)); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	requests.Reset()
	resultCache := newResultCache(newTieredStore(local, remote), "example.com/m")
	assertCacheHit(t, resultCache, remoteDigest)
	assertCacheHit(t, newResultCache(local, "example.com/m"), remoteDigest)
	requests.Reset()
	assertCacheHit(t, resultCache, remoteDigest)
	if requests.Total() != 0 {
		t.Fatalf("backfilled hit made %d remote requests, want zero", requests.Total())
	}
}

func assertTieredMiss(t *testing.T, local *diskStore, remote *memoryRemoteStore, requests *requestCounts) {
	t.Helper()
	requests.Reset()
	resultCache := newResultCache(newTieredStore(local, remote), "example.com/m")
	assertCacheMiss(t, resultCache, digestForTest("3"))
	if requests.Count(http.MethodGet) != 1 {
		t.Fatalf("remote gets=%d, want 1", requests.Count(http.MethodGet))
	}
}

func runCacheMatrix(t *testing.T, diskOnly *Cache, tiered *Cache) {
	t.Helper()
	hitDigest := digestForTest("5")
	if err := diskOnly.Put(hitDigest, entryForTest()); err != nil {
		t.Fatalf("seed disk only: %v", err)
	}
	if err := tiered.store.Put(tiered.objectKey(hitDigest), entryJSONForTest(t)); err != nil {
		t.Fatalf("seed tiered disk: %v", err)
	}
	assertSameGet(t, diskOnly, tiered, hitDigest)
	assertSameGet(t, diskOnly, tiered, digestForTest("6"))
	assertSamePut(t, diskOnly, tiered, digestForTest("7"))
	assertSameDelete(t, diskOnly, tiered, hitDigest)
}

func assertSameGet(t *testing.T, left *Cache, right *Cache, digest string) {
	t.Helper()
	leftEntry, leftFound := left.Get(digest)
	rightEntry, rightFound := right.Get(digest)
	if leftFound != rightFound || leftEntry.Passed != rightEntry.Passed {
		t.Fatalf("Get parity found=(%v,%v) passed=(%d,%d)",
			leftFound, rightFound, leftEntry.Passed, rightEntry.Passed)
	}
}

func assertSamePut(t *testing.T, left *Cache, right *Cache, digest string) {
	t.Helper()
	leftErr := left.Put(digest, entryForTest())
	rightErr := right.Put(digest, entryForTest())
	if (leftErr == nil) != (rightErr == nil) {
		t.Fatalf("Put parity left=%v right=%v", leftErr, rightErr)
	}
	assertSameGet(t, left, right, digest)
}

func assertSameDelete(t *testing.T, left *Cache, right *Cache, digest string) {
	t.Helper()
	left.Delete(digest)
	right.Delete(digest)
	assertSameGet(t, left, right, digest)
}

func newTieredCacheForTest(t *testing.T) (*diskStore, *memoryRemoteStore, *requestCounts) {
	t.Helper()
	local, err := newDiskStore(t.TempDir())
	if err != nil {
		t.Fatalf("local store: %v", err)
	}
	requests := &requestCounts{}
	remote := &memoryRemoteStore{requests: requests}
	return local, remote, requests
}

func assertCacheHit(t *testing.T, resultCache *Cache, digest string) {
	t.Helper()
	if _, found := resultCache.Get(digest); !found {
		t.Fatalf("%s must hit", digest)
	}
}

func assertCacheMiss(t *testing.T, resultCache *Cache, digest string) {
	t.Helper()
	if _, found := resultCache.Get(digest); found {
		t.Fatalf("%s must miss", digest)
	}
}

func assertRemoteHit(t *testing.T, remote remoteStore, key string) {
	t.Helper()
	if _, found, err := remote.Get(key); err != nil || !found {
		t.Fatalf("%s remote hit = %v err %v, want hit", key, found, err)
	}
}

func assertRemoteMiss(t *testing.T, remote remoteStore, key string) {
	t.Helper()
	if _, found, err := remote.Get(key); err != nil || found {
		t.Fatalf("%s remote hit = %v err %v, want miss", key, found, err)
	}
}

func assertRemoteStats(t *testing.T, store *tieredStore, reads int, writes int) {
	t.Helper()
	stats := store.RemoteStats()
	if stats.Reads != reads || stats.Writes != writes {
		t.Fatalf("remote stats = %+v, want reads=%d writes=%d", stats, reads, writes)
	}
}

type requestCounts struct {
	mu      sync.Mutex
	methods map[string]int
}

func (c *requestCounts) Count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.methods[method]
}

func (c *requestCounts) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, count := range c.methods {
		total += count
	}
	return total
}

func (c *requestCounts) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.methods = map[string]int{}
}

// remoteConfigForTest creates a validated-boundary-shaped remote fixture.
func remoteConfigForTest(bucket string, prefix string, endpoint string) *appconfig.Remote {
	return &appconfig.Remote{
		Bucket: bucket, Prefix: prefix, Region: "auto", Endpoint: endpoint,
		AccessKeyID: "access", SecretAccessKey: "secret",
	}
}

type memoryRemoteStore struct {
	mu       sync.Mutex
	objects  map[string][]byte
	requests *requestCounts
}

func (s *memoryRemoteStore) Get(key string) ([]byte, bool, error) {
	s.requests.Add(http.MethodGet)
	s.mu.Lock()
	defer s.mu.Unlock()
	body, found := s.objects[key]
	return append([]byte(nil), body...), found, nil
}

func (s *memoryRemoteStore) Put(key string, body []byte) error {
	s.requests.Add(http.MethodPut)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = map[string][]byte{}
	}
	s.objects[key] = append([]byte(nil), body...)
	return nil
}

func (s *memoryRemoteStore) Delete(key string) error {
	s.requests.Add(http.MethodDelete)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

type failingRemoteStore struct {
	getErr error
	putErr error
}

func (s *failingRemoteStore) Get(string) ([]byte, bool, error) {
	return nil, false, s.getErr
}

func (s *failingRemoteStore) Put(string, []byte) error {
	return s.putErr
}

func (s *failingRemoteStore) Delete(string) error {
	return nil
}

func (c *requestCounts) Add(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.methods == nil {
		c.methods = map[string]int{}
	}
	c.methods[method]++
}
