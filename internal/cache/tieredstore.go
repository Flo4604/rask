package cache

import (
	"context"
	"sync"
	"sync/atomic"
)

// tieredStore keeps disk as the authoritative local tier and remote as a
// best-effort shared tier. Remote failures are invisible to callers so adding a
// remote can only contribute hits, never new failure modes.
type tieredStore struct {
	disk         objectStore
	remote       remoteStore
	pending      sync.Map
	remoteReads  atomic.Uint64
	remoteWrites atomic.Uint64
}

type remoteStore interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Put(ctx context.Context, key string, body []byte) error
	Delete(ctx context.Context, key string) error

	// List exposes remote keys only for explicit purge commands.
	List(ctx context.Context, prefix string) ([]string, error)
}

// newTieredStore returns a disk-near, remote-far object store.
func newTieredStore(disk objectStore, remote remoteStore) *tieredStore {
	return &tieredStore{disk: disk, remote: remote}
}

// Get checks disk first, then remote; remote hits are staged for backfill once
// Cache validates the body and calls Touch.
func (s *tieredStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	body, found, _ := s.disk.Get(ctx, key)
	if found {
		return body, true, nil
	}
	body, found, err := s.remote.Get(ctx, key)
	if err != nil || !found {
		return nil, false, nil
	}
	s.pending.Store(key, append([]byte(nil), body...))
	return body, true, nil
}

// Put writes and verifies disk through Cache, then best-effort writes remote.
func (s *tieredStore) Put(ctx context.Context, key string, body []byte) error {
	if err := s.disk.Put(ctx, key, body); err != nil {
		return err
	}
	if err := s.remote.Put(ctx, key, body); err == nil {
		s.remoteWrites.Add(1)
	}
	return nil
}

// Delete removes disk and asks the remote tier to evict without surfacing errors.
func (s *tieredStore) Delete(ctx context.Context, key string) error {
	err := s.disk.Delete(ctx, key)
	_ = s.remote.Delete(ctx, key)
	s.pending.Delete(key)
	return err
}

// Touch marks local hits as recently used and backfills validated remote hits.
func (s *tieredStore) Touch(ctx context.Context, key string) error {
	if body, ok := s.pending.LoadAndDelete(key); ok {
		if cached, ok := body.([]byte); ok {
			_ = s.disk.Put(ctx, key, cached)
		}
		s.remoteReads.Add(1)
		return nil
	}
	return s.disk.Touch(ctx, key)
}

// RemoteStats returns successful remote read and write counters.
func (s *tieredStore) RemoteStats() RemoteStats {
	return RemoteStats{
		Reads:  int(s.remoteReads.Load()),
		Writes: int(s.remoteWrites.Load()),
	}
}

// List only reports local objects because pruning is a disk concern.
func (s *tieredStore) List(prefix string) ([]storedObject, error) {
	return s.disk.List(prefix)
}

// RemovePrefix cleans only local cache contents; explicit purge owns remote removal.
func (s *tieredStore) RemovePrefix(prefix string) error {
	return s.disk.RemovePrefix(prefix)
}
