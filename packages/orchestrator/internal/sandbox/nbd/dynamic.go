//go:build linux

package nbd

import (
	"context"
	"fmt"

	"github.com/Merovius/nbd/nbdnl"
)

func (p *DevicePool) connect(ctx context.Context, mount *DirectPathMount, idx DeviceSlot, size uint64, flags nbdnl.ServerFlags, opts ...nbdnl.ConnectOption) (uint32, error) {
	if !p.dynamic {
		return nbdnl.Connect(idx, mount.socksClient, size, 0, flags, opts...)
	}
	// Serialize only the kernel ownership transaction, not I/O, readiness or
	// cleanup. The shared netlink transport already serializes its requests.
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return 0, ErrClosed
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	idx, err := nbdnl.Connect(nbdnl.IndexAny, mount.socksClient, size, 0, flags, opts...)
	if err != nil {
		return 0, err
	}
	p.dynamicMounts[idx] = mount
	acquired.Add(ctx, 1)
	return idx, nil
}

func (p *DevicePool) disconnect(mount *DirectPathMount, idx DeviceSlot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A delayed close must not disconnect another mount using a recycled ID.
	if p.dynamicMounts[idx] != mount {
		return nil
	}
	if err := nbdnl.Disconnect(idx); err != nil {
		return fmt.Errorf("disconnect dynamic NBD %d: %w", idx, err)
	}
	delete(p.dynamicMounts, idx)
	return nil
}
