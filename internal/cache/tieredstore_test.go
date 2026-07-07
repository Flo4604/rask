package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appconfig "github.com/chronark/rask/internal/config"
)

const (
	// tieredCancelDelayForTest gives the remote lookup time to park on ctx.Done.
	tieredCancelDelayForTest = 50 * time.Millisecond

	// tieredCancelledGetMax proves tiered Get returns on context cancellation.
	tieredCancelledGetMax = 500 * time.Millisecond
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
	if err := newResultCache(local, "example.com/m").Put(context.Background(), localDigest, entryForTest()); err != nil {
		t.Fatalf("seed local: %v", err)
	}
	requests.Reset()
	if _, found := newResultCache(newTieredStore(local, remote), "example.com/m").Get(context.Background(), localDigest); !found {
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
	if err := resultCache.Put(context.Background(), digest, entryForTest()); err != nil {
		t.Fatalf("put: %v", err)
	}
	assertCacheHit(t, newResultCache(local, "example.com/m"), digest)
	assertRemoteHit(t, remote, resultCache.objectKey(digest))
	requests.Reset()
	resultCache.Delete(context.Background(), digest)
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
	if err := remote.Put(context.Background(), key, entryJSONForTest(t)); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	assertCacheHit(t, resultCache, remoteDigest)
	assertCacheMiss(t, resultCache, digestForTest("9"))
	if err := resultCache.Put(context.Background(), digestForTest("a"), entryForTest()); err != nil {
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
	if err := resultCache.Put(context.Background(), digestForTest("b"), entryForTest()); err != nil {
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
			if err := resultCache.Put(context.Background(), digest, entryForTest()); err != nil {
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

// TestTieredStoreRemoteGetHonorsContextCancellation protects interrupted misses.
func TestTieredStoreRemoteGetHonorsContextCancellation(t *testing.T) {
	local, err := newDiskStore(t.TempDir())
	if err != nil {
		t.Fatalf("local store: %v", err)
	}
	resultCache := newResultCache(newTieredStore(local, blockingRemoteStore{}), "example.com/m")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(tieredCancelDelayForTest, cancel)
	startedAt := time.Now()
	_, found := resultCache.Get(ctx, digestForTest("d"))
	elapsed := time.Since(startedAt)
	if found {
		t.Fatalf("cancelled remote Get must report a miss")
	}
	if elapsed > tieredCancelledGetMax {
		t.Fatalf("cancelled tiered Get took %s, want under %s", elapsed, tieredCancelledGetMax)
	}
}

// TestPurgeRemoteModuleDeletesOnlyShard protects module-scoped remote cleanup.
func TestPurgeRemoteModuleDeletesOnlyShard(t *testing.T) {
	fake, remote := startCacheS3Fake(t)
	left := openRemoteCacheForTest(t, "example.com/left", remote)
	right := openRemoteCacheForTest(t, "example.com/right", remote)
	leftDigest := digestForTest("e")
	rightDigest := digestForTest("f")
	if err := left.Put(context.Background(), leftDigest, entryForTest()); err != nil {
		t.Fatalf("seed left: %v", err)
	}
	if err := right.Put(context.Background(), rightDigest, entryForTest()); err != nil {
		t.Fatalf("seed right: %v", err)
	}
	count, err := left.PurgeRemoteModule(context.Background())
	if err != nil {
		t.Fatalf("purge left: %v", err)
	}
	if count != 1 {
		t.Fatalf("deleted=%d, want 1", count)
	}
	if fake.HasCacheKey(left.objectKey(leftDigest)) {
		t.Fatalf("left remote object survived purge")
	}
	if !fake.HasCacheKey(right.objectKey(rightDigest)) {
		t.Fatalf("right remote object was deleted")
	}
}

// TestPurgeRemoteDeleteFailureReportsCount protects explicit purge errors.
func TestPurgeRemoteDeleteFailureReportsCount(t *testing.T) {
	fake, remote := startCacheS3Fake(t)
	first := objectKeyForParts(moduleKeyForPath("example.com/a"), digestForTest("1"))
	second := objectKeyForParts(moduleKeyForPath("example.com/b"), digestForTest("2"))
	fake.SeedCacheKey(first, []byte(`{"ok":true}`))
	fake.SeedCacheKey(second, []byte(`{"ok":true}`))
	fake.FailDelete(second)
	count, err := PurgeRemoteAll(context.Background(), remote)
	if err == nil || !strings.Contains(err.Error(), "after deleting 1") {
		t.Fatalf("purge err=%v, want count context", err)
	}
	if count != 1 {
		t.Fatalf("deleted=%d, want 1", count)
	}
	if fake.HasCacheKey(first) {
		t.Fatalf("first object survived before failing delete")
	}
	if !fake.HasCacheKey(second) {
		t.Fatalf("failed delete removed second object")
	}
}

// TestPurgeRemoteModuleRequiresRemote protects the defensive no-remote error.
func TestPurgeRemoteModuleRequiresRemote(t *testing.T) {
	resultCache := openCacheAt(t, t.TempDir(), "example.com/m")
	if _, err := resultCache.PurgeRemoteModule(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "no remote cache configured") {
		t.Fatalf("err=%v, want no remote error", err)
	}
}

func assertRemoteBackfill(t *testing.T, local *diskStore, remote *memoryRemoteStore, requests *requestCounts) {
	t.Helper()
	remoteDigest := digestForTest("2")
	key := objectKeyForParts(moduleKeyForPath("example.com/m"), remoteDigest)
	if err := remote.Put(context.Background(), key, entryJSONForTest(t)); err != nil {
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
	if err := diskOnly.Put(context.Background(), hitDigest, entryForTest()); err != nil {
		t.Fatalf("seed disk only: %v", err)
	}
	if err := tiered.store.Put(context.Background(), tiered.objectKey(hitDigest), entryJSONForTest(t)); err != nil {
		t.Fatalf("seed tiered disk: %v", err)
	}
	assertSameGet(t, diskOnly, tiered, hitDigest)
	assertSameGet(t, diskOnly, tiered, digestForTest("6"))
	assertSamePut(t, diskOnly, tiered, digestForTest("7"))
	assertSameDelete(t, diskOnly, tiered, hitDigest)
}

func assertSameGet(t *testing.T, left *Cache, right *Cache, digest string) {
	t.Helper()
	leftEntry, leftFound := left.Get(context.Background(), digest)
	rightEntry, rightFound := right.Get(context.Background(), digest)
	if leftFound != rightFound || leftEntry.Passed != rightEntry.Passed {
		t.Fatalf("Get parity found=(%v,%v) passed=(%d,%d)",
			leftFound, rightFound, leftEntry.Passed, rightEntry.Passed)
	}
}

func assertSamePut(t *testing.T, left *Cache, right *Cache, digest string) {
	t.Helper()
	leftErr := left.Put(context.Background(), digest, entryForTest())
	rightErr := right.Put(context.Background(), digest, entryForTest())
	if (leftErr == nil) != (rightErr == nil) {
		t.Fatalf("Put parity left=%v right=%v", leftErr, rightErr)
	}
	assertSameGet(t, left, right, digest)
}

func assertSameDelete(t *testing.T, left *Cache, right *Cache, digest string) {
	t.Helper()
	left.Delete(context.Background(), digest)
	right.Delete(context.Background(), digest)
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

func openRemoteCacheForTest(t *testing.T, modulePath string, remote *appconfig.Remote) *Cache {
	t.Helper()
	resultCache, err := Open(Config{
		ModulePath: modulePath,
		DiskDir:    t.TempDir(),
		Remote:     remote,
	})
	if err != nil {
		t.Fatalf("open remote cache: %v", err)
	}
	return resultCache
}

func assertCacheHit(t *testing.T, resultCache *Cache, digest string) {
	t.Helper()
	if _, found := resultCache.Get(context.Background(), digest); !found {
		t.Fatalf("%s must hit", digest)
	}
}

func assertCacheMiss(t *testing.T, resultCache *Cache, digest string) {
	t.Helper()
	if _, found := resultCache.Get(context.Background(), digest); found {
		t.Fatalf("%s must miss", digest)
	}
}

func assertRemoteHit(t *testing.T, remote remoteStore, key string) {
	t.Helper()
	if _, found, err := remote.Get(context.Background(), key); err != nil || !found {
		t.Fatalf("%s remote hit = %v err %v, want hit", key, found, err)
	}
}

func assertRemoteMiss(t *testing.T, remote remoteStore, key string) {
	t.Helper()
	if _, found, err := remote.Get(context.Background(), key); err != nil || found {
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

func startCacheS3Fake(t *testing.T) (*cacheS3Fake, *appconfig.Remote) {
	t.Helper()
	fake := newCacheS3Fake("rask-cache", "prefix")
	server := &httptest.Server{
		Listener: cacheLoopbackListenerForTest(t),
		Config:   &http.Server{Handler: fake},
	}
	server.Start()
	t.Cleanup(server.Close)
	return fake, remoteConfigForTest(fake.bucket, fake.prefix, server.URL)
}

func cacheLoopbackListenerForTest(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("loopback bind unavailable in this sandbox: %v", err)
		}
		t.Fatalf("allocate loopback port: %v", err)
	}
	return listener
}

type cacheS3Fake struct {
	mu         sync.Mutex
	bucket     string
	prefix     string
	objects    map[string][]byte
	failDelete map[string]bool
}

func newCacheS3Fake(bucket string, prefix string) *cacheS3Fake {
	return &cacheS3Fake{
		bucket: bucket, prefix: prefix,
		objects: map[string][]byte{}, failDelete: map[string]bool{},
	}
}

func (f *cacheS3Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		f.ServeList(w, r)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodGet:
		f.ServeGet(w, key)
	case http.MethodPut:
		f.ServePut(w, r, key)
	case http.MethodDelete:
		f.ServeDelete(w, key)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *cacheS3Fake) SeedCacheKey(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[f.cacheObjectName(key)] = append([]byte(nil), body...)
}

func (f *cacheS3Fake) HasCacheKey(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, found := f.objects[f.cacheObjectName(key)]
	return found
}

func (f *cacheS3Fake) FailDelete(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failDelete[f.cacheObjectName(key)] = true
}

func (f *cacheS3Fake) ServeGet(w http.ResponseWriter, key string) {
	f.mu.Lock()
	body, found := f.objects[key]
	f.mu.Unlock()
	if !found {
		http.NotFound(w, nil)
		return
	}
	_, _ = w.Write(append([]byte(nil), body...))
}

func (f *cacheS3Fake) ServePut(w http.ResponseWriter, r *http.Request, key string) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.objects[key] = append([]byte(nil), body...)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *cacheS3Fake) ServeDelete(w http.ResponseWriter, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete[key] {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	delete(f.objects, key)
	w.WriteHeader(http.StatusNoContent)
}

func (f *cacheS3Fake) ServeList(w http.ResponseWriter, r *http.Request) {
	keys := f.ListKeys(r.URL.Query().Get("prefix"))
	fmt.Fprint(w, "<ListBucketResult><IsTruncated>false</IsTruncated>")
	for _, key := range keys {
		fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", key)
	}
	fmt.Fprint(w, "</ListBucketResult>")
}

func (f *cacheS3Fake) ListKeys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	bucketPrefix := f.bucket + "/" + prefix
	for key := range f.objects {
		if strings.HasPrefix(key, bucketPrefix) {
			keys = append(keys, strings.TrimPrefix(key, f.bucket+"/"))
		}
	}
	sort.Strings(keys)
	return keys
}

// cacheObjectName mirrors the client's flat remote layout: the two-character
// digest shard exists only in cache keys, never in stored remote object names.
func (f *cacheS3Fake) cacheObjectName(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) == 4 {
		parts = append(parts[:2], parts[3])
	}
	return f.bucket + "/" + f.prefix + "/" + strings.Join(parts, "/")
}

type memoryRemoteStore struct {
	mu       sync.Mutex
	objects  map[string][]byte
	requests *requestCounts
}

func (s *memoryRemoteStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.requests.Add(http.MethodGet)
	s.mu.Lock()
	defer s.mu.Unlock()
	body, found := s.objects[key]
	return append([]byte(nil), body...), found, nil
}

func (s *memoryRemoteStore) Put(_ context.Context, key string, body []byte) error {
	s.requests.Add(http.MethodPut)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = map[string][]byte{}
	}
	s.objects[key] = append([]byte(nil), body...)
	return nil
}

func (s *memoryRemoteStore) Delete(_ context.Context, key string) error {
	s.requests.Add(http.MethodDelete)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *memoryRemoteStore) List(_ context.Context, prefix string) ([]string, error) {
	s.requests.Add("LIST")
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

type failingRemoteStore struct {
	getErr    error
	putErr    error
	listErr   error
	deleteErr error
}

// blockingRemoteStore simulates a remote request that only ctx can interrupt.
type blockingRemoteStore struct{}

// Get waits on ctx so tieredStore must pass cancellation to remote reads.
func (s blockingRemoteStore) Get(ctx context.Context, _ string) ([]byte, bool, error) {
	<-ctx.Done()
	return nil, false, ctx.Err()
}

// Put is unused because this fake exists only for read cancellation.
func (s blockingRemoteStore) Put(context.Context, string, []byte) error {
	return nil
}

// Delete is unused because this fake exists only for read cancellation.
func (s blockingRemoteStore) Delete(context.Context, string) error {
	return nil
}

// List is unused because this fake exists only for read cancellation.
func (s blockingRemoteStore) List(context.Context, string) ([]string, error) {
	return nil, nil
}

func (s *failingRemoteStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, s.getErr
}

func (s *failingRemoteStore) Put(context.Context, string, []byte) error {
	return s.putErr
}

func (s *failingRemoteStore) Delete(context.Context, string) error {
	return s.deleteErr
}

func (s *failingRemoteStore) List(context.Context, string) ([]string, error) {
	return nil, s.listErr
}

func (c *requestCounts) Add(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.methods == nil {
		c.methods = map[string]int{}
	}
	c.methods[method]++
}
