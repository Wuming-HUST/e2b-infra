package build

import (
	"context"
	"fmt"
	"io"
	"math"

	"github.com/google/uuid"

	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type File struct {
	header      *header.Header
	store       *DiffStore
	fileType    DiffType
	persistence storage.StorageProvider
	metrics     blockmetrics.Metrics
}

func NewFile(
	header *header.Header,
	store *DiffStore,
	fileType DiffType,
	persistence storage.StorageProvider,
	metrics blockmetrics.Metrics,
) *File {
	return &File{
		header:      header,
		store:       store,
		fileType:    fileType,
		persistence: persistence,
		metrics:     metrics,
	}
}

func (b *File) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	return readLogicalAt(ctx, b.header, p, off, func(ctx context.Context, buildID uuid.UUID, dst []byte, mappedOffset int64) (int, error) {
		candidate, err := b.storageDiff(&buildID)
		if err != nil {
			return 0, fmt.Errorf("failed to get build: %w", err)
		}
		mappedBuild, err := b.store.Pin(ctx, candidate)
		if err != nil {
			return 0, err
		}
		defer mappedBuild.Close()
		return mappedBuild.ReadAt(ctx, dst, mappedOffset)
	})
}

// readLogicalAt translates arbitrary byte ranges through the block-aligned
// header. Explicit zero extents must clear reused caller buffers; missing or
// truncated backing data must never be accepted as a zero extent.
func readLogicalAt(ctx context.Context, h *header.Header, p []byte, off int64,
	read func(context.Context, uuid.UUID, []byte, int64) (int, error),
) (n int, err error) {
	if h == nil || h.Metadata == nil || h.Metadata.BlockSize == 0 || h.Metadata.BlockSize > math.MaxInt64 || h.Metadata.Size > math.MaxInt64 || off < 0 {
		return 0, fmt.Errorf("invalid logical read offset or header")
	}
	if len(p) == 0 {
		return 0, nil
	}
	size := int64(h.Metadata.Size)
	if off >= size {
		return 0, io.EOF
	}
	blockSize := int64(h.Metadata.BlockSize)
	limit := min(int64(len(p)), size-off)
	for int64(n) < limit {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		logicalOffset := off + int64(n)
		withinBlock := logicalOffset % blockSize
		mappedOffset, mappedLength, buildID, err := h.GetShiftedMapping(ctx, logicalOffset-withinBlock)
		if err != nil {
			return n, fmt.Errorf("failed to get mapping: %w", err)
		}
		if mappedOffset < 0 || mappedOffset > math.MaxInt64-withinBlock || mappedLength <= withinBlock {
			return n, fmt.Errorf("invalid logical mapping at offset %d", logicalOffset)
		}
		mappedOffset += withinBlock
		readLength := min(mappedLength-withinBlock, limit-int64(n))
		dst := p[n : int64(n)+readLength]
		if *buildID == uuid.Nil {
			clear(dst)
			n += len(dst)
			continue
		}
		for copied := 0; copied < len(dst); {
			if err := ctx.Err(); err != nil {
				return n, err
			}
			count, readErr := read(ctx, *buildID, dst[copied:], mappedOffset+int64(copied))
			if count < 0 || count > len(dst)-copied {
				return n, fmt.Errorf("invalid source read count %d", count)
			}
			copied += count
			n += count
			if readErr != nil {
				return n, fmt.Errorf("failed to read from source: %w", readErr)
			}
			if count == 0 {
				return n, fmt.Errorf("failed to read from source: %w", io.ErrNoProgress)
			}
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// The slice access must be in the predefined blocksize of the build.
func (b *File) Slice(ctx context.Context, off, _ int64) ([]byte, error) {
	mappedOffset, _, buildID, err := b.header.GetShiftedMapping(ctx, off)
	if err != nil {
		return nil, fmt.Errorf("failed to get mapping: %w", err)
	}

	// Pass an empty page sized to the build's block size when the build id is nil.
	if *buildID == uuid.Nil {
		switch b.header.Metadata.BlockSize {
		case header.HugepageSize:
			return header.EmptyHugePage, nil
		default:
			return header.EmptyBlock, nil
		}
	}

	build, err := b.getBuild(ctx, buildID)
	if err != nil {
		return nil, fmt.Errorf("failed to get build: %w", err)
	}

	return build.Slice(ctx, mappedOffset, int64(b.header.Metadata.BlockSize))
}

func (b *File) getBuild(ctx context.Context, buildID *uuid.UUID) (Diff, error) {
	storageDiff, err := b.storageDiff(buildID)
	if err != nil {
		return nil, err
	}
	source, err := b.store.Get(ctx, storageDiff)
	if err != nil {
		return nil, fmt.Errorf("failed to get build from store: %w", err)
	}
	return source, nil
}

func (b *File) storageDiff(buildID *uuid.UUID) (*StorageDiff, error) {
	storageDiff, err := newStorageDiff(
		b.store.cachePath,
		buildID.String(),
		b.fileType,
		int64(b.header.Metadata.BlockSize),
		b.metrics,
		b.persistence,
		b.store.flags,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage diff: %w", err)
	}

	return storageDiff, nil
}
