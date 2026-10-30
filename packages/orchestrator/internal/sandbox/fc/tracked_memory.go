package fc

import (
	"context"
	"fmt"

	"github.com/bits-and-blooms/bitset"
	"github.com/e2b-dev/infra/packages/shared/pkg/fc/client/operations"
	"github.com/e2b-dev/infra/packages/shared/pkg/fc/models"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// TrackedDirtyMemory accepts only the versioned, frozen KVM/VMM contract.
// In particular an older FC's empty bitmap must never mean safe inheritance.
func (p *Process) TrackedDirtyMemory(ctx context.Context, size, blockSize int64) (*header.DiffMetadata, error) {
	res, err := p.client.client.Operations.GetDirtyMemory(&operations.GetDirtyMemoryParams{Context: ctx})
	if err != nil {
		return nil, fmt.Errorf("get tracked RAM candidates: %w", err)
	}
	return validateTrackedMemory(res.Payload, size, blockSize)
}

func validateTrackedMemory(m *models.DirtyMemory, size, blockSize int64) (*header.DiffMetadata, error) {
	if m == nil || size <= 0 || blockSize != 4096 || size%blockSize != 0 {
		return nil, fmt.Errorf("invalid tracked RAM response or layout")
	}
	if m.TrackingVersion != 1 || m.TrackingMode != "bitmap" || !m.TrackingComplete || !m.CaptureReady ||
		m.LogicalSize != uint64(size) || m.PageSize != uint64(blockSize) {
		return nil, fmt.Errorf("FC does not provide a complete frozen tracked RAM contract: %+v", struct {
			Version         uint32
			Mode            string
			Complete, Ready bool
			Size, Page      uint64
		}{m.TrackingVersion, m.TrackingMode, m.TrackingComplete, m.CaptureReady, m.LogicalSize, m.PageSize})
	}
	pages := uint64(size / blockSize)
	if uint64(len(m.Bitmap)) != (pages+63)/64 {
		return nil, fmt.Errorf("tracked RAM bitmap length differs from logical layout")
	}
	if tail := pages % 64; tail != 0 && m.Bitmap[len(m.Bitmap)-1]>>tail != 0 {
		return nil, fmt.Errorf("tracked RAM bitmap has out-of-range bits")
	}
	return &header.DiffMetadata{Dirty: bitset.From(m.Bitmap), Empty: bitset.New(0), BlockSize: blockSize}, nil
}
