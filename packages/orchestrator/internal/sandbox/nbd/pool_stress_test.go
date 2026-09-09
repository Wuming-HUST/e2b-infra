package nbd

import (
	"context"
	"testing"

	"github.com/Merovius/nbd/nbdnl"
	"github.com/bits-and-blooms/bitset"
	"github.com/stretchr/testify/require"
)

func TestStressNBDDoesNotWaitForPreallocatedSlots(t *testing.T) {
	pool := &DevicePool{dynamic: true, done: make(chan struct{}), slots: make(chan DeviceSlot), usedSlots: bitset.New(1)}
	// Simulate the Populate goroutine exiting: GetDevice must not read a zero
	// device ID from the closed ready channel or wait for a fixed-size pool.
	close(pool.slots)
	for i := 0; i < 8192; i++ {
		idx, err := pool.GetDevice(t.Context())
		require.NoError(t, err)
		require.Equal(t, uint32(nbdnl.IndexAny), idx)
	}
	require.NoError(t, pool.ReleaseDevice(t.Context(), nbdnl.IndexAny))
	require.Zero(t, pool.usedSlots.Count(), "IndexAny must never become an owned device")
	oldMount, newMount := &DirectPathMount{}, &DirectPathMount{}
	pool.dynamicMounts = map[DeviceSlot]*DirectPathMount{8192: newMount}
	require.NoError(t, pool.disconnect(oldMount, 8192), "stale close must not touch a recycled kernel device")
	require.NoError(t, pool.ReleaseDevice(t.Context(), 8192))
	require.Same(t, newMount, pool.dynamicMounts[8192], "old release must not remove successor ownership")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := pool.GetDevice(ctx)
	require.ErrorIs(t, err, context.Canceled)
	close(pool.done)
	_, err = pool.GetDevice(t.Context())
	require.ErrorIs(t, err, ErrClosed)
}

func TestNBDClosedReadyChannelIsAnError(t *testing.T) {
	pool := &DevicePool{done: make(chan struct{}), slots: make(chan DeviceSlot)}
	close(pool.slots)
	_, err := pool.GetDevice(t.Context())
	require.ErrorIs(t, err, ErrClosed)
}
