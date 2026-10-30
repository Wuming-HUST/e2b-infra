//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func TestCheckpointPublishDoesNotReplaceExistingMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	previous := []byte("previous immutable checkpoint")
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	err := publishCheckpointMetadata(t.Context(), metadata.Template{Version: metadata.CurrentVersion}, path)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing checkpoint replaced or wrong failure: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, previous) {
		t.Fatalf("previous metadata changed: %q %v", got, err)
	}
	assertNoCheckpointTemps(t, path)
}

func TestCheckpointPublishConcurrentWritersHaveOneCompleteWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	const writers = 8
	start := make(chan struct{})
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Go(func() {
			<-start
			m := metadata.Template{Version: metadata.CurrentVersion, Context: metadata.Context{User: uuid.NewString()}}
			results <- publishCheckpointMetadata(context.Background(), m, path)
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var succeeded int
	for err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, os.ErrExist) {
			t.Errorf("unexpected publication error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("publication winners=%d, want 1", succeeded)
	}
	m, err := metadata.FromFile(path)
	if err != nil || m.Version != metadata.CurrentVersion || m.Context.User == "" {
		t.Fatalf("winner metadata incomplete: %+v %v", m, err)
	}
	assertNoCheckpointTemps(t, path)
}

func TestCheckpointPublishCanceledContextPublishesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := publishCheckpointMetadata(ctx, metadata.Template{Version: metadata.CurrentVersion}, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled metadata visible: %v", err)
	}
	assertNoCheckpointTemps(t, path)
}

// Sample Err before triggering cancellation, modeling cancellation racing just
// after the last file-read check. Publication must recheck after serialization
// and fsync, immediately before its commit marker becomes visible.
type cancelAfterCheckpointCheck struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Int32
}

func (c *cancelAfterCheckpointCheck) Err() error {
	err := c.Context.Err()
	if c.checks.Add(1) == 3 {
		c.cancel()
	}
	return err
}
func TestCheckpointPublishCancellationAfterSerialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelAfterCheckpointCheck{Context: base, cancel: cancel}
	err := publishCheckpointMetadata(ctx, metadata.Template{Version: metadata.CurrentVersion}, path)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation was published: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late-canceled metadata visible: %v", err)
	}
	assertNoCheckpointTemps(t, path)
}

func assertNoCheckpointTemps(t *testing.T, path string) {
	t.Helper()
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".checkpoint-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("checkpoint temporary files leaked: %v %v", temps, err)
	}
}

// Use a real immutable file for the payload's path and size. Unneeded data
// methods remain unimplemented instead of inventing a fake cache.
type checkpointPathDiff struct {
	build.Diff
	path string
}

func (d checkpointPathDiff) CachePath() (string, error) { return d.path, nil }
func (d checkpointPathDiff) Size(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return d.FileSize()
}
func (d checkpointPathDiff) FileSize() (int64, error) {
	info, err := os.Stat(d.path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

type checkpointDigestDiff struct {
	checkpointPathDiff
	digest [sha256.Size]byte
	valid  bool
}

func (d checkpointDigestDiff) ProcessMemorySHA256() ([sha256.Size]byte, bool) {
	return d.digest, d.valid
}

func newCheckpointSnapshot(t *testing.T) (*Snapshot, []byte) {
	t.Helper()
	dir := t.TempDir()
	page := os.Getpagesize()
	data := bytes.Repeat([]byte{0x7B}, page)
	memPath := filepath.Join(dir, "memory")
	if err := os.WriteFile(memPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state")
	if err := os.WriteFile(statePath, []byte("frozen FC device state"), 0o600); err != nil {
		t.Fatal(err)
	}
	memHeader, err := header.NewHeader(header.NewTemplateMetadata(uuid.New(), uint64(page), uint64(page)), nil)
	if err != nil {
		t.Fatal(err)
	}
	rootMetadata := header.NewTemplateMetadata(uuid.New(), 4096, 4096)
	rootHeader, err := header.NewHeader(rootMetadata, []*header.BuildMap{{Offset: 0, Length: 4096, BuildId: uuid.New(), BuildStorageOffset: 0}})
	if err != nil {
		t.Fatal(err)
	}
	return &Snapshot{
		MemfileDiff: checkpointPathDiff{path: memPath}, MemfileDiffHeader: memHeader,
		RootfsDiff: &build.NoDiff{}, RootfsDiffHeader: rootHeader,
		Snapfile: template.NewLocalFileLink(statePath), Metafile: template.NewLocalFileLink(filepath.Join(dir, "metadata.json")),
	}, data
}

func TestCheckpointSealFullLogicalUsesRealCaptureDigestAndEmptyRootfs(t *testing.T) {
	s, want := newCheckpointSnapshot(t)
	page := os.Getpagesize()
	mapped, err := unix.Mmap(-1, 0, page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(mapped)
	copy(mapped, want)
	cache, err := block.NewCacheFromProcessMemory(t.Context(), int64(page), filepath.Join(t.TempDir(), "captured"), os.Getpid(), []block.Range{{Start: int64(uintptr(unsafe.Pointer(&mapped[0]))), Size: int64(page)}})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := build.NewLocalDiffFromCache(build.GetDiffStoreKey(uuid.NewString(), build.Memfile), cache)
	if err != nil {
		cache.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := diff.Close(); err != nil {
			t.Error(err)
		}
	})
	s.MemfileDiff = diff
	if err := sealSnapshotMetadata(t.Context(), s, metadata.Template{Version: metadata.CurrentVersion}); err != nil {
		t.Fatal(err)
	}
	m, err := metadata.FromFile(s.Metafile.Path())
	if err != nil {
		t.Fatal(err)
	}
	c := m.Checkpoint
	if c == nil || c.RAMExportMode != "full_logical" || c.LogicalRAMSize != uint64(page) {
		t.Fatalf("capture mislabeled: %+v", c)
	}
	wantDigest := sha256.Sum256(want)
	if c.LogicalRAMSHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("capture digest lost: got %s want %x", c.LogicalRAMSHA256, wantDigest)
	}
	empty := sha256.Sum256(nil)
	if c.RootfsDiffSHA256 != hex.EncodeToString(empty[:]) {
		t.Fatalf("NoDiff rootfs not sealed as empty payload: %s", c.RootfsDiffSHA256)
	}
	stateDigest := sha256.Sum256([]byte("frozen FC device state"))
	if c.FCStateSHA256 != hex.EncodeToString(stateDigest[:]) {
		t.Fatal("FC state digest does not describe frozen state")
	}
	for _, item := range []struct {
		h    *header.Header
		want string
	}{{s.MemfileDiffHeader, c.RAMHeaderSHA256}, {s.RootfsDiffHeader, c.RootfsHeaderSHA256}} {
		encoded, err := header.Serialize(item.h.Metadata, item.h.Mapping)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(encoded)
		if item.want != hex.EncodeToString(sum[:]) {
			t.Fatal("header digest missing or inconsistent")
		}
	}
}

func TestCheckpointSealTrackedUsesPayloadDigestWithoutLogicalHash(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonempty", true: "empty"}[empty], func(t *testing.T) {
			s, data := newCheckpointSnapshot(t)
			s.RAMExportMode = "tracked_delta"
			if empty {
				data = nil
				s.MemfileDiffHeader.Mapping[0].BuildId = uuid.New()
				path, _ := s.MemfileDiff.CachePath()
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path, _ := s.MemfileDiff.CachePath()
			s.MemfileDiff = checkpointDigestDiff{checkpointPathDiff: checkpointPathDiff{path: path}, digest: sha256.Sum256(data), valid: true}
			if err := sealSnapshotMetadata(t.Context(), s, metadata.Template{Version: metadata.CurrentVersion}); err != nil {
				t.Fatal(err)
			}
			m, err := metadata.FromFile(s.Metafile.Path())
			if err != nil {
				t.Fatal(err)
			}
			c := m.Checkpoint
			want := sha256.Sum256(data)
			if c.FormatVersion != 2 || c.RAMExportMode != "tracked_delta" || c.RAMTrackingVersion != 1 ||
				c.LogicalRAMSHA256 != "" || c.RAMPayloadSHA256 != hex.EncodeToString(want[:]) || c.RAMPayloadSize != uint64(len(data)) {
				t.Fatalf("invalid tracked manifest: %+v", c)
			}
		})
	}
}

func TestCheckpointSealTrackedRejectsIncompletePayloadBeforePublish(t *testing.T) {
	for _, change := range []string{"missing_digest", "wrong_digest_state", "wrong_length"} {
		t.Run(change, func(t *testing.T) {
			s, data := newCheckpointSnapshot(t)
			s.RAMExportMode = "tracked_delta"
			path, _ := s.MemfileDiff.CachePath()
			s.MemfileDiff = checkpointDigestDiff{checkpointPathDiff: checkpointPathDiff{path: path}, digest: sha256.Sum256(data), valid: true}
			switch change {
			case "missing_digest":
				s.MemfileDiff = checkpointPathDiff{path: path}
			case "wrong_digest_state":
				s.MemfileDiff = checkpointDigestDiff{checkpointPathDiff: checkpointPathDiff{path: path}}
			case "wrong_length":
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := sealSnapshotMetadata(t.Context(), s, metadata.Template{Version: metadata.CurrentVersion}); err == nil {
				t.Fatal("published incomplete tracked payload")
			}
			if _, err := os.Stat(s.Metafile.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("metadata became visible: %v", err)
			}
		})
	}
}

func TestCheckpointSealFullLogicalRequiresCapturedDigest(t *testing.T) {
	for _, validMethod := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-capture-method", true: "capture-not-valid"}[validMethod], func(t *testing.T) {
			s, _ := newCheckpointSnapshot(t)
			if validMethod {
				s.MemfileDiff = checkpointDigestDiff{checkpointPathDiff: s.MemfileDiff.(checkpointPathDiff), valid: false}
			}
			if err := sealSnapshotMetadata(t.Context(), s, metadata.Template{Version: metadata.CurrentVersion}); err == nil {
				t.Fatal("complete header falsely accepted without a capture digest")
			}
			if _, err := os.Stat(s.Metafile.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed checkpoint published: %v", err)
			}
		})
	}
}

func TestCheckpointSealInheritedHeaderIsNotFullLogical(t *testing.T) {
	s, data := newCheckpointSnapshot(t)
	// A digest of bytes copied this generation does not certify an inherited
	// logical image; its header still references a different immutable parent.
	s.MemfileDiffHeader.Mapping[0].BuildId = uuid.New()
	s.MemfileDiff = checkpointDigestDiff{checkpointPathDiff: s.MemfileDiff.(checkpointPathDiff), digest: sha256.Sum256(data), valid: true}
	if err := sealSnapshotMetadata(t.Context(), s, metadata.Template{Version: metadata.CurrentVersion}); err != nil {
		t.Fatal(err)
	}
	m, err := metadata.FromFile(s.Metafile.Path())
	if err != nil {
		t.Fatal(err)
	}
	if m.Checkpoint == nil || m.Checkpoint.RAMExportMode != "uffd_delta" || m.Checkpoint.LogicalRAMSHA256 != "" {
		t.Fatalf("inherited image mislabeled as full logical: %+v", m.Checkpoint)
	}
}

func TestCheckpointSealRejectsCaptureLengthMismatch(t *testing.T) {
	for _, factor := range []int{0, 2} {
		t.Run(map[int]string{0: "truncated", 2: "oversized"}[factor], func(t *testing.T) {
			s, data := newCheckpointSnapshot(t)
			pathDiff := s.MemfileDiff.(checkpointPathDiff)
			if err := os.Truncate(pathDiff.path, int64(len(data)*factor)); err != nil {
				t.Fatal(err)
			}
			s.MemfileDiff = checkpointDigestDiff{checkpointPathDiff: pathDiff, digest: sha256.Sum256(data), valid: true}
			if err := sealSnapshotMetadata(t.Context(), s, metadata.Template{Version: metadata.CurrentVersion}); err == nil {
				t.Fatal("complete header plus digest accepted a different capture length")
			}
			if _, err := os.Stat(s.Metafile.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("length-mismatched checkpoint published: %v", err)
			}
		})
	}
}
