package userfaultfd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/fdexit"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/memory"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/testutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// /dev/userfaultfd can be delegated to a container without enabling the
// host-wide unprivileged_userfaultfd sysctl or allowing its syscall in seccomp.
func newRemoveTestFd() (Fd, error) {
	dev, err := os.OpenFile("/dev/userfaultfd", os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer dev.Close()
	fd, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dev.Fd(), 0xAA00, syscall.O_CLOEXEC|syscall.O_NONBLOCK)
	if errno != 0 {
		return 0, errno
	}
	return Fd(fd), nil
}

// A never-faulted discard is absent from the fault tracker and KVM dirty log.
// It must nevertheless override the parent snapshot in the next diff.
func TestRemovedOffsetsIncludeUnfaultedAndRewrittenPages(t *testing.T) {
	h, err := configureCrossProcessTest(t, testConfig{
		pagesize: header.PageSize, numberOfPages: 4,
		removeEnabled: true, collectRemoved: true,
	})
	require.NoError(t, err)
	mem := *h.memoryArea
	page := int(header.PageSize)
	require.NoError(t, h.executeRead(t.Context(), operation{offset: 0, mode: operationModeRead}))
	require.NoError(t, unix.Madvise(mem[:3*page], unix.MADV_DONTNEED))
	mem[13] = 0xAB // page 0 now has a new value; pages 1 and 2 remain missing.
	offsets, err := h.offsetsOnce()
	require.NoError(t, err)
	require.Equal(t, []uint{0, uint(page), uint(2 * page)}, offsets)
	require.Equal(t, byte(0xAB), mem[13])
}

type gatedRemoveSource struct {
	started chan struct{}
	release chan struct{}
	data    []byte
}

func (s *gatedRemoveSource) BlockSize() int64 { return header.PageSize }
func (s *gatedRemoveSource) Slice(ctx context.Context, _, _ int64) ([]byte, error) {
	close(s.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return s.data, nil
	}
}

// Hold a real source fetch across REMOVE, then complete the original install.
// No missing mmap is read by Go until the explicit demand install has finished.
func TestRemoveDuringSourceFetch(t *testing.T) {
	for _, access := range []block.AccessType{block.Read, block.Prefetch} {
		t.Run(fmt.Sprint(access), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			mem, start, err := testutils.NewPageMmap(t, uint64(header.PageSize), uint64(header.PageSize))
			require.NoError(t, err)
			fd, err := newRemoveTestFd()
			require.NoError(t, err)
			t.Cleanup(func() { _ = fd.close() })
			require.NoError(t, configureRemoveTestAPI(fd))
			require.NoError(t, register(fd, start, uint64(len(mem)), CULong(UFFDIO_REGISTER_MODE_MISSING)))
			source := &gatedRemoveSource{
				started: make(chan struct{}), release: make(chan struct{}),
				data: bytes.Repeat([]byte{0xCD}, len(mem)),
			}
			l, err := logger.NewDevelopmentLogger()
			require.NoError(t, err)
			u, err := NewUserfaultfdFromFd(uintptr(fd), source, memory.NewMapping([]memory.Region{{
				BaseHostVirtAddr: start, Size: uintptr(len(mem)), PageSize: uintptr(header.PageSize),
			}}), l)
			require.NoError(t, err)
			exit, err := fdexit.New()
			require.NoError(t, err)
			defer exit.Close()
			serveDone := make(chan error, 1)
			go func() { serveDone <- u.Serve(ctx, exit) }()
			defer func() { cancel(); require.NoError(t, <-serveDone) }()
			copyDone := make(chan error, 1)
			go func() { copyDone <- u.faultPage(ctx, start, 0, uintptr(len(mem)), source, nil, access) }()
			select {
			case <-source.started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.NoError(t, unix.Madvise(mem, unix.MADV_DONTNEED))
			removed, err := u.RemovedOffsets(ctx)
			require.NoError(t, err)
			require.True(t, removed.Has(0))
			close(source.release)
			err = <-copyDone
			if access == block.Prefetch {
				require.ErrorIs(t, err, ErrPrefaultDiscarded)
				// Reading a removed page must skip the stale source entirely.
				err = u.faultPage(ctx, start, 0, uintptr(len(mem)), source, nil, block.Read)
			}
			require.NoError(t, err)
			require.Equal(t, make([]byte, len(mem)), mem)
		})
	}
}

func configureRemoveTestAPI(fd Fd) error {
	api := newUffdioAPI(UFFD_API, UFFD_FEATURE_EVENT_REMOVE)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), UFFDIO_API, uintptr(unsafe.Pointer(&api)))
	if errno != 0 {
		return fmt.Errorf("configure REMOVE API: %w", errno)
	}
	return nil
}

func TestRemoveRefaultZero(t *testing.T) {
	for _, prefault := range []bool{false, true} {
		t.Run(fmt.Sprintf("prefetch=%t", prefault), func(t *testing.T) {
			h, err := configureCrossProcessTest(t, testConfig{
				pagesize: header.PageSize, numberOfPages: 4,
				removeEnabled: true, prefetchDuringRemove: prefault,
			})
			require.NoError(t, err)
			page := int(header.PageSize)
			mem := *h.memoryArea
			require.NoError(t, h.executeRead(t.Context(), operation{offset: 0, mode: operationModeRead}))
			zeros := make([]byte, page*2)
			for iteration := 0; iteration < 32; iteration++ {
				// Include both a resident page and an initially missing page.
				require.NoError(t, unix.Madvise(mem[:2*page], unix.MADV_DONTNEED))
				require.True(t, bytes.Equal(mem[:2*page], zeros), "REMOVE resurrected snapshot bytes at iteration %d", iteration)
				mem[13] = 0xAB
				require.Equal(t, byte(0xAB), mem[13])
				require.True(t, bytes.Equal(mem[14:page], zeros[14:page]), "partial write must leave the rest zero")
				require.NoError(t, h.executeRead(t.Context(), operation{offset: int64(3 * page), mode: operationModeRead}))
			}
		})
	}
}
