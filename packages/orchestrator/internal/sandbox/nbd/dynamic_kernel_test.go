package nbd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd/testutils"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func TestStressDynamicNBDKernel(t *testing.T) {
	if os.Getenv("E2B_NBD_KERNEL_TEST") != "1" {
		t.Skip("requires an isolated Linux host with NBD and CAP_SYS_ADMIN/MKNOD")
	}
	t.Setenv("E2B_UNRESTRICTED_STRESS", "true")
	data, err := os.ReadFile("/sys/module/nbd/parameters/nbds_max")
	require.NoError(t, err)
	initial, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	if initial > 16 {
		t.Skip("use an isolated kernel with nbds_max <= 16 for the bounded growth test")
	}
	// A zero client uses flag fallbacks without contacting a feature service.
	flags := &featureflags.Client{}
	pool, err := nbd.NewDevicePool()
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, pool.Close(ctx))
	})
	var ids []uint32
	var owners []string
	var files []*os.File
	var overlays []*block.Overlay
	mounts := make([]*nbd.DirectPathMount, 0)
	t.Cleanup(func() {
		for _, f := range files {
			assert.NoError(t, f.Close())
		}
		for i, mount := range mounts {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			assert.NoError(t, mount.Close(ctx))
			cancel()
			assert.Eventually(t, func() bool {
				owner, err := os.ReadFile(fmt.Sprintf("/sys/block/nbd%d/pid", ids[i]))
				return os.IsNotExist(err) || (err == nil && string(owner) != owners[i])
			}, 2*time.Second, 10*time.Millisecond, "device %d must release this connection", ids[i])
		}
		for _, overlay := range overlays {
			assert.NoError(t, overlay.Close())
		}
	})
	// Keep every device open concurrently so reuse cannot fake pool growth.
	for i := 0; i < initial+2; i++ {
		const size = int64(1 << 20)
		base, err := testutils.NewZeroDevice(size, header.RootfsBlockSize)
		require.NoError(t, err)
		cache, err := block.NewCache(size, header.RootfsBlockSize, filepath.Join(t.TempDir(), "cow"), false)
		require.NoError(t, err)
		overlay := block.NewOverlay(base, cache)
		overlays = append(overlays, overlay)
		mount := nbd.NewDirectPathMount(overlay, pool, flags)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		idx, err := mount.Open(ctx)
		// Open's context is also the dispatch lifetime. Keep it until cleanup.
		t.Cleanup(cancel)
		require.NoError(t, err)
		require.NotContains(t, ids, idx)
		ids = append(ids, idx)
		owner, ownerErr := os.ReadFile(fmt.Sprintf("/sys/block/nbd%d/pid", idx))
		require.NoError(t, ownerErr)
		owners = append(owners, string(owner))
		mounts = append(mounts, mount)
		f, err := os.OpenFile(nbd.GetDevicePath(idx), os.O_RDWR, 0)
		require.NoError(t, err)
		files = append(files, f)
	}
	var beyondInitial bool
	for i, f := range files {
		beyondInitial = beyondInitial || int(ids[i]) >= initial
		payload := []byte{byte(i + 1), 0xA5, 0x5A}
		_, err := f.WriteAt(payload, 0)
		require.NoError(t, err)
		require.NoError(t, f.Sync())
	}
	for i, f := range files {
		got := make([]byte, 3)
		_, err := f.ReadAt(got, 0)
		require.NoError(t, err)
		require.Equal(t, []byte{byte(i + 1), 0xA5, 0x5A}, got)
	}
	require.True(t, beyondInitial)
	t.Logf("nbds_max=%d, simultaneously connected=%d, device IDs=%v; distinct data verified", initial, len(ids), ids)
}
