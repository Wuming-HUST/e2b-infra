package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// PinnedFile owns the manifest and one pin per unique nonzero backing Diff.
// Its lifetime is independent of both the template and the build cache. Pinning
// opens sparse backing objects but never reads/materializes their RAM payload.
type PinnedFile struct {
	header   *header.Header
	diffs    map[uuid.UUID]*PinnedDiff
	mu       sync.RWMutex
	closed   bool
	closeErr error
}

var _ block.ReadonlyDevice = (*PinnedFile)(nil)

func (b *File) Pin(ctx context.Context) (*PinnedFile, error) {
	if b.header == nil || b.header.Metadata == nil {
		return nil, fmt.Errorf("cannot pin a file without a manifest")
	}
	// Snapshot the manifest so a caller retaining the template cannot mutate the
	// view's translation or substitute an unpinned object after acquisition.
	metadata := *b.header.Metadata
	mappings := make([]*header.BuildMap, len(b.header.Mapping))
	for i, mapping := range b.header.Mapping {
		if mapping == nil {
			return nil, fmt.Errorf("nil manifest extent")
		}
		copy := *mapping
		mappings[i] = &copy
	}
	h, err := header.NewHeader(&metadata, mappings)
	if err != nil {
		return nil, err
	}
	view := &PinnedFile{header: h, diffs: make(map[uuid.UUID]*PinnedDiff)}
	for _, mapping := range h.Mapping {
		id := mapping.BuildId
		if id == uuid.Nil || view.diffs[id] != nil {
			continue
		}
		candidate, err := b.storageDiff(&id)
		var pin *PinnedDiff
		if err == nil {
			pin, err = b.store.Pin(ctx, candidate)
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("pin backing %s: %w", id, err), view.Close())
		}
		view.diffs[id] = pin
	}
	return view, nil
}

func (b *PinnedFile) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return 0, os.ErrClosed
	}
	return readLogicalAt(ctx, b.header, p, off, func(ctx context.Context, id uuid.UUID, p []byte, off int64) (int, error) {
		diff := b.diffs[id]
		if diff == nil {
			return 0, fmt.Errorf("manifest references unpinned backing %s", id)
		}
		return diff.ReadAt(ctx, p, off)
	})
}

// Slice returns a copy so its lifetime does not escape an in-flight read and
// race Close. NBD uses ReadAt, so this does not add payload copies to that path.
func (b *PinnedFile) Slice(ctx context.Context, off, length int64) ([]byte, error) {
	if length < 0 || length > int64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("invalid slice length")
	}
	p := make([]byte, int(length))
	n, err := b.ReadAt(ctx, p, off)
	return p[:n], err
}

func (b *PinnedFile) Size(context.Context) (int64, error) { return int64(b.header.Metadata.Size), nil }
func (b *PinnedFile) BlockSize() int64                    { return int64(b.header.Metadata.BlockSize) }
func (b *PinnedFile) Header() *header.Header              { return b.header }

func (b *PinnedFile) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		for _, diff := range b.diffs {
			b.closeErr = errors.Join(b.closeErr, diff.Close())
		}
	}
	return b.closeErr
}
