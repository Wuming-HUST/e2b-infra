package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/jellydator/ttlcache/v3"
)

// cachedDiff owns exactly one Diff. Closing the cache's reference retires it;
// the final pin closes the underlying mapping. The mutex also makes eviction
// and acquiring a pin atomic even though ttlcache invokes callbacks async.
type cachedDiff struct {
	Diff
	original Diff
	mu       sync.Mutex
	pins     int
	retired  bool
	closed   bool
	closeErr error
}

func newCachedDiff(original, owned Diff) *cachedDiff {
	return &cachedDiff{Diff: owned, original: original}
}

func (d *cachedDiff) pin() (*PinnedDiff, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.retired {
		return nil, false
	}
	d.pins++
	return &PinnedDiff{entry: d}, true
}

func (d *cachedDiff) closeIfUnused() error {
	if d.retired && d.pins == 0 && !d.closed {
		d.closed = true
		d.closeErr = d.Diff.Close()
	}
	return d.closeErr
}

func (d *cachedDiff) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.retired = true
	return d.closeIfUnused()
}

// PinnedDiff is an owned reference independent of cache membership. Close is
// idempotent and drains this handle's in-flight reads before dropping its pin.
type PinnedDiff struct {
	entry    *cachedDiff
	mu       sync.RWMutex
	closed   bool
	closeErr error
}

func (d *PinnedDiff) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		d.entry.mu.Lock()
		d.entry.pins--
		d.closeErr = d.entry.closeIfUnused()
		d.entry.mu.Unlock()
	}
	return d.closeErr
}

func (d *PinnedDiff) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return 0, os.ErrClosed
	}
	return d.entry.Diff.ReadAt(ctx, p, off)
}

func (d *PinnedDiff) Slice(ctx context.Context, off, length int64) ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, os.ErrClosed
	}
	return d.entry.Diff.Slice(ctx, off, length)
}

// Pin acquires a cache entry without ever returning a borrowed Diff. Opening a
// remote object and allocating its sparse cache does not fetch its RAM payload.
// Initialization is serialized with publication so failed/partial objects are
// never observed by another acquisition.
func (s *DiffStore) Pin(ctx context.Context, candidate Diff) (*PinnedDiff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := candidate.CacheKey()
	s.resetDelete(key)
	if item := s.cache.Get(key); item != nil {
		entry, ok := item.Value().(*cachedDiff)
		if !ok {
			return nil, fmt.Errorf("unowned cache entry %s", key)
		}
		if pin, ok := entry.pin(); ok {
			return pin, nil
		}
		// An eviction callback may have retired this exact entry between Get
		// and pin. Never resurrect a retired mapping.
		s.cache.Delete(key)
	}
	if err := candidate.Init(ctx); err != nil {
		return nil, fmt.Errorf("failed to init source: %w", err)
	}
	owned, err := ownDiff(candidate)
	if err != nil {
		return nil, err
	}
	entry := newCachedDiff(candidate, owned)
	pin, _ := entry.pin()
	// An expired item is invisible to Get, but Set updates it in place and
	// does not run eviction callbacks. Delete it explicitly to retire its owner.
	s.cache.Delete(key)
	s.cache.Set(key, entry, ttlcache.DefaultTTL)
	return pin, nil
}

func ownDiff(d Diff) (Diff, error) {
	if local, ok := d.(*localDiff); ok {
		return local.transferToStore()
	}
	return d, nil
}

// localDiffStoreOwner bypasses the original snapshot handle's Close after its
// ownership has been transferred to the cache. This prevents Snapshot cleanup
// from unmapping RAM still used by a pinned source.
type localDiffStoreOwner struct{ *localDiff }

func (d *localDiffStoreOwner) Close() error { return d.localDiff.closeOwned() }

func (d *localDiff) transferToStore() (Diff, error) {
	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	if d.ownerClosed {
		return nil, os.ErrClosed
	}
	if d.storeOwned {
		return nil, errors.New("local diff ownership already transferred")
	}
	d.storeOwned = true
	return &localDiffStoreOwner{d}, nil
}

func (d *localDiff) closeOwned() error {
	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	return d.closeOwnedLocked()
}

func (d *localDiff) closeOwnedLocked() error {
	if !d.ownerClosed {
		d.ownerClosed = true
		d.ownerCloseErr = d.cache.Close()
	}
	return d.ownerCloseErr
}
