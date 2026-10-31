//go:build linux

package pmem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// Run in a private mount namespace with an explicitly supplied small tmpfs.
// Never fill the host's ordinary filesystem to simulate ENOSPC.
func TestPmemMaterializeDiskFullIntegration(t *testing.T) {
	dir := os.Getenv("E2B_PMEM_ENOSPC_DIR")
	if dir == "" {
		t.Skip("requires explicit private tmpfs E2B_PMEM_ENOSPC_DIR")
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("tmpfs path must be absolute")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	image := testImage()
	if uint64(fs.Type) != 0x01021994 || uint64(fs.Bavail)*uint64(fs.Bsize) >= image.Size {
		t.Fatal("requires a dedicated tmpfs smaller than the test image")
	}
	cacheDir := filepath.Join(dir, "lower-cache")
	if err := os.Mkdir(cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	cache := NewCache(cacheDir)
	source := &testSource{size: int64(image.Size)}
	path, err := cache.Materialize(t.Context(), image, func(context.Context) (storage.SeekableReader, error) { return source, nil })
	if !errors.Is(err, syscall.ENOSPC) || path != "" {
		t.Fatalf("path=%q error=%v; want unpublished ENOSPC", path, err)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial or published files survived failed materialization: %v", entries)
	}
	t.Log("real ENOSPC rejected; no final image or temporary partial file published")
}
