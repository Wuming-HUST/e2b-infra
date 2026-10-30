//go:build linux

package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
)

// Run only this test under sudo with E2B_SHARED_NBD_KERNEL_TEST=1. It allocates
// one kernel-selected connection, never selects a pre-existing device by ID,
// never drops global caches, and keeps total logical RAM at four MiB. Set
// E2B_SHARED_NBD_REQUIRE_PFN=1 to fail when pagemap PFNs are permission-masked.
// Two mappings are a block mmap/COW contract test, not a two-Firecracker test.
func TestReadonlyNBDKernelSharing(t *testing.T) {
	testReadonlyNBDKernelSharing(t, false)
}

// This separate gate proves reclamation of this device's clean page cache. It
// is still a two-mapping mechanism test, not Firecracker reclaim acceptance.
func TestReadonlyNBDKernelCacheEviction(t *testing.T) {
	if os.Getenv("E2B_SHARED_NBD_REQUIRE_EVICTION") != "1" {
		t.Skip("set E2B_SHARED_NBD_REQUIRE_EVICTION=1 to require actual owned-device cache eviction")
	}
	testReadonlyNBDKernelSharing(t, true)
}

func testReadonlyNBDKernelSharing(t *testing.T, requireEviction bool) {
	if os.Getenv("E2B_SHARED_NBD_KERNEL_TEST") != "1" {
		if requireEviction {
			t.Fatal("EVICTION_NOT_ACCEPTED: E2B_SHARED_NBD_KERNEL_TEST=1 is also required")
		}
		t.Skip("set E2B_SHARED_NBD_KERNEL_TEST=1 on an authorized Linux NBD host")
	}
	page := os.Getpagesize()
	if page != 4096 {
		if requireEviction {
			t.Fatalf("EVICTION_NOT_ACCEPTED: requires a 4096-byte host page; actual=%d", page)
		}
		t.Skipf("first-stage kernel contract requires a 4096-byte host page; actual=%d", page)
	}
	t.Setenv("E2B_UNRESTRICTED_STRESS", "true")
	pool, err := NewDevicePool()
	if err != nil {
		t.Fatal(err)
	}
	if !pool.dynamic {
		t.Fatal("test must use kernel-selected dynamic allocation")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := pool.Close(ctx); err != nil {
			t.Errorf("close own pool: %v", err)
		}
	})
	const size = 4 << 20
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i/page)%251 + 1)
	}
	var failed atomic.Bool
	var reads atomic.Int64
	var cleanPageReads atomic.Int64
	p := &readonlyMountSource{dispatchProvider: dispatchProvider{data: data}}
	p.read = func(ctx context.Context, b []byte, off int64) (int, error) {
		reads.Add(1)
		if failed.Load() {
			return 0, errors.New("injected immutable source failure")
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if off < 0 || off > int64(len(data)) {
			return 0, io.EOF
		}
		n := copy(b, data[off:])
		if n != len(b) {
			return n, io.EOF
		}
		if off < int64(3*page) && off+int64(n) > int64(2*page) {
			// Count successful source reads covering page 2, not unrelated
			// requests or injected failures elsewhere in the image.
			cleanPageReads.Add(1)
		}
		return n, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	mount := NewReadonlyDirectPathMount(p, pool, &featureflags.Client{})
	openCtx, cancelOpen := context.WithCancel(ctx)
	t.Cleanup(cancelOpen)
	idx, err := mount.Open(openCtx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if err := mount.Close(cleanupCtx); err != nil {
			t.Errorf("close owned nbd%d: %v", idx, err)
		}
		if p.closes.Load() != 0 {
			t.Error("mount closed registry-owned source")
		}
	})
	// Canceling the acquisition/RPC owner cannot relinquish a connected kernel
	// device. Runtime transport ownership ends only through explicit Close.
	cancelOpen()
	path := GetDevicePath(idx)
	t.Logf("owned_device=%s pid=%d logical_size=%d host_page_size=%d", path, os.Getpid(), size, page)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	ro, capacity, err := readonlyDeviceInfo(file)
	if err != nil || !ro || capacity != size {
		t.Fatalf("kernel readonly/capacity: ro=%v bytes=%d err=%v", ro, capacity, err)
	}
	if err := validateReadonlyReady(path, size+int64(page)); err == nil {
		t.Fatal("readiness accepted wrong source capacity")
	}
	probe := make([]byte, page)
	if n, err := file.ReadAt(probe, 0); err != nil || n != len(probe) || !bytes.Equal(probe, data[:page]) {
		t.Fatalf("caller cancellation broke owned transport: n=%d err=%v", n, err)
	}
	a, err := unix.Mmap(int(file.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Munmap(a); err != nil {
			t.Error(err)
		}
	})
	b, err := unix.Mmap(int(file.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Munmap(b); err != nil {
			t.Error(err)
		}
	})
	// First accesses are reads. Collect sharing evidence before a full oracle
	// can warm the image, and sample repeatedly to detect unstable PFNs.
	if a[0] != data[0] || b[0] != data[0] {
		t.Fatal("initial mmap content mismatch")
	}
	pfnVerified := true
	for sample := 0; sample < 3; sample++ {
		pa, ea := pagemapPFN(a, page)
		pb, eb := pagemapPFN(b, page)
		if ea != nil || eb != nil {
			pfnVerified = false
			t.Logf("PFN_UNVERIFIED sample=%d a=%v b=%v", sample, ea, eb)
			break
		}
		t.Logf("SHARED_PFN sample=%d a=%d b=%d", sample, pa, pb)
		if pa != pb {
			t.Fatalf("clean mappings have different PFNs: %d %d", pa, pb)
		}
	}
	if !pfnVerified && os.Getenv("E2B_SHARED_NBD_REQUIRE_PFN") == "1" {
		t.Fatal("PFN evidence required but unavailable")
	}
	a[0] = 0xA5
	if b[0] != data[0] || data[0] == 0xA5 {
		t.Fatal("private A write changed B/source")
	}
	b[page] = 0x5A
	if a[page] != data[page] || data[page] == 0x5A {
		t.Fatal("private B write changed A/source")
	}
	if pfnVerified {
		pa, ea := pagemapPFN(a, page)
		pb, eb := pagemapPFN(b, page)
		if ea != nil || eb != nil || pa == pb {
			t.Fatalf("COW PFNs not distinct: a=%d b=%d errors=%v,%v", pa, pb, ea, eb)
		}
		t.Logf("COW_PFN a=%d b=%d", pa, pb)
	}
	// Drop only a clean mapping page, never a private dirty page. This tests
	// refault after mapping eviction; it is not proof of cache reclamation.
	cleanOff := 2 * page
	if b[cleanOff] != data[cleanOff] {
		t.Fatal("clean page mismatch")
	}
	if err := unix.Madvise(b[cleanOff:cleanOff+page], unix.MADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
	if b[cleanOff] != data[cleanOff] {
		t.Fatal("clean page refault changed content")
	}
	for i := range a {
		want := data[i]
		if i == 0 {
			want = 0xA5
		}
		if a[i] != want {
			t.Fatalf("A oracle mismatch at %d", i)
		}
		want = data[i]
		if i == page {
			want = 0x5A
		}
		if b[i] != want {
			t.Fatalf("B oracle mismatch at %d", i)
		}
	}
	if mount.Err() != nil {
		t.Fatalf("healthy service failed: %v", mount.Err())
	}
	if requireEviction {
		verifyReadonlyNBDCacheEviction(t, file, a, b, data, page, &cleanPageReads)
	}
	// O_DIRECT bypasses any cache populated by the oracle, ensuring fault
	// injection reaches the source without global cache operations or SIGBUS.
	direct, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECT, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := direct.Close(); err != nil {
			t.Error(err)
		}
	})
	aligned, err := unix.Mmap(-1, 0, page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Munmap(aligned); err != nil {
			t.Error(err)
		}
	})
	beforeFailure := reads.Load()
	failed.Store(true)
	n, readErr := direct.ReadAt(aligned, size-int64(page))
	if readErr == nil || n == page {
		t.Fatalf("source failure falsely succeeded: n=%d err=%v", n, readErr)
	}
	if reads.Load() <= beforeFailure {
		t.Fatal("fault injection did not reach source")
	}
	select {
	case <-mount.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("source failure not broadcast to holders")
	}
	if mount.Err() == nil {
		t.Fatal("mount failure has no cause")
	}
	if errors.Is(readErr, syscall.EINTR) {
		t.Fatalf("read was interrupted rather than receiving source error: %v", readErr)
	}
	t.Logf("verified capacity=%d readonly=%v COW=true mapping_refault=true source_reads=%d failure=%v read_error=%v PFN_verified=%v", capacity, ro, reads.Load(), mount.Err(), readErr, pfnVerified)
}

func verifyReadonlyNBDCacheEviction(t *testing.T, file *os.File, a, b, source []byte, page int, sourceReads *atomic.Int64) {
	t.Helper()
	off := 2 * page
	cleanA, cleanB := a[off:off+page], b[off:off+page]
	want := source[off : off+page]
	if !bytes.Equal(cleanA, want) || !bytes.Equal(cleanB, want) {
		t.Fatal("EVICTION_NOT_ACCEPTED: clean page differs before eviction")
	}
	// Restrict later refaults to this page. These hints affect only this test's
	// mappings/file; they do not invalidate caches or touch private dirty pages.
	for _, mapping := range [][]byte{cleanA, cleanB} {
		if err := unix.Madvise(mapping, unix.MADV_RANDOM); err != nil {
			t.Fatalf("EVICTION_NOT_ACCEPTED: MADV_RANDOM: %v", err)
		}
	}
	if err := unix.Fadvise(int(file.Fd()), int64(off), int64(page), unix.FADV_RANDOM); err != nil {
		t.Fatalf("EVICTION_NOT_ACCEPTED: FADV_RANDOM: %v", err)
	}
	cacheResident := func(stage string, want bool) {
		t.Helper()
		for i, mapping := range [][]byte{cleanA, cleanB} {
			resident, err := mappingCacheResident(mapping)
			if err != nil || resident != want {
				t.Fatalf("EVICTION_NOT_ACCEPTED: %s mapping=%d cache_resident=%v want=%v err=%v", stage, i, resident, want, err)
			}
		}
		t.Logf("EVICTION_CACHE stage=%s clean_page=2 both_cache_resident=%v", stage, want)
	}
	cacheResident("hot", true)
	// Remove only page 2's PTEs. Dirty private pages 0/1 must retain their COW
	// contents; merely dropping these PTEs cannot prove page-cache reclamation.
	for _, mapping := range [][]byte{cleanA, cleanB} {
		if err := unix.Madvise(mapping, unix.MADV_DONTNEED); err != nil {
			t.Fatalf("EVICTION_NOT_ACCEPTED: clean MADV_DONTNEED: %v", err)
		}
	}
	cacheResident("PTEs_removed_cache_still_hot", true)
	// BLKFLSBUF synchronizes and invalidates only the block device opened by
	// this test. This is its new 4 MiB readonly NBD, never a filesystem mount,
	// an existing NBD number, a global drop_caches operation, or memory pressure.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), uintptr(unix.BLKFLSBUF), 0); errno != 0 {
		t.Fatalf("EVICTION_NOT_ACCEPTED: owned-device BLKFLSBUF: %v", errno)
	}
	cacheResident("owned_device_invalidated", false)
	before := sourceReads.Load()
	if !bytes.Equal(cleanA, want) {
		t.Fatal("EVICTION_NOT_ACCEPTED: A refault changed clean content")
	}
	after := sourceReads.Load()
	if after <= before {
		t.Fatalf("EVICTION_NOT_ACCEPTED: refault did not reread page 2 from source: before=%d after=%d", before, after)
	}
	if !bytes.Equal(cleanB, want) {
		t.Fatal("EVICTION_NOT_ACCEPTED: B refault changed clean content")
	}
	cacheResident("refaulted", true)
	if a[0] != 0xA5 || b[0] != source[0] || b[page] != 0x5A || a[page] != source[page] {
		t.Fatal("EVICTION_NOT_ACCEPTED: owned-device invalidation changed private COW pages")
	}
	t.Logf("EVICTION_VERIFIED clean_page=2 target_source_reads_before=%d target_source_reads_after=%d private_COW_preserved=true", before, after)
}

// mincore reports backing page-cache residency even when this mapping has no
// present PTE. Permission masking can force resident=true, which fails the cold
// assertion above rather than producing a false eviction acceptance.
func mappingCacheResident(mapping []byte) (bool, error) {
	var vector [1]byte
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&mapping[0])), uintptr(len(mapping)), uintptr(unsafe.Pointer(&vector[0])))
	runtime.KeepAlive(mapping)
	if errno != 0 {
		return false, errno
	}
	return vector[0]&1 != 0, nil
}

func pagemapPFN(mapping []byte, pageSize int) (uint64, error) {
	file, err := os.Open("/proc/self/pagemap")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	address := uintptr(unsafe.Pointer(&mapping[0]))
	var data [8]byte
	_, err = file.ReadAt(data[:], int64(address/uintptr(pageSize))*8)
	runtime.KeepAlive(mapping)
	if err != nil {
		return 0, err
	}
	entry := binary.LittleEndian.Uint64(data[:])
	if entry&(1<<63) == 0 {
		return 0, fmt.Errorf("page not present: %#x", entry)
	}
	pfn := entry & ((1 << 55) - 1)
	if pfn == 0 {
		return 0, errors.New("PFN masked (CAP_SYS_ADMIN required)")
	}
	return pfn, nil
}
