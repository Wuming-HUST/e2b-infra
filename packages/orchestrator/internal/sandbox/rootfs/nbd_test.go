package rootfs

import (
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	nbdtestutils "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd/testutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// newUnstartedNBDProvider builds an NBDProvider whose Start was never called,
// mirroring the sandbox creation error paths that close the provider before
// the Start goroutine is launched (sandbox.go PMEM guard failures).
func newUnstartedNBDProvider(t *testing.T) *NBDProvider {
	t.Helper()

	rootfsDevice, err := nbdtestutils.NewZeroDevice(1<<20, 4096)
	if err != nil {
		t.Fatalf("failed to create zero device: %v", err)
	}

	cache, err := block.NewCache(1<<20, 4096, t.TempDir()+"/cache", false)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	overlay := block.NewOverlay(rootfsDevice, cache)
	mnt := nbd.NewDirectPathMount(overlay, nil, nil)

	return &NBDProvider{
		mnt:                mnt,
		overlay:            overlay,
		ready:              utils.NewSetOnce[string](),
		finishedOperations: make(chan struct{}, 1),
		blockSize:          4096,
	}
}

// TestNBDProviderCloseWithoutStartDoesNotDeadlock is the regression test for
// the Close deadlock: sync() used to block forever in SetOnce.Wait when the
// provider was never started, swallowing the creation error as a 15-minute
// hang (f02-compat first run).
func TestNBDProviderCloseWithoutStartDoesNotDeadlock(t *testing.T) {
	provider := newUnstartedNBDProvider(t)

	done := make(chan error, 1)
	go func() {
		done <- provider.Close(t.Context())
	}()

	select {
	case err := <-done:
		// Close must return promptly; a non-nil error from closing the
		// never-started provider's overlay is acceptable, a hang is not.
		if err != nil {
			t.Logf("Close returned error (acceptable, no deadlock): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close deadlocked on a never-started NBD provider (Bug 3 regression)")
	}
}

// TestNBDProviderSyncWithoutStartIsNoop pins the sync() skip semantics: no
// flush attempt (and no blocking Path() call) before Start is invoked.
func TestNBDProviderSyncWithoutStartIsNoop(t *testing.T) {
	provider := newUnstartedNBDProvider(t)

	done := make(chan error, 1)
	go func() {
		done <- provider.sync(t.Context())
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sync on never-started provider must be a no-op, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sync deadlocked on a never-started NBD provider (Bug 3 regression)")
	}
}
