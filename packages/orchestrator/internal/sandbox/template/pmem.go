package template

import (
	"context"
	"fmt"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/pmem"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

type pmemSourceProvider interface {
	PmemLowerSource() (pmem.OpenSource, error)
}

func PmemLowerSource(t Template) (pmem.OpenSource, error) {
	if p, ok := t.(pmemSourceProvider); ok {
		return p.PmemLowerSource()
	}
	return nil, fmt.Errorf("template has no pmem lower source")
}
func (t *restoreMemoryTemplate) PmemLowerSource() (pmem.OpenSource, error) {
	return PmemLowerSource(t.Template)
}
func (t *MaskTemplate) PmemLowerSource() (pmem.OpenSource, error) { return PmemLowerSource(t.template) }
func (t *storageTemplate) PmemLowerSource() (pmem.OpenSource, error) {
	m, err := t.Metadata()
	if err != nil {
		return nil, err
	}
	if m.Rootfs == nil {
		return nil, fmt.Errorf("template is not pmem")
	}
	if err = m.Rootfs.Validate(); err != nil {
		return nil, err
	}
	image := m.Rootfs.Lower
	return func(ctx context.Context) (storage.SeekableReader, error) {
		return t.persistence.OpenSeekable(ctx, image.ObjectPath(), storage.RootFSObjectType)
	}, nil
}

type pmemTemplate struct {
	Template
	meta   metadata.Template
	source pmem.OpenSource
}

// NewPmemTemplate supplies a new cold-boot format, never reuses a legacy RAM
// snapshot. rootfs must be a fresh private ext4 containing upper/ and work/.
func NewPmemTemplate(t Template, meta metadata.Template, source pmem.OpenSource) (Template, error) {
	if meta.Rootfs == nil || source == nil {
		return nil, fmt.Errorf("pmem template needs a lower layout and source")
	}
	if err := meta.Rootfs.Validate(); err != nil {
		return nil, err
	}
	meta.Version = 3
	return &pmemTemplate{Template: t, meta: meta, source: source}, nil
}
func (t *pmemTemplate) Metadata() (metadata.Template, error)      { return t.meta, nil }
func (t *pmemTemplate) PmemLowerSource() (pmem.OpenSource, error) { return t.source, nil }
