package nbd_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd/testutils"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func TestStressDynamicNBDConcurrentReuse(t *testing.T) {
	if os.Getenv("E2B_NBD_KERNEL_TEST") != "1" {
		t.Skip("requires an isolated Linux NBD test host")
	}
	t.Setenv("E2B_UNRESTRICTED_STRESS", "true")
	// A zero client uses flag fallbacks without contacting a feature service.
	flags := &featureflags.Client{}
	pool, err := nbd.NewDevicePool()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	group, ctx := errgroup.WithContext(ctx)
	for worker := 0; worker < 4; worker++ {
		group.Go(func() error {
			for round := 0; round < 8; round++ {
				marker := byte(worker*8 + round + 1)
				if err := exerciseDynamicDevice(ctx, pool, flags, filepath.Join(dir, fmt.Sprint(marker)), marker); err != nil {
					return fmt.Errorf("worker %d round %d: %w", worker, round, err)
				}
			}
			return nil
		})
	}
	err = group.Wait()
	// Check ownership even after a worker fails; report the original failure too.
	require.NoError(t, errors.Join(err, pool.Close(context.Background())))
	t.Log("32 dynamic NBD lifecycles across 4 concurrent workers verified")
}

func exerciseDynamicDevice(ctx context.Context, pool *nbd.DevicePool, flags *featureflags.Client, cachePath string, marker byte) (err error) {
	const size = int64(1 << 20)
	base, err := testutils.NewZeroDevice(size, header.RootfsBlockSize)
	if err != nil {
		return err
	}
	cache, err := block.NewCache(size, header.RootfsBlockSize, cachePath, false)
	if err != nil {
		return err
	}
	overlay := block.NewOverlay(base, cache)
	defer func() { err = errors.Join(err, overlay.Close()) }()
	mount := nbd.NewDirectPathMount(overlay, pool, flags)
	idx, err := mount.Open(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = errors.Join(err, mount.Close(cleanupCtx))
	}()
	file, err := os.OpenFile(nbd.GetDevicePath(idx), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if _, err = file.WriteAt([]byte{marker}, 0); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	got := make([]byte, 1)
	if _, err = file.ReadAt(got, 0); err != nil {
		return err
	}
	if got[0] != marker {
		return fmt.Errorf("nbd%d data changed: got %d, want %d", idx, got[0], marker)
	}
	return nil
}
