//go:build linux

package block

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// M01 process-reader check: the nonzero file-backed parent has no present PTE
// before export. process_vm_readv must fault it in rather than lose its bytes.
// This exercises a native process mapping, not a KVM restore acceptance test.
func TestProcessMemoryUnfaultedNonzeroParent(t *testing.T) {
	path := t.TempDir() + "/parent"
	want := bytes.Repeat([]byte{0x7b}, os.Getpagesize())
	require.NoError(t, os.WriteFile(path, want, 0o600))
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	mapped, err := unix.Mmap(int(f.Fd()), 0, len(want), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
	require.NoError(t, err)
	defer unix.Munmap(mapped)
	addr := uintptr(unsafe.Pointer(&mapped[0]))
	pagemap, err := os.Open("/proc/self/pagemap")
	if err != nil {
		t.Skipf("cannot establish absent-PTE precondition: %v", err)
	}
	defer pagemap.Close()
	var entry [8]byte
	_, err = pagemap.ReadAt(entry[:], int64(addr/uintptr(os.Getpagesize()))*8)
	require.NoError(t, err)
	require.Zero(t, binary.LittleEndian.Uint64(entry[:])&(uint64(1)<<63), "parent page must not be faulted in before export")
	cache, err := NewCacheFromProcessMemory(t.Context(), int64(os.Getpagesize()), t.TempDir()+"/cache", os.Getpid(), []Range{{Start: int64(addr), Size: int64(len(want))}})
	require.NoError(t, err)
	defer cache.Close()
	got := make([]byte, len(want))
	_, err = cache.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, want, got)
	digest, ok := cache.ProcessMemorySHA256()
	require.True(t, ok)
	require.Equal(t, sha256.Sum256(want), digest)
}

func TestProcessMemoryShortReadsContinueAcrossRegions(t *testing.T) {
	cache, err := NewCache(8192, 4096, t.TempDir()+"/cache", false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })
	expected := bytes.Repeat([]byte{0xa1}, 8192)
	for i := 1000; i < len(expected); i++ {
		expected[i] = 0xb2
	}
	var copied int
	var calls int
	read := func(_ int, local []unix.Iovec, remote []unix.RemoteIovec, _ uint) (int, error) {
		calls++
		require.Equal(t, uint64(8192-copied), local[0].Len)
		// Short reads end both inside a region and across region boundaries.
		n := min(777, 8192-copied)
		wantAddress := uintptr(0x90000 + copied)
		if copied >= 1000 {
			wantAddress = uintptr(0x10000 + copied - 1000)
		}
		require.Equal(t, wantAddress, remote[0].Base)
		copy(unsafe.Slice(local[0].Base, n), expected[copied:copied+n])
		copied += n
		return n, nil
	}
	err = cache.copyProcessMemoryWithReader(t.Context(), 1, []Range{{Start: 0x90000, Size: 1000}, {Start: 0x10000, Size: 7192}}, read)
	require.NoError(t, err)
	require.Greater(t, calls, 2)
	actual := make([]byte, len(expected))
	_, err = cache.ReadAt(actual, 0)
	require.NoError(t, err)
	require.Equal(t, expected, actual)
	require.Equal(t, []int64{0, 4096}, cache.dirtySortedKeys())
	digest, ok := cache.ProcessMemorySHA256()
	require.True(t, ok)
	require.Equal(t, sha256.Sum256(expected), digest)
}

func TestProcessMemoryReadFailureDoesNotMarkPartialBlocks(t *testing.T) {
	for _, readErr := range []error{nil, unix.EFAULT} {
		cache, err := NewCache(4096, 4096, t.TempDir()+"/cache", false)
		require.NoError(t, err)
		err = cache.copyProcessMemoryWithReader(t.Context(), 1, []Range{{Start: 0x10000, Size: 4096}},
			func(int, []unix.Iovec, []unix.RemoteIovec, uint) (int, error) { return 0, readErr })
		require.Error(t, err)
		require.Empty(t, cache.dirtySortedKeys())
		require.NoError(t, cache.Close())
	}
}

func TestProcessMemoryRetryCanBeCanceled(t *testing.T) {
	cache, err := NewCache(4096, 4096, t.TempDir()+"/cache", false)
	require.NoError(t, err)
	defer cache.Close()
	ctx, cancel := context.WithCancel(t.Context())
	err = cache.copyProcessMemoryWithReader(ctx, 1, []Range{{Start: 0x10000, Size: 4096}},
		func(int, []unix.Iovec, []unix.RemoteIovec, uint) (int, error) {
			cancel()
			return 0, unix.ENOMEM
		})
	require.ErrorIs(t, err, context.Canceled)
}

func TestProcessMemoryFailureCleansCacheAndPreservesExistingArtifact(t *testing.T) {
	path := t.TempDir() + "/cache"
	_, err := NewCacheFromProcessMemory(t.Context(), 4096, path, -1, []Range{{Start: 0x10000, Size: 4096}})
	require.Error(t, err)
	_, err = os.Stat(path)
	require.True(t, errors.Is(err, os.ErrNotExist))
	require.NoError(t, os.WriteFile(path, []byte("existing snapshot"), 0o600))
	_, err = NewCacheFromProcessMemory(t.Context(), 4096, path, os.Getpid(), []Range{{Start: 0x10000, Size: 4096}})
	require.ErrorIs(t, err, os.ErrExist)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "existing snapshot", string(data))
}

func TestProcessMemoryRejectsInvalidRangesBeforeCreatingArtifact(t *testing.T) {
	for _, ranges := range [][]Range{
		{{Start: -1, Size: 4096}},
		{{Start: 1, Size: -4096}},
		{{Start: math.MaxInt64 - 1, Size: 4096}},
		{{Start: 0, Size: math.MaxInt64}, {Start: 0, Size: 4096}},
		{{Start: 1, Size: 4095}},
	} {
		path := t.TempDir() + "/cache"
		_, err := NewCacheFromProcessMemory(t.Context(), 4096, path, 1, ranges)
		require.Error(t, err)
		_, err = os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}
