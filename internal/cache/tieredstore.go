package cache

import (
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
	Get(key string) ([]byte, bool, error)
	Put(key string, body []byte) error
	Delete(key string) error
}

// newTieredStore returns a disk-near, remote-far object store.
func newTieredStore(disk objectStore, remote remoteStore) *tieredStore {
	return &tieredStore{disk: disk, remote: remote}
}

// Get checks disk first, then remote; remote hits are staged for backfill once
// Cache validates the body and calls Touch.
func (s *tieredStore) Get(key string) ([]byte, bool, error) {
	body, found, _ := s.disk.Get(key)
	if found {
		return body, true, nil
	}
	body, found, err := s.remote.Get(key)
	if err != nil || !found {
		return nil, false, nil
	}
	s.pending.Store(key, append([]byte(nil), body...))
	return body, true, nil
}

// Put writes and verifies disk through Cache, then best-effort writes remote.
func (s *tieredStore) Put(key string, body []byte) error {
	if err := s.disk.Put(key, body); err != nil {
		return err
	}
	if err := s.remote.Put(key, body); err == nil {
		s.remoteWrites.Add(1)
	}
	return nil
}

// Delete removes disk and asks the remote tier to evict without surfacing errors.
func (s *tieredStore) Delete(key string) error {
	err := s.disk.Delete(key)
	_ = s.remote.Delete(key)
	s.pending.Delete(key)
	return err
}

// Touch marks local hits as recently used and backfills validated remote hits.
func (s *tieredStore) Touch(key string) error {
	if body, ok := s.pending.LoadAndDelete(key); ok {
		if cached, ok := body.([]byte); ok {
			_ = s.disk.Put(key, cached)
		}
		s.remoteReads.Add(1)
		return nil
	}
	return s.disk.Touch(key)
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

// RemovePrefix cleans only local cache contents; bucket lifecycle owns remote retention.
func (s *tieredStore) RemovePrefix(prefix string) error {
	return s.disk.RemovePrefix(prefix)
}
