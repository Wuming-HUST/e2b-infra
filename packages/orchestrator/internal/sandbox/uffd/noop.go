package uffd

import (
	"context"
	"fmt"

	"github.com/bits-and-blooms/bitset"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/fc"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

type NoopMemory struct {
	size      int64
	blockSize int64

	exit *utils.ErrorOnce
}

var _ MemoryBackend = (*NoopMemory)(nil)

func NewNoopMemory(size, blockSize int64) *NoopMemory {
	return &NoopMemory{
		size:      size,
		blockSize: blockSize,
		exit:      utils.NewErrorOnce(),
	}
}

func (m *NoopMemory) Prefault(_ context.Context, _ int64, _ []byte) error {
	return nil
}

func (m *NoopMemory) DiffMetadata(ctx context.Context, _ *fc.Process) (*header.DiffMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.size < 0 || m.blockSize <= 0 || m.size%m.blockSize != 0 {
		return nil, fmt.Errorf("invalid logical RAM layout: size=%d block_size=%d", m.size, m.blockSize)
	}
	// File/MAP_PRIVATE RAM has no complete change tracker. In particular, an
	// unfaulted nonzero parent page must never become an Empty override. Export
	// every logical block, including zeros, from the same frozen checkpoint.
	// Residency (mincore/pagemap) cannot determine content or safe inheritance.
	numberOfPages := header.TotalBlocks(m.size, m.blockSize)
	dirty := bitset.New(uint(numberOfPages))
	dirty.FlipRange(0, uint(numberOfPages))

	return &header.DiffMetadata{
		Dirty:     dirty,
		Empty:     bitset.New(0),
		BlockSize: m.blockSize,
	}, nil
}

func (m *NoopMemory) PrefetchData(_ context.Context) (block.PrefetchData, error) {
	// NoopMemory doesn't track block accesses, so return empty data
	return block.PrefetchData{
		BlockEntries: nil,
		BlockSize:    m.blockSize,
	}, nil
}

func (m *NoopMemory) Start(context.Context, string) error {
	return nil
}

func (m *NoopMemory) Stop() error {
	m.exit.SetSuccess()

	// This should be idempotent, so no need to return an error if it's already set.
	return nil
}

func (m *NoopMemory) Ready() chan struct{} {
	ch := make(chan struct{})
	close(ch)

	return ch
}

func (m *NoopMemory) Exit() *utils.ErrorOnce {
	return m.exit
}
