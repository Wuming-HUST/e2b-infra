package rootfs

import (
	"context"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/pmem"
	"github.com/e2b-dev/infra/packages/shared/pkg/fc/models"
)

// PmemProvider exports the private upper ext4 at block level. It never walks
// guest paths, so overlay whiteouts, opaque directories, xattrs and ownership
// remain in the same NBD checkpoint contract as the legacy rootfs.
type PmemProvider struct {
	Provider
	layout  pmem.Layout
	binding models.PmemConfig
}

func NewPmemProvider(ctx context.Context, upper Provider, cache *pmem.Cache, layout pmem.Layout, source pmem.OpenSource) (*PmemProvider, error) {
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	path, err := cache.Materialize(ctx, layout.Lower, source)
	if err != nil {
		return nil, err
	}
	return &PmemProvider{Provider: upper, layout: layout, binding: layout.Binding(path)}, nil
}
func (p *PmemProvider) PmemBinding() models.PmemConfig { return p.binding }
func (p *PmemProvider) PmemLayout() pmem.Layout        { return p.layout }
func (p *PmemProvider) PmemDataOffset() uint64         { return p.layout.DataOffset }
