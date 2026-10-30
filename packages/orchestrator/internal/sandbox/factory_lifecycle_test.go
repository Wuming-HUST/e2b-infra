package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func lifecycleTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestFactoryLifecycleDrainCancelsAndWaitsForStartupCleanup(t *testing.T) {
	ctx := lifecycleTestContext(t)
	var l factoryLifecycle
	startupCtx, startup, err := l.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDone := make(chan struct{})
	shutdown := make(chan error, 1)
	go func() { shutdown <- l.drain(ctx) }()
	select {
	case <-startupCtx.Done():
		if !errors.Is(context.Cause(startupCtx), errFactoryDraining) {
			t.Fatal(context.Cause(startupCtx))
		}
	case <-ctx.Done():
		t.Fatal("startup was not canceled")
	}
	if _, _, err := l.begin(ctx); !errors.Is(err, errFactoryDraining) {
		t.Fatal("startup gate remained open", err)
	}
	select {
	case err := <-shutdown:
		t.Fatal("drain returned before startup cleanup", err)
	default:
	}
	go func() { <-cleanupDone; startup.finish(nil, nil) }()
	close(cleanupDone)
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("drain did not finish")
	}
}

func TestFactoryLifecycleLateStartupBecomesTrackedBeforeDrain(t *testing.T) {
	ctx := lifecycleTestContext(t)
	var l factoryLifecycle
	_, startup, err := l.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l.beginShutdown()
	closeStarted, processExited := make(chan struct{}), make(chan struct{})
	var closes atomic.Int32
	startup.finish(func(closeCtx context.Context) error {
		if closeCtx.Err() != nil {
			t.Error("canceled ForceStop context reached process cleanup")
		}
		closes.Add(1)
		close(closeStarted)
		<-processExited
		return nil
	}, nil)
	forceStop, cancel := context.WithCancel(ctx)
	cancel()
	drained := make(chan error, 2)
	go func() { drained <- l.drain(forceStop) }()
	<-closeStarted
	go func() { drained <- l.drain(ctx) }()
	select {
	case err := <-drained:
		t.Fatal("drain returned while process still mapped backing", err)
	default:
	}
	close(processExited)
	for range 2 {
		select {
		case err := <-drained:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("drain did not complete")
		}
	}
	if closes.Load() != 1 {
		t.Fatalf("process close calls: %d", closes.Load())
	}
}

func TestFactoryLifecycleNormalCloseRemovesOwnerAndErrorsDoNotSkipOthers(t *testing.T) {
	ctx := lifecycleTestContext(t)
	var l factoryLifecycle
	var alreadyClosed, closeCalls atomic.Int32
	_, normal, err := l.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var untrack func(error)
	normal.finish(func(context.Context) error { alreadyClosed.Add(1); return nil }, func(remove func(error)) { untrack = remove })
	untrack(nil)
	untrack(nil)
	boom := errors.New("cleanup failed after process exit")
	for i := 0; i < 4; i++ {
		_, startup, err := l.begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		startup.finish(func(context.Context) error { closeCalls.Add(1); return boom }, nil)
	}
	if err := l.drain(ctx); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := l.drain(ctx); !errors.Is(err, boom) {
		t.Fatal("repeated drain lost cleanup error", err)
	}
	if alreadyClosed.Load() != 0 || closeCalls.Load() != 4 {
		t.Fatalf("closed previously removed %d or skipped users %d", alreadyClosed.Load(), closeCalls.Load())
	}
}

func TestFactoryLifecycleConcurrentPublicationAndDrain(t *testing.T) {
	ctx := lifecycleTestContext(t)
	var l factoryLifecycle
	const count = 64
	startups := make([]*factoryStartup, count)
	for i := range startups {
		_, s, err := l.begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		startups[i] = s
	}
	gate := make(chan struct{})
	var wg sync.WaitGroup
	var closes atomic.Int32
	for _, startup := range startups {
		wg.Go(func() {
			<-gate
			startup.finish(func(context.Context) error { closes.Add(1); return nil }, nil)
			startup.finish(nil, nil)
		})
	}
	done := make(chan error, 1)
	go func() { <-gate; done <- l.drain(ctx) }()
	close(gate)
	wg.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("shutdown lost startup publication")
	}
	if closes.Load() != count {
		t.Fatalf("closed %d of %d published processes", closes.Load(), count)
	}
}

func TestFactoryLifecyclePreservesHistoricalCloseFailure(t *testing.T) {
	ctx := lifecycleTestContext(t)
	var l factoryLifecycle
	_, startup, err := l.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var untrack func(error)
	startup.finish(func(context.Context) error { t.Error("closed historical owner twice"); return nil }, func(remove func(error)) { untrack = remove })
	boom := errors.New("old sandbox NBD disconnect unconfirmed")
	untrack(boom)
	untrack(errors.New("duplicate must not replace original receipt"))
	if err := l.drain(ctx); !errors.Is(err, boom) {
		t.Fatal("historical teardown error lost", err)
	}
	if err := l.drain(ctx); !errors.Is(err, boom) {
		t.Fatal("repeated shutdown lost historical error", err)
	}
}

func TestFactoryLifecyclePreservesStartupCleanupFailure(t *testing.T) {
	ctx := lifecycleTestContext(t)
	var l factoryLifecycle
	_, startup, err := l.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("startup failed and rootfs disconnect unconfirmed")
	startup.finish(nil, nil, boom)
	if err := l.drain(ctx); !errors.Is(err, boom) {
		t.Fatal("startup teardown failure was forgotten", err)
	}
}

func TestSnapshotPathsRejectExistingArtifactsAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "state")
	if err := os.WriteFile(existing, []byte("keep snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectExistingSnapshotPaths(filepath.Join(dir, "new"), existing); err == nil {
		t.Fatal("accepted pre-existing snapshot")
	}
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "missing"), link); err != nil {
		t.Fatal(err)
	}
	if err := rejectExistingSnapshotPaths(link); err == nil {
		t.Fatal("accepted existing symlink")
	}
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "keep snapshot" {
		t.Fatal("modified prior snapshot", err)
	}
	if err := rejectExistingSnapshotPaths(filepath.Join(dir, "new")); err != nil {
		t.Fatal(err)
	}
}
