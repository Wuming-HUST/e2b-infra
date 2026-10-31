package pmem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

type testSource struct {
	size  int64
	reads atomic.Int64
	short bool
}

func (s *testSource) Size(context.Context) (int64, error) { return s.size, nil }
func (s *testSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	s.reads.Add(1)
	for i := range p {
		p[i] = byte((off + int64(i)) / 4096 % 251)
	}
	if s.short {
		return len(p) - 1, io.EOF
	}
	return len(p), nil
}
func testImage() Image {
	h := sha256.New()
	b := make([]byte, 4<<20)
	s := testSource{}
	for off := int64(0); off < 128<<20; off += int64(len(b)) {
		_, _ = s.ReadAt(context.Background(), b, off)
		_, _ = h.Write(b)
	}
	return Image{SHA256: hex.EncodeToString(h.Sum(nil)), Size: 128 << 20}
}
func TestPmemMaterializeSingleFlightWaiterCancellationAndSameInode(t *testing.T) {
	image := testImage()
	cache := NewCache(t.TempDir())
	source := &testSource{size: int64(image.Size)}
	started, release := make(chan struct{}), make(chan struct{})
	var opens atomic.Int64
	open := func(context.Context) (storage.SeekableReader, error) {
		opens.Add(1)
		close(started)
		<-release
		return source, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, e := cache.Materialize(ctx, image, open); first <- e }()
	<-started
	cancel()
	if !errors.Is(<-first, context.Canceled) {
		t.Fatal("waiter cancellation ignored")
	}
	var wg sync.WaitGroup
	paths := make([]string, 4)
	errs := make([]error, 4)
	for i := range paths {
		wg.Add(1)
		go func(i int) { defer wg.Done(); paths[i], errs[i] = cache.Materialize(t.Context(), image, open) }(i)
	}
	close(release)
	wg.Wait()
	for i := range paths {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if paths[i] != paths[0] {
			t.Fatal("different published paths")
		}
	}
	if opens.Load() != 1 || source.reads.Load() != 32 {
		t.Fatalf("opens=%d reads=%d", opens.Load(), source.reads.Load())
	}
	before, e := os.Stat(paths[0])
	if e != nil {
		t.Fatal(e)
	}
	again, e := cache.Materialize(t.Context(), image, open)
	if e != nil {
		t.Fatal(e)
	}
	after, _ := os.Stat(again)
	if !os.SameFile(before, after) {
		t.Fatal("replaced live inode")
	}
	if before.Mode().Perm() != 0444 || uint64(before.Size()) != image.Size {
		t.Fatal("unsealed image")
	}
	// Even an already-ready cache must reject altered content before binding.
	if e = os.Chmod(again, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e = cache.Materialize(t.Context(), image, open); e == nil {
		t.Fatal("accepted mutable lower")
	}
}
func TestPmemMaterializeRejectsShortWrongSizeDigestAndSymlink(t *testing.T) {
	image := testImage()
	for _, name := range []string{"size", "short", "digest", "symlink"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cache := NewCache(dir)
			source := &testSource{size: int64(image.Size)}
			bad := image
			switch name {
			case "size":
				source.size -= 4096
			case "short":
				source.short = true
			case "digest":
				bad.SHA256 = hex.EncodeToString(make([]byte, 32))
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if e := os.WriteFile(target, []byte("untrusted"), 0444); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(target, filepath.Join(dir, bad.SHA256+".raw")); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := cache.Materialize(t.Context(), bad, func(context.Context) (storage.SeekableReader, error) { return source, nil }); e == nil {
				t.Fatal("accepted bad lower")
			}
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if entry.Name()[0] == '.' {
					t.Fatal("temporary file leaked")
				}
			}
		})
	}
}
func TestPmemLayoutRejectsUnsupportedAndOverflowBindings(t *testing.T) {
	good := Layout{FormatVersion: 1, Mode: Mode, Lower: testImage(), GuestAddress: 1 << 38}
	if e := good.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"r2", "unaligned", "overflow", "empty", "bad_hash", "data_offset"} {
		bad := good
		switch name {
		case "r2":
			bad.Mode = "nbd-backed-pmem"
		case "unaligned":
			bad.GuestAddress++
		case "overflow":
			bad.GuestAddress = ^uint64(0)
		case "empty":
			bad.Lower.Size = 0
		case "data_offset":
			bad.DataOffset = 4096
		case "bad_hash":
			bad.Lower.SHA256 = "../../file"
		}
		if e := bad.Validate(); e == nil {
			t.Fatal(name)
		}
	}
}

func TestPmemLayoutBindsNamespaceOffsetInDigest(t *testing.T) {
	raw := Layout{FormatVersion: 1, Mode: Mode, Lower: testImage(), GuestAddress: 1 << 38}
	pfn := raw
	pfn.DataOffset = 2 << 20
	if err := pfn.Validate(); err != nil {
		t.Fatal(err)
	}
	if raw.Digest() == pfn.Digest() {
		t.Fatal("namespace offset not bound in checkpoint")
	}
}
