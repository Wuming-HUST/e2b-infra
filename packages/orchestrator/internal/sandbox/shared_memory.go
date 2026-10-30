package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/memorybacking"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// sourceDevice is borrowed by NBD; the registry alone owns source.Close.
// NBD uses ReadAt/Size. Slice exists for the common ReadonlyDevice contract.
type sourceDevice struct{ memorybacking.SnapshotSource }

func (d sourceDevice) Size(context.Context) (int64, error) { return d.Identity().LogicalSize, nil }
func (d sourceDevice) BlockSize() int64                    { return 4096 }
func (d sourceDevice) Header() *header.Header              { return nil }
func (d sourceDevice) Close() error                        { return nil }
func (d sourceDevice) Slice(ctx context.Context, off, length int64) ([]byte, error) {
	if length < 0 || length > 32<<20 {
		return nil, errors.New("invalid shared memory slice length")
	}
	b := make([]byte, int(length))
	_, err := d.ReadAt(ctx, b, off)
	return b, err
}

type nbdMemoryBacking struct {
	mount *nbd.DirectPathMount
	path  string
	ready chan struct{}
}

func (b *nbdMemoryBacking) Path() string                    { return b.path }
func (b *nbdMemoryBacking) Ready() <-chan struct{}          { return b.ready }
func (b *nbdMemoryBacking) Done() <-chan struct{}           { return b.mount.Done() }
func (b *nbdMemoryBacking) Err() error                      { return b.mount.Err() }
func (b *nbdMemoryBacking) Close(ctx context.Context) error { return b.mount.Close(ctx) }

func newSharedNBDFactory(pool *nbd.DevicePool, flags *featureflags.Client) memorybacking.Factory {
	return func(ctx context.Context, source memorybacking.SnapshotSource) (memorybacking.Backing, error) {
		mount := nbd.NewReadonlyDirectPathMount(sourceDevice{source}, pool, flags)
		ready := make(chan struct{})
		backing := &nbdMemoryBacking{mount: mount, ready: ready}
		index, err := mount.Open(ctx)
		if err != nil {
			// Even a failed Open may own a connection whose disconnect failed.
			// Return ownership so the registry can drain or quarantine it.
			return backing, fmt.Errorf("open shared readonly NBD: %w", err)
		}
		backing.path = nbd.GetDevicePath(index)
		close(ready)
		return backing, nil
	}
}

// The VM/exporter keep the lease until FC has exited, including startup errors.
// Failure is observed through Done/Err, so every holder sees the same failure.
type leasedMemory struct {
	uffd.MemoryBackend
	lease *memorybacking.Lease
	exit  *utils.ErrorOnce
	stop  chan struct{}
	once  sync.Once
}

func newLeasedMemory(backend uffd.MemoryBackend, lease *memorybacking.Lease) *leasedMemory {
	return &leasedMemory{MemoryBackend: backend, lease: lease, exit: utils.NewErrorOnce(), stop: make(chan struct{})}
}
func (m *leasedMemory) Ready() chan struct{}   { return m.MemoryBackend.Ready() }
func (m *leasedMemory) Exit() *utils.ErrorOnce { return m.exit }
func (m *leasedMemory) Start(ctx context.Context, path string) error {
	if err := m.MemoryBackend.Start(ctx, path); err != nil {
		return err
	}
	go func() {
		select {
		case <-m.lease.Done():
			err := m.lease.Err()
			if err == nil {
				err = errors.New("shared memory backing stopped while VM was active")
			}
			m.exit.SetError(err)
		case <-m.stop:
		}
	}()
	return nil
}
func (m *leasedMemory) Stop() error {
	m.once.Do(func() { close(m.stop) })
	err := errors.Join(m.lease.Err(), m.MemoryBackend.Stop())
	m.exit.SetError(err)
	return err
}
