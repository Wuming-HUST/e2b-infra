package memorybacking

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testIdentity = SnapshotIdentity{Kind: "ram", ContentID: "sha256:parent", LayoutID: "one-region-4k", LogicalSize: 8192}

type testSource struct {
	id     SnapshotIdentity
	closes atomic.Int32
}

func (s *testSource) Identity() SnapshotIdentity { return s.id }
func (s *testSource) ReadAt(_ context.Context, p []byte, _ int64) (int, error) {
	for i := range p {
		p[i] = 9
	}
	return len(p), nil
}
func (s *testSource) Close() error { s.closes.Add(1); return nil }

type testBacking struct {
	ready, done             chan struct{}
	err                     error
	closeErr                error
	closes                  atomic.Int32
	closeStarted, closeGate chan struct{}
}

func newTestBacking() *testBacking {
	b := &testBacking{ready: make(chan struct{}), done: make(chan struct{})}
	close(b.ready)
	return b
}
func (*testBacking) Path() string             { return "/dev/test-owned-nbd" }
func (b *testBacking) Ready() <-chan struct{} { return b.ready }
func (b *testBacking) Done() <-chan struct{}  { return b.done }
func (b *testBacking) Err() error             { return b.err }
func (b *testBacking) Close(context.Context) error {
	b.closes.Add(1)
	if b.closeStarted != nil {
		close(b.closeStarted)
		<-b.closeGate
	}
	return b.closeErr
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitRefs(t *testing.T, r *Registry, id SnapshotIdentity, n int) {
	t.Helper()
	ctx := testContext(t)
	for {
		r.mu.Lock()
		e := r.entries[id]
		refs := 0
		if e != nil {
			refs = e.refs
		}
		r.mu.Unlock()
		if refs == n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %d references; got %d", n, refs)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestRegistryConcurrentAcquireCancellationIsolationAndFinalClose(t *testing.T) {
	ctx := testContext(t)
	gate := make(chan struct{})
	s := &testSource{id: testIdentity}
	b := newTestBacking()
	var creates atomic.Int32
	r := NewRegistry(func(context.Context, SnapshotIdentity) (SnapshotSource, error) { return s, nil }, func(c context.Context, _ SnapshotSource) (Backing, error) {
		creates.Add(1)
		select {
		case <-gate:
			return b, nil
		case <-c.Done():
			return nil, c.Err()
		}
	})
	cancelCtx, cancel := context.WithCancel(ctx)
	first := make(chan error, 1)
	go func() { _, err := r.Acquire(cancelCtx, testIdentity); first <- err }()
	waitRefs(t, r, testIdentity, 1)
	const holders = 24
	leases := make(chan *Lease, holders)
	errs := make(chan error, holders)
	for range holders {
		go func() {
			l, err := r.Acquire(ctx, testIdentity)
			if err != nil {
				errs <- err
			} else {
				leases <- l
			}
		}()
	}
	waitRefs(t, r, testIdentity, holders+1)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter: %v", err)
	}
	close(gate)
	var all []*Lease
	for range holders {
		select {
		case l := <-leases:
			all = append(all, l)
		case err := <-errs:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if creates.Load() != 1 {
		t.Fatalf("created %d devices", creates.Load())
	}
	for _, l := range all[:holders-1] {
		if err := l.Release(ctx); err != nil {
			t.Fatal(err)
		}
		if err := l.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if s.closes.Load() != 0 || b.closes.Load() != 0 {
		t.Fatal("device closed with an active holder")
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if err := all[holders-1].Release(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if s.closes.Load() != 1 || b.closes.Load() != 1 {
		t.Fatalf("close counts source=%d device=%d", s.closes.Load(), b.closes.Load())
	}
	if err := r.Drain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryAcquireWaitsForDisconnectBeforeRecreate(t *testing.T) {
	ctx := testContext(t)
	b := newTestBacking()
	b.closeStarted = make(chan struct{})
	b.closeGate = make(chan struct{})
	var creates atomic.Int32
	r := NewRegistry(func(context.Context, SnapshotIdentity) (SnapshotSource, error) {
		return &testSource{id: testIdentity}, nil
	}, func(context.Context, SnapshotSource) (Backing, error) {
		if creates.Add(1) == 1 {
			return b, nil
		}
		return newTestBacking(), nil
	})
	l, err := r.Acquire(ctx, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() { released <- l.Release(ctx) }()
	<-b.closeStarted
	newLease := make(chan *Lease, 1)
	go func() {
		l, err := r.Acquire(ctx, testIdentity)
		if err != nil {
			t.Error(err)
		}
		newLease <- l
	}()
	// The old device is held in Close; no new factory may run before disconnect.
	if creates.Load() != 1 {
		t.Fatal("recreated before disconnect")
	}
	close(b.closeGate)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	l = <-newLease
	if l == nil {
		t.Fatal("nil lease")
	}
	if creates.Load() != 2 {
		t.Fatal("did not recreate")
	}
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryKeepsRuntimeContextUntilDisconnectConfirmed(t *testing.T) {
	for _, closeErr := range []error{nil, errors.New("disconnect unconfirmed")} {
		t.Run(fmt.Sprint(closeErr), func(t *testing.T) {
			ctx := testContext(t)
			b := newTestBacking()
			b.closeErr = closeErr
			b.closeStarted, b.closeGate = make(chan struct{}), make(chan struct{})
			var runtimeCtx context.Context
			r := NewRegistry(func(context.Context, SnapshotIdentity) (SnapshotSource, error) {
				return &testSource{id: testIdentity}, nil
			}, func(c context.Context, _ SnapshotSource) (Backing, error) {
				runtimeCtx = c
				return b, nil
			})
			lease, err := r.Acquire(ctx, testIdentity)
			if err != nil {
				t.Fatal(err)
			}
			released := make(chan error, 1)
			go func() { released <- lease.Release(ctx) }()
			<-b.closeStarted
			if err := runtimeCtx.Err(); err != nil {
				t.Errorf("transport canceled before disconnect: %v", err)
			}
			close(b.closeGate)
			if err := <-released; !errors.Is(err, closeErr) {
				t.Fatalf("release: %v", err)
			}
			if closeErr == nil && runtimeCtx.Err() == nil {
				t.Fatal("confirmed cleanup retained runtime context")
			}
			if closeErr != nil && runtimeCtx.Err() != nil {
				t.Fatal("quarantine canceled an unconfirmed transport")
			}
		})
	}
}

func TestRegistryFailureBroadcastAndShutdownWaitForOwners(t *testing.T) {
	ctx := testContext(t)
	b := newTestBacking()
	s := &testSource{id: testIdentity}
	r := NewRegistry(func(context.Context, SnapshotIdentity) (SnapshotSource, error) { return s, nil }, func(context.Context, SnapshotSource) (Backing, error) { return b, nil })
	a, err := r.Acquire(ctx, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Acquire(ctx, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("lost NBD server")
	b.err = boom
	close(b.done)
	for _, l := range []*Lease{a, c} {
		select {
		case err := <-l.Failure():
			if !errors.Is(err, boom) {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		<-l.Done()
		if !errors.Is(l.Err(), boom) {
			t.Fatal(l.Err())
		}
	}
	drainCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.Drain(drainCtx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := r.Acquire(ctx, testIdentity); !errors.Is(err, ErrRegistryDraining) {
		t.Fatal(err)
	}
	if s.closes.Load() != 0 {
		t.Fatal("drain closed live source")
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if s.closes.Load() != 1 {
		t.Fatal("source not closed exactly once")
	}
}

func TestRegistryCreateFailureFanoutRetryAndIdentitySeparation(t *testing.T) {
	ctx := testContext(t)
	gate := make(chan struct{})
	boom := errors.New("connect rejected")
	var count atomic.Int32
	var sourcesMu sync.Mutex
	var sources []*testSource
	r := NewRegistry(func(_ context.Context, id SnapshotIdentity) (SnapshotSource, error) {
		s := &testSource{id: id}
		sourcesMu.Lock()
		sources = append(sources, s)
		sourcesMu.Unlock()
		return s, nil
	}, func(context.Context, SnapshotSource) (Backing, error) {
		if count.Add(1) == 1 {
			<-gate
			return nil, boom
		}
		return newTestBacking(), nil
	})
	const n = 12
	errs := make(chan error, n)
	for range n {
		go func() { _, err := r.Acquire(ctx, testIdentity); errs <- err }()
	}
	waitRefs(t, r, testIdentity, n)
	close(gate)
	for range n {
		if err := <-errs; !errors.Is(err, boom) {
			t.Fatal(err)
		}
	}
	a, err := r.Acquire(ctx, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []SnapshotIdentity{
		{Kind: "rootfs", ContentID: testIdentity.ContentID, LayoutID: testIdentity.LayoutID, LogicalSize: testIdentity.LogicalSize},
		{Kind: "ram", ContentID: "sha256:other", LayoutID: testIdentity.LayoutID, LogicalSize: testIdentity.LogicalSize},
		{Kind: "ram", ContentID: testIdentity.ContentID, LayoutID: "two-region", LogicalSize: testIdentity.LogicalSize},
		{Kind: "ram", ContentID: testIdentity.ContentID, LayoutID: testIdentity.LayoutID, LogicalSize: 16384},
	} {
		l, err := r.Acquire(ctx, changed)
		if err != nil {
			t.Fatal(err)
		}
		if l.entry == a.entry {
			t.Fatal("different immutable identity merged")
		}
		if err := l.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range sources {
		if s.closes.Load() != 1 {
			t.Fatal("source close ownership violation")
		}
	}
}

func TestRegistrySourceIdentityMismatchNeverReachesFactory(t *testing.T) {
	ctx := testContext(t)
	id := testIdentity
	id.LayoutID = "wrong"
	s := &testSource{id: id}
	r := NewRegistry(func(context.Context, SnapshotIdentity) (SnapshotSource, error) { return s, nil }, func(context.Context, SnapshotSource) (Backing, error) {
		t.Fatal("mismatched identity reached factory")
		return nil, nil
	})
	if _, err := r.Acquire(ctx, testIdentity); err == nil {
		t.Fatal("accepted wrong logical source")
	}
	if err := r.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if s.closes.Load() != 1 {
		t.Fatal("mismatched source leaked")
	}
}

func TestRegistryAcquireWithSourceUsesOnlyCreatorAndFailedCleanupGatesRetry(t *testing.T) {
	ctx := testContext(t)
	failed := newTestBacking()
	failed.closeStarted, failed.closeGate = make(chan struct{}), make(chan struct{})
	gate := make(chan struct{})
	boom := errors.New("failed after device allocation")
	var creates, opens atomic.Int32
	r := NewRegistry(nil, func(context.Context, SnapshotSource) (Backing, error) {
		if creates.Add(1) == 1 {
			<-gate
			return failed, boom
		}
		return newTestBacking(), nil
	})
	open := func(_ context.Context, id SnapshotIdentity) (SnapshotSource, error) {
		opens.Add(1)
		return &testSource{id: id}, nil
	}
	const n = 8
	errs := make(chan error, n)
	for range n {
		go func() { _, err := r.AcquireWithSource(ctx, testIdentity, open); errs <- err }()
	}
	waitRefs(t, r, testIdentity, n)
	close(gate)
	<-failed.closeStarted
	for range n {
		if err := <-errs; !errors.Is(err, boom) {
			t.Fatal(err)
		}
	}
	// A cleanup can outlive all failed waiters, yet new acquisitions must not
	// retarget its device until the factory's allocated resource disconnects.
	newLease := make(chan *Lease, 1)
	go func() {
		l, err := r.AcquireWithSource(ctx, testIdentity, open)
		if err != nil {
			t.Error(err)
		}
		newLease <- l
	}()
	if creates.Load() != 1 || opens.Load() != 1 {
		t.Fatal("retried during old disconnect")
	}
	close(failed.closeGate)
	l := <-newLease
	if l == nil {
		t.Fatal("retry failed")
	}
	other, err := r.AcquireWithSource(ctx, testIdentity, func(context.Context, SnapshotIdentity) (SnapshotSource, error) {
		t.Error("ready reuse called a second opener")
		return nil, errors.New("must not open")
	})
	if err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 2 {
		t.Fatalf("source opener calls %d", opens.Load())
	}
	if err := other.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Drain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryDrainDuringFailedCreation(t *testing.T) {
	ctx := testContext(t)
	started, finish := make(chan struct{}), make(chan struct{})
	s := &testSource{id: testIdentity}
	r := NewRegistry(func(context.Context, SnapshotIdentity) (SnapshotSource, error) { return s, nil }, func(context.Context, SnapshotSource) (Backing, error) {
		close(started)
		<-finish
		return nil, errors.New("factory failure")
	})
	acquired := make(chan error, 1)
	go func() { _, err := r.Acquire(ctx, testIdentity); acquired <- err }()
	<-started
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.Drain(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() { drained <- r.Drain(ctx) }()
	close(finish)
	if err := <-acquired; err == nil {
		t.Fatal("creation failed successfully")
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if s.closes.Load() != 1 {
		t.Fatal("source close count")
	}
}

func TestRegistryDisconnectFailureQuarantinesIdentityAndPreservesSource(t *testing.T) {
	ctx := testContext(t)
	boom := errors.New("disconnect acknowledgement timed out")
	b := newTestBacking()
	b.closeErr = boom
	s := &testSource{id: testIdentity}
	var creates atomic.Int32
	r := NewRegistry(func(context.Context, SnapshotIdentity) (SnapshotSource, error) { return s, nil }, func(context.Context, SnapshotSource) (Backing, error) { creates.Add(1); return b, nil })
	l, err := r.Acquire(ctx, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := l.Release(ctx); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if s.closes.Load() != 0 {
		t.Fatal("source closed despite unconfirmed device disconnect")
	}
	if _, err := r.Acquire(ctx, testIdentity); !errors.Is(err, boom) {
		t.Fatal("quarantined identity reused or did not fail promptly", err)
	}
	if creates.Load() != 1 {
		t.Fatal("quarantined source was retargeted")
	}
	if err := r.Drain(ctx); !errors.Is(err, boom) {
		t.Fatal("drain falsely reported safe teardown", err)
	}
	if err := r.Drain(ctx); !errors.Is(err, boom) {
		t.Fatal("repeated drain lost quarantine error", err)
	}
	r.mu.Lock()
	e := r.entries[testIdentity]
	r.mu.Unlock()
	if e == nil || e.source != s || e.backing != b {
		t.Fatal("quarantine lost ownership of failed resource")
	}
	if s.closes.Load() != 0 || b.closes.Load() != 1 {
		t.Fatal("quarantine retried teardown automatically")
	}
}
