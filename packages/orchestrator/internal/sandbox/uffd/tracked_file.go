package uffd

import (
	"context"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/fc"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// TrackedFileMemory has no missing-page pager: File/MAP_PRIVATE still faults
// through its backing. Only snapshot candidate selection differs from Noop.
type TrackedFileMemory struct{ *NoopMemory }

func NewTrackedFileMemory(size, blockSize int64) *TrackedFileMemory {
	return &TrackedFileMemory{NewNoopMemory(size, blockSize)}
}

func (*TrackedFileMemory) RAMExportMode() string { return "tracked_delta" }

func (m *TrackedFileMemory) DiffMetadata(ctx context.Context, p *fc.Process) (*header.DiffMetadata, error) {
	return p.TrackedDirtyMemory(ctx, m.size, m.blockSize)
}
