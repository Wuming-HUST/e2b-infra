package userfaultfd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/fdexit"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/memory"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/userfaultfd")

const maxRequestsInProgress = 4096

// Address-space changes can outlive a fixed retry budget under FPR pressure.
// Retry with bounded backoff until the caller or handler stops, releasing all
// event-state locks before waiting so REMOVE can always make progress.
var (
	ErrUnexpectedEventType = errors.New("unexpected event type")
	ErrClosed              = errors.New("userfaultfd closed")
	ErrPrefaultDiscarded   = errors.New("prefault skipped: guest released the page")
)

func copyWithEagainRetry(ctx context.Context, copy func() error) error {
	delay := 50 * time.Microsecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := copy()
		if !errors.Is(err, unix.EAGAIN) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 1600*time.Microsecond)
	}
}

// hasEvent checks if a specific poll event flag is set in revents.
func hasEvent(revents, event int16) bool {
	return revents&event != 0
}

type Userfaultfd struct {
	fd Fd

	src block.Slicer
	ma  *memory.Mapping

	// We don't skip the already mapped pages, because if the memory is swappable the page *might* under some conditions be mapped out.
	// For hugepages this should not be a problem, but might theoretically happen to normal pages with swap
	missingRequests *block.Tracker
	// We use the settleRequests to guard the missingRequests so we can access a consistent state of the missingRequests after the requests are finished.
	settleRequests sync.RWMutex

	prefetchTracker *block.PrefetchTracker
	removedRanges   removedPageRanges

	wg errgroup.Group

	logger    logger.Logger
	closeOnce sync.Once
	closeErr  error
}

// NewUserfaultfdFromFd creates a new userfaultfd instance with optional configuration.
func NewUserfaultfdFromFd(fd uintptr, src block.Slicer, m *memory.Mapping, logger logger.Logger) (*Userfaultfd, error) {
	blockSize := src.BlockSize()

	for _, region := range m.Regions {
		if region.PageSize != uintptr(blockSize) {
			return nil, fmt.Errorf("block size mismatch: %d != %d for region %d", region.PageSize, blockSize, region.BaseHostVirtAddr)
		}
	}

	u := &Userfaultfd{
		fd:              Fd(fd),
		src:             src,
		missingRequests: block.NewTracker(blockSize),
		prefetchTracker: block.NewPrefetchTracker(blockSize),
		ma:              m,
		logger:          logger,
	}

	// By default this was unlimited.
	// Now that we don't skip previously faulted pages we add at least some boundaries to the concurrency.
	// Also, in some brief tests, adding a limit actually improved the handling at high concurrency.
	u.wg.SetLimit(maxRequestsInProgress)

	return u, nil
}

func (u *Userfaultfd) Close() error {
	u.closeOnce.Do(func() {
		u.removedRanges.mu.Lock()
		defer u.removedRanges.mu.Unlock()
		u.removedRanges.closed = true
		u.closeErr = u.fd.close()
	})
	return u.closeErr
}

func (u *Userfaultfd) Serve(
	ctx context.Context,
	fdExit *fdexit.FdExit,
) (serveErr error) {
	ctx, cancel := context.WithCancel(ctx)
	stopWake := context.AfterFunc(ctx, func() { _ = fdExit.SignalExit() })
	defer func() {
		cancel()
		stopWake()
		serveErr = errors.Join(serveErr, u.wg.Wait())
	}()
	var pending []func() error
	pollFds := []unix.PollFd{
		{Fd: int32(u.fd), Events: unix.POLLIN},
		{Fd: fdExit.Reader(), Events: unix.POLLIN},
	}

	eagainCounter := newCounterReporter(u.logger, "uffd: eagain during fd read (accumulated)")
	defer eagainCounter.Close(ctx)

	noDataCounter := newCounterReporter(u.logger, "uffd: no data in fd (accumulated)")
	defer noDataCounter.Close(ctx)

	exitFdErrorCounter := newCounterReporter(u.logger, "uffd: exit fd poll errors (accumulated)")
	defer exitFdErrorCounter.Close(ctx)

	uffdErrorCounter := newCounterReporter(u.logger, "uffd: uffd fd poll errors (accumulated)")
	defer uffdErrorCounter.Close(ctx)

	pollErrorEvents := map[int16]string{
		unix.POLLHUP:  "POLLHUP",
		unix.POLLERR:  "POLLERR",
		unix.POLLNVAL: "POLLNVAL",
	}

outerLoop:
	for {
		// A saturated worker pool must not stop the reader acknowledging FPR.
		for len(pending) > 0 && u.wg.TryGo(pending[0]) {
			pending[0] = nil
			pending = pending[1:]
		}
		pollTimeout := -1
		if len(pending) > 0 {
			pollTimeout = 1
		}
		if _, err := unix.Poll(
			pollFds,
			pollTimeout,
		); err != nil {
			if err == unix.EINTR {
				u.logger.Debug(ctx, "uffd: interrupted polling, going back to polling")

				continue
			}

			if err == unix.EAGAIN {
				u.logger.Debug(ctx, "uffd: eagain during polling, going back to polling")

				continue
			}

			u.logger.Error(ctx, "UFFD serve polling error", zap.Error(err))

			return fmt.Errorf("failed polling: %w", err)
		}

		exitFd := pollFds[1]
		if hasEvent(exitFd.Revents, unix.POLLIN) {
			return nil
		}

		// Track exit fd error events
		for event, name := range pollErrorEvents {
			if hasEvent(exitFd.Revents, event) {
				exitFdErrorCounter.Increase(name)
			}
		}

		uffdFd := pollFds[0]

		// Track uffd error events
		for event, name := range pollErrorEvents {
			if hasEvent(uffdFd.Revents, event) {
				uffdErrorCounter.Increase(name)
			}
		}

		if !hasEvent(uffdFd.Revents, unix.POLLIN) {
			// Uffd is not ready for reading as there is nothing to read on the fd.
			// https://github.com/firecracker-microvm/firecracker/issues/5056
			// https://elixir.bootlin.com/linux/v6.8.12/source/fs/userfaultfd.c#L1149
			// TODO: Check for all the errors
			// - https://docs.kernel.org/admin-guide/mm/userfaultfd.html
			// - https://elixir.bootlin.com/linux/v6.8.12/source/fs/userfaultfd.c
			// - https://man7.org/linux/man-pages/man2/userfaultfd.2.html
			// It might be possible to just check for data != 0 in the syscall.Read loop
			// but I don't feel confident about doing that.
			noDataCounter.Increase("POLLIN")

			continue
		}

		msg, err := u.readEvent()
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) {
			eagainCounter.Increase("EAGAIN")
			continue outerLoop
		}
		if err != nil {
			return fmt.Errorf("read UFFD event: %w", err)
		}
		if getMsgEvent(&msg) == UFFD_EVENT_REMOVE {
			continue
		}

		arg := getMsgArg(&msg)
		pagefault := (*(*UffdPagefault)(unsafe.Pointer(&arg[0])))
		flags := pagefault.flags

		addr := getPagefaultAddress(&pagefault)

		offset, pagesize, err := u.ma.GetOffset(addr)
		if err != nil {
			u.logger.Error(ctx, "UFFD serve get mapping error", zap.Error(err))

			return fmt.Errorf("failed to map: %w", err)
		}

		// Handle write to missing page (WRITE flag)
		// If the event has WRITE flag, it was a write to a missing page.
		// For the write to be executed, we first need to copy the page from the source to the guest memory.
		if flags == UFFD_PAGEFAULT_FLAG_WRITE {
			pending = append(pending, func() error {
				return u.faultPage(ctx, addr, offset, pagesize, u.src, fdExit.SignalExit, block.Write)
			})

			continue
		}

		// Handle read to missing page ("MISSING" flag)
		// If the event has no flags, it was a read to a missing page and we need to copy the page from the source to the guest memory.
		if flags == 0 {
			pending = append(pending, func() error {
				return u.faultPage(ctx, addr, offset, pagesize, u.src, fdExit.SignalExit, block.Read)
			})

			continue
		}

		// MINOR and WP flags are not expected as we don't register the uffd with these flags.
		return fmt.Errorf("unexpected event type: %d, closing uffd", flags)
	}
}

// readEvent pairs the nonblocking read (which acknowledges REMOVE to the
// kernel) with source invalidation. Installs cannot slip into that interval.
// Source I/O and EAGAIN backoff never hold this lock.
func (u *Userfaultfd) readEvent() (UffdMsg, error) {
	u.removedRanges.mu.Lock()
	defer u.removedRanges.mu.Unlock()
	var msg UffdMsg
	if u.removedRanges.closed {
		return msg, ErrClosed
	}
	buf := make([]byte, unsafe.Sizeof(msg))
	n, err := syscall.Read(int(u.fd), buf)
	if err != nil {
		return msg, err
	}
	if n != len(buf) {
		return msg, fmt.Errorf("short UFFD event: %d bytes", n)
	}
	msg = *(*UffdMsg)(unsafe.Pointer(&buf[0]))
	switch event := getMsgEvent(&msg); event {
	case UFFD_EVENT_PAGEFAULT:
		return msg, nil
	case UFFD_EVENT_REMOVE:
		rng := getRemoveRange(&msg)
		start, end := uintptr(rng.Start), uintptr(rng.End)
		if end <= start {
			return msg, fmt.Errorf("invalid REMOVE range [%x, %x)", start, end)
		}
		for cursor := start; cursor < end; {
			matched := false
			for _, region := range u.ma.Regions {
				regionEnd := region.BaseHostVirtAddr + region.Size
				if cursor < region.BaseHostVirtAddr || cursor >= regionEnd {
					continue
				}
				next := min(end, regionEnd)
				if cursor%region.PageSize != 0 || next%region.PageSize != 0 {
					return msg, fmt.Errorf("unaligned REMOVE range [%x, %x)", cursor, next)
				}
				cursor = next
				matched = true
				break
			}
			if !matched {
				return msg, fmt.Errorf("REMOVE address %x outside guest mapping", cursor)
			}
		}
		u.removedRanges.addLocked(start, end)
		return msg, nil
	default:
		// REMAP has from/to/len, not start/end. Firecracker registers only
		// REMOVE and uses a stable mapping. Stop before any future prefault
		// can target an unmapped or repurposed address.
		u.removedRanges.closed = true
		return msg, fmt.Errorf("%w: %d", ErrUnexpectedEventType, event)
	}
}

func (u *Userfaultfd) faulted() *block.Tracker {
	// This will be at worst cancelled when the uffd is closed.
	u.settleRequests.Lock()
	// The locking here would work even without using defer (just lock-then-unlock the mutex), but at this point let's make it lock to the clone,
	// so it is consistent even if there is a another uffd call after.
	defer u.settleRequests.Unlock()

	return u.missingRequests.Clone()
}

func (u *Userfaultfd) PrefetchData() block.PrefetchData {
	// This will be at worst cancelled when the uffd is closed.
	u.settleRequests.Lock()
	// The locking here would work even without using defer (just lock-then-unlock the mutex), but at this point let's make it lock to the clone,
	// so it is consistent even if there is a another uffd call after.
	defer u.settleRequests.Unlock()

	return u.prefetchTracker.PrefetchData()
}

// RemovedOffsets must be called after pausing the VM and taking its snapshot.
// Wait for queued REMOVE acknowledgements to be reflected in our state. All
// ever-discarded pages must be exported in the next diff: they may now contain
// zeros OR newer guest writes, so marking them all Empty would lose data.
func (u *Userfaultfd) RemovedOffsets(ctx context.Context) (*block.Tracker, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		u.removedRanges.mu.RLock()
		if u.removedRanges.closed {
			u.removedRanges.mu.RUnlock()
			return nil, ErrClosed
		}
		poll := []unix.PollFd{{Fd: int32(u.fd), Events: unix.POLLIN}}
		_, err := unix.Poll(poll, 0)
		if err != nil {
			u.removedRanges.mu.RUnlock()
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, err
		}
		if !hasEvent(poll[0].Revents, unix.POLLIN) {
			removed := block.NewTracker(u.src.BlockSize())
			for _, r := range u.removedRanges.ranges {
				for addr := r.start; addr < r.end; {
					offset, size, err := u.ma.GetOffset(addr)
					if err != nil {
						u.removedRanges.mu.RUnlock()
						return nil, err
					}
					removed.Add(offset)
					addr += size
				}
			}
			u.removedRanges.mu.RUnlock()
			return removed, nil
		}
		u.removedRanges.mu.RUnlock()
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Prefault proactively copies a page to guest memory at the given offset.
// This is used to speed up sandbox starts by prefetching pages that are known to be needed.
// Returns nil on success, or if the page is already mapped (EEXIST is handled gracefully).
func (u *Userfaultfd) Prefault(ctx context.Context, offset int64, data []byte) error {
	ctx, span := tracer.Start(ctx, "prefault page")
	defer span.End()

	// Get host virtual address and page size for this offset
	addr, pagesize, err := u.ma.GetHostVirtAddr(offset)
	if err != nil {
		return fmt.Errorf("failed to get host virtual address: %w", err)
	}

	if len(data) != int(pagesize) {
		return fmt.Errorf("data length (%d) is less than pagesize (%d)", len(data), pagesize)
	}

	return u.faultPage(ctx, addr, offset, pagesize, directDataSource{data, int64(pagesize)}, nil, block.Prefetch)
}

// directDataSource wraps a byte slice to implement block.Slicer for prefaulting.
type directDataSource struct {
	data     []byte
	pagesize int64
}

func (d directDataSource) Slice(_ context.Context, _, _ int64) ([]byte, error) {
	return d.data, nil
}

func (d directDataSource) BlockSize() int64 {
	return d.pagesize
}

func (u *Userfaultfd) faultPage(
	ctx context.Context,
	addr uintptr,
	offset int64,
	pagesize uintptr,
	source block.Slicer,
	onFailure func() error,
	accessType block.AccessType,
) error {
	span := trace.SpanFromContext(ctx)

	// The RLock must be called inside the goroutine to ensure RUnlock runs via defer,
	// even if the errgroup is cancelled or the goroutine returns early.
	// This check protects us against race condition between marking the request as missing and accessing the missingRequests tracker.
	// The Firecracker pause should return only after the requested memory is faulted in, so we don't need to guard the pagefault from the moment it is created.
	u.settleRequests.RLock()
	defer u.settleRequests.RUnlock()

	defer func() {
		if r := recover(); r != nil {
			u.logger.Error(ctx, "UFFD serve panic", zap.Any("pagesize", pagesize), zap.Any("panic", r))
		}
	}()

	var b []byte
	var dataErr error
	if !u.removedRanges.contains(addr) {
		b, dataErr = source.Slice(ctx, offset, int64(pagesize))
	}
	// A discard also invalidates a failed in-flight source fetch.
	if dataErr != nil && !u.removedRanges.contains(addr) {
		var signalErr error
		if onFailure != nil {
			signalErr = onFailure()
		}

		joinedErr := errors.Join(dataErr, signalErr)

		span.RecordError(joinedErr)
		u.logger.Error(ctx, "UFFD serve data fetch error", zap.Error(joinedErr))

		return fmt.Errorf("failed to read from source: %w", joinedErr)
	}

	var copyMode CULong

	// Performing copy() on UFFD clears the WP bit unless we explicitly tell
	// it not to. We do that for faults caused by a read access. Write accesses
	// would anyways cause clear the write-protection bit.
	//if accessType != block.Write {
	//	copyMode |= UFFDIO_COPY_MODE_WP
	//}

	copyErr := copyWithEagainRetry(ctx, func() error {
		// Hold the read lock across the ioctl. REMOVE takes the write lock, so
		// an event cannot invalidate a page between the state check and copy.
		return u.removedRanges.with(addr, func(removed bool) error {
			if removed {
				if accessType == block.Prefetch {
					return ErrPrefaultDiscarded
				}
				if pagesize == header.HugepageSize {
					return u.fd.copy(addr, pagesize, header.EmptyHugePage, copyMode)
				}
				return u.fd.zero(addr, pagesize)
			}

			return u.fd.copy(addr, pagesize, b, copyMode)
		})
	})
	if errors.Is(copyErr, unix.EEXIST) {
		// Page is already mapped
		span.SetAttributes(attribute.Bool("uffd.already_mapped", true))

		return nil
	}

	if errors.Is(copyErr, ErrPrefaultDiscarded) {
		return copyErr
	}
	if copyErr != nil {
		if ctx.Err() != nil {
			return nil
		}
		var signalErr error
		if onFailure != nil {
			signalErr = onFailure()
		}

		joinedErr := errors.Join(copyErr, signalErr)

		span.RecordError(joinedErr)
		u.logger.Error(ctx, "UFFD serve uffdio copy error", zap.Error(joinedErr))

		return fmt.Errorf("failed uffdio copy %w", joinedErr)
	}

	// Add the offset to the missing requests tracker with metadata.
	u.missingRequests.Add(offset)
	u.prefetchTracker.Add(offset, accessType)

	return nil
}
