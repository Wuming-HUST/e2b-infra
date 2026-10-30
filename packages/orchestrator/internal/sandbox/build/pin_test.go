package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type pinTestDiff struct {
	NoDiff
	key     DiffStoreKey
	data    []byte
	reads   atomic.Int32
	closes  atomic.Int32
	inits   atomic.Int32
	initErr error
	entered chan struct{}
	resume  chan struct{}
}

func (d *pinTestDiff) CacheKey() DiffStoreKey     { return d.key }
func (d *pinTestDiff) Init(context.Context) error { d.inits.Add(1); return d.initErr }
func (d *pinTestDiff) Close() error               { d.closes.Add(1); return nil }
func (d *pinTestDiff) FileSize() (int64, error)   { return int64(len(d.data)), nil }
func (d *pinTestDiff) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if d.entered != nil {
		close(d.entered)
		<-d.resume
	}
	d.reads.Add(1)
	if d.closes.Load() != 0 {
		return 0, os.ErrClosed
	}
	return copy(p, d.data[off:]), nil
}

func pinTestStore(t *testing.T, ttl time.Duration) *DiffStore {
	t.Helper()
	s, err := NewDiffStore(cfg.Config{}, nil, t.TempDir(), ttl, time.Hour)
	require.NoError(t, err)
	t.Cleanup(s.RemoveCache)
	return s
}

func waitPinTest(t *testing.T, f func() bool) {
	t.Helper()
	require.Eventually(t, f, 3*time.Second, time.Millisecond)
}

func TestPinnedFileSurvivesInvalidationAndLastViewClosesOnce(t *testing.T) {
	t.Parallel()
	s := pinTestStore(t, time.Hour)
	a, b := uuid.New(), uuid.New()
	da := &pinTestDiff{key: GetDiffStoreKey(a.String(), Memfile), data: bytes.Repeat([]byte{0x31}, 8192)}
	db := &pinTestDiff{key: GetDiffStoreKey(b.String(), Memfile), data: bytes.Repeat([]byte{0x72}, 4096)}
	s.Add(da)
	s.Add(db)
	h, err := header.NewHeader(&header.Metadata{Version: 3, Size: 16384, BlockSize: 4096}, []*header.BuildMap{
		{Offset: 0, Length: 4096, BuildId: a},
		{Offset: 4096, Length: 4096, BuildId: b},
		{Offset: 8192, Length: 4096, BuildId: uuid.Nil},
		{Offset: 12288, Length: 4096, BuildId: a, BuildStorageOffset: 4096},
	})
	require.NoError(t, err)
	f := NewFile(h, s, Memfile, nil, blockmetrics.Metrics{})
	v1, err := f.Pin(t.Context())
	require.NoError(t, err)
	v2, err := f.Pin(t.Context())
	require.NoError(t, err)
	require.Len(t, v1.diffs, 2)
	require.Zero(t, da.reads.Load(), "pinning must not fetch RAM payload")
	require.Zero(t, db.reads.Load())
	entry := v1.diffs[a].entry
	s.RemoveCache()
	waitPinTest(t, func() bool { entry.mu.Lock(); defer entry.mu.Unlock(); return entry.retired })
	// The view owns its manifest as well as its unread backing objects.
	h.Mapping[0].BuildId = uuid.New()
	got := bytes.Repeat([]byte{0xff}, 16384)
	_, err = v1.ReadAt(t.Context(), got, 0)
	require.NoError(t, err)
	want := append(bytes.Repeat([]byte{0x31}, 4096), bytes.Repeat([]byte{0x72}, 4096)...)
	want = append(want, make([]byte, 4096)...)
	want = append(want, bytes.Repeat([]byte{0x31}, 4096)...)
	require.Equal(t, want, got)
	require.NoError(t, v1.Close())
	require.Zero(t, da.closes.Load())
	require.Zero(t, db.closes.Load())
	_, err = v2.ReadAt(t.Context(), got[:512], 17)
	require.NoError(t, err)
	require.NoError(t, v2.Close())
	require.NoError(t, v2.Close())
	waitPinTest(t, func() bool { return da.closes.Load() == 1 && db.closes.Load() == 1 })
	_, err = v2.ReadAt(t.Context(), got, 0)
	require.ErrorIs(t, err, os.ErrClosed)
}

func TestPinnedDiffSurvivesTTLAndExpiredReplacementRetiresOwner(t *testing.T) {
	t.Parallel()
	s := pinTestStore(t, 10*time.Millisecond)
	old := &pinTestDiff{key: "ttl", data: []byte("original")}
	s.Add(old)
	first, err := s.Pin(t.Context(), &pinTestDiff{key: old.key})
	require.NoError(t, err)
	time.Sleep(25 * time.Millisecond)
	// No Start/DeleteExpired: exercise the expired-but-still-present Set path.
	replacement := &pinTestDiff{key: old.key, data: []byte("replaced")}
	second, err := s.Pin(t.Context(), replacement)
	require.NoError(t, err)
	waitPinTest(t, func() bool { first.entry.mu.Lock(); defer first.entry.mu.Unlock(); return first.entry.retired })
	b := make([]byte, 8)
	_, err = first.ReadAt(t.Context(), b, 0)
	require.NoError(t, err)
	require.Equal(t, "original", string(b))
	require.Zero(t, old.closes.Load())
	require.NoError(t, first.Close())
	require.EqualValues(t, 1, old.closes.Load())
	_, err = second.ReadAt(t.Context(), b, 0)
	require.NoError(t, err)
	require.Equal(t, "replaced", string(b))
	s.RemoveCache()
	require.NoError(t, second.Close())
	waitPinTest(t, func() bool { return replacement.closes.Load() == 1 })
}

func TestPinnedDiffSurvivesAutomaticTTLEviction(t *testing.T) {
	t.Parallel()
	s := pinTestStore(t, 20*time.Millisecond)
	go s.cache.Start()
	t.Cleanup(s.cache.Stop)
	d := &pinTestDiff{key: "automatic-ttl", data: []byte{0x51}}
	pin, err := s.Pin(t.Context(), d)
	require.NoError(t, err)
	waitPinTest(t, func() bool { pin.entry.mu.Lock(); defer pin.entry.mu.Unlock(); return pin.entry.retired })
	require.False(t, s.Has(d))
	require.Zero(t, d.closes.Load())
	got := []byte{0}
	_, err = pin.ReadAt(t.Context(), got, 0)
	require.NoError(t, err)
	require.Equal(t, []byte{0x51}, got)
	require.NoError(t, pin.Close())
	require.EqualValues(t, 1, d.closes.Load())
}

type pinFailStorage struct{ storage.StorageProvider }

func (pinFailStorage) OpenSeekable(context.Context, string, storage.SeekableObjectType) (storage.Seekable, error) {
	return nil, errors.New("remote object unavailable")
}

func TestPinnedFilePartialAcquisitionReleasesPins(t *testing.T) {
	t.Parallel()
	s := pinTestStore(t, time.Hour)
	a, b := uuid.New(), uuid.New()
	d := &pinTestDiff{key: GetDiffStoreKey(a.String(), Memfile), data: []byte{1}}
	require.NoError(t, s.Add(d))
	h, err := header.NewHeader(&header.Metadata{Version: 3, Size: 8192, BlockSize: 4096}, []*header.BuildMap{
		{Offset: 0, Length: 4096, BuildId: a}, {Offset: 4096, Length: 4096, BuildId: b},
	})
	require.NoError(t, err)
	f := NewFile(h, s, Memfile, pinFailStorage{}, blockmetrics.Metrics{})
	view, err := f.Pin(t.Context())
	require.ErrorContains(t, err, "remote object unavailable")
	require.Nil(t, view)
	s.RemoveCache()
	waitPinTest(t, func() bool { return d.closes.Load() == 1 })
}

func TestDiffPinFailedStorageInitDoesNotHangClose(t *testing.T) {
	t.Parallel()
	s := pinTestStore(t, time.Hour)
	d, err := newStorageDiff(s.cachePath, uuid.NewString(), Memfile, 4096, blockmetrics.Metrics{}, pinFailStorage{}, nil)
	require.NoError(t, err)
	_, err = s.Pin(t.Context(), d)
	require.ErrorContains(t, err, "remote object unavailable")
	done := make(chan error, 1)
	go func() { done <- d.Close() }()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "remote object unavailable")
	case <-time.After(time.Second):
		t.Fatal("failed initialization left Close waiting forever")
	}
}

func TestDiffPinAddRejectsClosedLocalDiff(t *testing.T) {
	t.Parallel()
	s := pinTestStore(t, time.Hour)
	d := newRootFSDiff(t, s.cachePath, "closed-local")
	require.NoError(t, d.Close())
	require.ErrorIs(t, s.Add(d), os.ErrClosed)
	require.False(t, s.Has(d))
}

func TestLocalDiffOwnershipTransferProtectsPinnedSource(t *testing.T) {
	t.Parallel()
	s := pinTestStore(t, time.Hour)
	id := uuid.New()
	f, err := NewLocalDiffFile(s.cachePath, id.String(), Memfile)
	require.NoError(t, err)
	want := bytes.Repeat([]byte{0x7a}, 4096)
	_, err = f.Write(want)
	require.NoError(t, err)
	d, err := f.CloseToDiff(4096)
	require.NoError(t, err)
	s.Add(d)
	pin, err := s.Pin(t.Context(), &pinTestDiff{key: d.CacheKey()})
	require.NoError(t, err)
	// Simulate Snapshot.Close, then template invalidation and build eviction.
	require.NoError(t, d.Close())
	s.RemoveCache()
	waitPinTest(t, func() bool { pin.entry.mu.Lock(); defer pin.entry.mu.Unlock(); return pin.entry.retired })
	got := make([]byte, 4096)
	_, err = pin.ReadAt(t.Context(), got, 0)
	require.NoError(t, err)
	require.Equal(t, want, got)
	path, err := d.CachePath()
	require.NoError(t, err)
	require.FileExists(t, path)
	require.NoError(t, pin.Close())
	require.NoError(t, pin.Close())
	require.NoError(t, d.Close())
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPinnedDiffCloseDrainsRead(t *testing.T) {
	t.Parallel()
	s := pinTestStore(t, time.Hour)
	d := &pinTestDiff{key: "blocked", data: []byte{1}, entered: make(chan struct{}), resume: make(chan struct{})}
	pin, err := s.Pin(t.Context(), d)
	require.NoError(t, err)
	s.RemoveCache()
	readDone := make(chan error, 1)
	go func() { _, err := pin.ReadAt(t.Context(), make([]byte, 1), 0); readDone <- err }()
	<-d.entered
	closed := make(chan error, 1)
	go func() { closed <- pin.Close() }()
	select {
	case <-closed:
		t.Fatal("closed an in-flight source read")
	case <-time.After(10 * time.Millisecond):
	}
	require.Zero(t, d.closes.Load())
	close(d.resume)
	require.NoError(t, <-readDone)
	require.NoError(t, <-closed)
	waitPinTest(t, func() bool { return d.closes.Load() == 1 })
}

func TestDiffPinInitializationAndConcurrentInvalidation(t *testing.T) {
	t.Parallel()
	s := pinTestStore(t, time.Hour)
	failed := &pinTestDiff{key: "failed", initErr: errors.New("open failed")}
	_, err := s.Pin(t.Context(), failed)
	require.ErrorContains(t, err, "open failed")
	require.False(t, s.Has(failed))
	var wg sync.WaitGroup
	var allMu sync.Mutex
	var all []*pinTestDiff
	for g := range 8 {
		wg.Go(func() {
			for i := range 30 {
				d := &pinTestDiff{key: DiffStoreKey(fmt.Sprintf("key-%d", g%2)), data: []byte{7}}
				allMu.Lock()
				all = append(all, d)
				allMu.Unlock()
				if i%3 == 0 {
					s.Add(d)
				}
				candidate := &pinTestDiff{key: d.key, data: []byte{7}}
				allMu.Lock()
				all = append(all, candidate)
				allMu.Unlock()
				pin, err := s.Pin(t.Context(), candidate)
				if !assertPinNoError(t, err) {
					continue
				}
				if i%2 == 0 {
					s.RemoveCache()
				}
				b := make([]byte, 1)
				_, err = pin.ReadAt(t.Context(), b, 0)
				assertPinNoError(t, err)
				if b[0] != 7 {
					t.Errorf("wrong source byte: %d", b[0])
				}
				assertPinNoError(t, pin.Close())
			}
		})
	}
	wg.Wait()
	s.RemoveCache()
	// Some candidates hit existing entries and were never initialized/owned.
	waitPinTest(t, func() bool {
		for _, d := range all {
			if d.inits.Load() > 0 && d.closes.Load() != 1 {
				return false
			}
		}
		return true
	})
	for _, d := range all {
		require.LessOrEqual(t, d.closes.Load(), int32(1))
	}
}

func assertPinNoError(t *testing.T, err error) bool {
	t.Helper()
	if err != nil {
		t.Error(err)
		return false
	}
	return true
}
