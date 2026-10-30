package fc

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/bits-and-blooms/bitset"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/memory"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// MemoryInfo returns the memory info for the sandbox.
// The dirty field represents mincore resident pages—essentially pages that were faulted in.
// The empty field represents pages that are *resident*, but also completely empty.
func (p *Process) MemoryInfo(ctx context.Context, blockSize int64) (*header.DiffMetadata, error) {
	return p.client.memoryInfo(ctx, blockSize)
}

func (p *Process) DirtyMemory(ctx context.Context, blockSize int64) (*header.DiffMetadata, error) {
	return p.client.dirtyMemory(ctx, blockSize)
}

func (p *Process) ExportMemory(
	ctx context.Context,
	include *bitset.BitSet,
	cachePath string,
	blockSize int64,
) (*block.Cache, error) {
	m, err := p.client.memoryMapping(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get memory mappings: %w", err)
	}

	remoteRanges, err := exportMemoryRanges(m, include, blockSize)
	if err != nil {
		return nil, err
	}

	pid, err := p.Pid()
	if err != nil {
		return nil, fmt.Errorf("failed to get pid: %w", err)
	}

	cache, err := block.NewCacheFromProcessMemory(ctx, blockSize, cachePath, pid, remoteRanges)
	if err != nil {
		return nil, fmt.Errorf("failed to create cache: %w", err)
	}

	return cache, nil
}

// exportMemoryRanges uses the snapshot protocol's logical offsets, never GPA
// or host-address order. The output cache is the packed stream of selected
// blocks, in the same order used by DiffMetadata.ToDiffHeader.
func exportMemoryRanges(m *memory.Mapping, include *bitset.BitSet, blockSize int64) ([]block.Range, error) {
	if m == nil || include == nil || blockSize <= 0 {
		return nil, fmt.Errorf("invalid export mapping, include bitmap or block size")
	}
	regions := slices.Clone(m.Regions)
	slices.SortFunc(regions, func(a, b memory.Region) int {
		if a.Offset < b.Offset {
			return -1
		}
		if a.Offset > b.Offset {
			return 1
		}
		return 0
	})
	var logicalSize int64
	for _, r := range regions {
		if uint64(r.Offset) > math.MaxInt64 || uint64(r.Size) > math.MaxInt64 || r.Size == 0 ||
			int64(r.Offset) != logicalSize || int64(r.Size) > math.MaxInt64-logicalSize ||
			uint64(r.BaseHostVirtAddr) > math.MaxInt64 || uint64(r.Size) > math.MaxInt64-uint64(r.BaseHostVirtAddr) {
			return nil, fmt.Errorf("invalid or noncontiguous logical memory region %+v", r)
		}
		logicalSize += int64(r.Size)
	}
	if logicalSize == 0 || logicalSize%blockSize != 0 {
		return nil, fmt.Errorf("logical RAM size %d is not a positive multiple of block size %d", logicalSize, blockSize)
	}
	if _, found := include.NextSet(uint(logicalSize / blockSize)); found {
		return nil, fmt.Errorf("include bitmap exceeds logical RAM size %d", logicalSize)
	}
	var ranges []block.Range
	for r := range block.BitsetRanges(include, blockSize) {
		hostRanges, err := m.GetHostVirtRanges(r.Start, r.Size)
		if err != nil {
			return nil, fmt.Errorf("failed to get host virt ranges: %w", err)
		}
		ranges = append(ranges, hostRanges...)
	}
	return ranges, nil
}
