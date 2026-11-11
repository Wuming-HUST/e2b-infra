package nbd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Merovius/nbd/nbdnl"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/stresstest"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

const (
	connectTimeout = 30 * time.Second

	// disconnectTimeout should not be necessary if the disconnect is reliable
	disconnectTimeout = 30 * time.Second
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd")

type DirectPathMount struct {
	cancelfn     context.CancelFunc
	devicePool   *DevicePool
	featureFlags *featureflags.Client

	Backend     block.Device
	readOnly    bool
	deviceIndex uint32
	blockSize   uint64

	dispatchers []*Dispatch
	socksClient []*os.File
	socksServer []io.Closer

	handlersWg    sync.WaitGroup
	failureOnce   sync.Once
	failureDone   chan struct{}
	failureErr    error
	closing       atomic.Bool
	transportLost atomic.Bool
	closeOnce     sync.Once
	closeErr      error
}

var ErrTransportOwnershipUnknown = errors.New("NBD transport lost; device ownership unknown, backing quarantined")

type mountFailure struct {
	err           error
	transportLost bool
}

func NewDirectPathMount(b block.Device, devicePool *DevicePool, featureFlags *featureflags.Client) *DirectPathMount {
	return &DirectPathMount{
		Backend:      b,
		blockSize:    4096,
		devicePool:   devicePool,
		featureFlags: featureFlags,
		socksClient:  make([]*os.File, 0),
		socksServer:  make([]io.Closer, 0),
		deviceIndex:  math.MaxUint32,
		failureDone:  make(chan struct{}),
	}
}

// NewReadonlyDirectPathMount serves immutable RAM without allocating a writable
// cache. The registry owns the source; closing this mount never closes it.
func NewReadonlyDirectPathMount(b block.ReadonlyDevice, devicePool *DevicePool, featureFlags *featureflags.Client) *DirectPathMount {
	d := NewDirectPathMount(readonlyMountDevice{b}, devicePool, featureFlags)
	d.readOnly = true
	return d
}

type readonlyMountDevice struct{ block.ReadonlyDevice }

func (readonlyMountDevice) WriteAt([]byte, int64) (int, error) { return 0, syscall.EROFS }
func (readonlyMountDevice) Close() error                       { return nil }

// Done broadcasts unexpected service failure to every holder. Explicit Close
// is an orderly shutdown and does not signal failure. Err is safe after Done.
func (d *DirectPathMount) Done() <-chan struct{} { return d.failureDone }
func (d *DirectPathMount) Err() error {
	select {
	case <-d.failureDone:
		return d.failureErr
	default:
		return nil
	}
}
func (d *DirectPathMount) fail(err error) {
	d.failureOnce.Do(func() { d.failureErr = err; close(d.failureDone) })
}

func (d *DirectPathMount) serverFlags() nbdnl.ServerFlags {
	flags := nbdnl.FlagHasFlags | nbdnl.FlagCanMulticonn
	if d.readOnly {
		flags |= nbdnl.FlagReadOnly
	}
	return flags
}

func (d *DirectPathMount) Open(ctx context.Context) (retDeviceIndex uint32, err error) {
	serviceCtx := ctx
	if d.readOnly {
		// Caller cancellation aborts Open, but only Close may terminate an
		// established transport: its sockets keep the kernel device owned.
		serviceCtx, d.cancelfn = context.WithCancel(context.WithoutCancel(ctx))
	} else {
		ctx, d.cancelfn = context.WithCancel(ctx)
		serviceCtx = ctx
	}

	defer func() {
		// Set the device index to the one returned, correctly capture error values
		if err != nil && d.deviceIndex != math.MaxUint32 {
			// A connect can succeed before readiness/cancellation fails. Release
			// that actual device; IndexAny is never an owned device.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), disconnectTimeout)
			cleanupErr := d.Close(cleanupCtx)
			err = errors.Join(err, cleanupErr)
			cancel()
			if cleanupErr == nil {
				d.deviceIndex = math.MaxUint32
				d.socksClient = nil
				d.socksServer = nil
				d.dispatchers = nil
			}
		} else {
			d.deviceIndex = retDeviceIndex
		}
		logger.L().Debug(ctx, "opening direct path mount", zap.Uint32("device_index", d.deviceIndex), zap.Error(err))
	}()

	telemetry.ReportEvent(ctx, "opening direct path mount")

	size, err := d.Backend.Size(ctx)
	if err != nil {
		return math.MaxUint32, err
	}
	if d.readOnly && (size <= 0 || uint64(size)%d.blockSize != 0) {
		return math.MaxUint32, fmt.Errorf("NBD size %d must be positive and aligned to %d", size, d.blockSize)
	}

	telemetry.ReportEvent(ctx, "got backend size")

	deviceIndex := uint32(math.MaxUint32)

	for {
		deviceIndex, err = d.devicePool.GetDevice(ctx)
		if err != nil {
			return math.MaxUint32, err
		}

		telemetry.ReportEvent(ctx, "got device index")

		d.socksClient = make([]*os.File, 0)
		d.socksServer = make([]io.Closer, 0)
		d.dispatchers = make([]*Dispatch, 0)

		connections := d.featureFlags.IntFlag(ctx, featureflags.NBDConnectionsPerDevice)
		if connections <= 0 {
			releaseErr := d.devicePool.ReleaseDevice(ctx, deviceIndex)
			return math.MaxUint32, errors.Join(fmt.Errorf("NBD connections per device must be positive: %d", connections), releaseErr)
		}
		// A failed connect attempt closes its sockets as part of retry. Buffer
		// errors per attempt and publish them only after that attempt connects.
		attemptErrors := make(chan mountFailure, 2*connections)
		var attemptConnected atomic.Bool

		for i := range connections {
			// Create the socket pairs
			sockPair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err != nil {
				closeErr := closeSocketPairs(d.socksClient, d.socksServer)
				releaseErr := d.devicePool.ReleaseDevice(ctx, deviceIndex)

				return math.MaxUint32, errors.Join(err, closeErr, releaseErr)
			}

			client := os.NewFile(uintptr(sockPair[0]), "client")
			server := os.NewFile(uintptr(sockPair[1]), "server")
			serverc, err := net.FileConn(server)
			if err != nil {
				// Close the current iteration's FDs (not yet added to d.socksClient/d.socksServer)
				client.Close()
				server.Close()
				closeErr := closeSocketPairs(d.socksClient, d.socksServer)
				releaseErr := d.devicePool.ReleaseDevice(ctx, deviceIndex)

				return math.MaxUint32, errors.Join(err, closeErr, releaseErr)
			}
			server.Close()

			dispatch := NewDispatch(serverc, d.Backend)
			if d.readOnly {
				dispatch = NewReadonlyDispatch(serverc, d.Backend)
				dispatch.onFailure = func(serviceErr error) {
					select {
					case attemptErrors <- mountFailure{err: serviceErr}:
					default:
					}
				}
			}
			requestedDeviceIndex := deviceIndex
			// Start reading commands on the socket and dispatching them to our provider
			d.handlersWg.Go(func() {
				handleErr := dispatch.Handle(serviceCtx)
				if d.readOnly && !d.closing.Load() && serviceCtx.Err() == nil {
					if handleErr == nil {
						handleErr = errors.New("readonly NBD service disconnected")
					}
					select {
					case attemptErrors <- mountFailure{err: handleErr, transportLost: true}:
					default:
					}
					if attemptConnected.Load() {
						d.transportLost.Store(true)
					}
				}
				// The error is expected to happen if the nbd (socket connection) is closed
				logger.L().Info(ctx, "closing handler for NBD commands",
					zap.Error(handleErr),
					zap.Uint32("requested_device_index", requestedDeviceIndex),
					zap.Int("socket_index", i),
				)
			})

			d.socksServer = append(d.socksServer, serverc)
			d.socksClient = append(d.socksClient, client)
			d.dispatchers = append(d.dispatchers, dispatch)
		}

		var opts []nbdnl.ConnectOption
		opts = append(opts, nbdnl.WithBlockSize(d.blockSize))
		opts = append(opts, nbdnl.WithTimeout(stresstest.Timeout(connectTimeout)))
		opts = append(opts, nbdnl.WithDeadconnTimeout(connectTimeout))

		serverFlags := d.serverFlags()

		idx, connectErr := d.devicePool.connect(ctx, d, deviceIndex, uint64(size), serverFlags, opts...)
		if connectErr == nil {
			// The idx should be the same as deviceIndex, because we are connecting to it,
			// but we will use the one returned by nbdnl
			deviceIndex = idx
			d.deviceIndex = idx
			attemptConnected.Store(true)
			if d.readOnly {
				d.handlersWg.Go(func() {
					for {
						select {
						case serviceErr := <-attemptErrors:
							if !d.closing.Load() && serviceCtx.Err() == nil {
								if serviceErr.transportLost {
									d.transportLost.Store(true)
								}
								d.fail(serviceErr.err)
							}
						case <-serviceCtx.Done():
							return
						}
					}
				})
			}

			break
		}

		logger.L().Error(ctx, "error opening NBD, retrying", zap.Error(connectErr), zap.Uint32("device_index", deviceIndex))

		// Sometimes (rare), there seems to be a BADF error here. Lets just retry for now...
		// Close things down and try again...
		err := closeSocketPairs(d.socksClient, d.socksServer)
		if err != nil {
			logger.L().Error(ctx, "error closing socket pairs on error opening NBD", zap.Error(err))
		}

		// Release the device back to the pool
		err = d.devicePool.ReleaseDevice(ctx, deviceIndex)
		if err != nil {
			logger.L().Error(ctx, "error opening NBD, error releasing device", zap.Error(err), zap.Uint32("device_index", deviceIndex))
		}

		if d.devicePool.dynamic {
			return math.MaxUint32, fmt.Errorf("dynamic NBD connect: %w", connectErr)
		}

		if strings.Contains(connectErr.Error(), "invalid argument") {
			return math.MaxUint32, connectErr
		}

		select {
		case <-ctx.Done():
			return math.MaxUint32, errors.Join(connectErr, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}

	// Wait until it's connected...
	for {
		select {
		case <-ctx.Done():
			return math.MaxUint32, ctx.Err()
		case <-d.failureDone:
			return math.MaxUint32, d.Err()
		default:
		}

		telemetry.ReportEvent(ctx, "waiting for NBD connection")

		s, err := nbdnl.Status(deviceIndex)
		if err == nil && s.Connected {
			break
		}

		time.Sleep(100 * time.Nanosecond)
	}

	if d.devicePool.dynamic {
		if err := ensureDevicePath(deviceIndex); err != nil {
			return math.MaxUint32, err
		}
	}
	if d.readOnly {
		if err := validateReadonlyReady(GetDevicePath(deviceIndex), size); err != nil {
			return math.MaxUint32, err
		}
		if err := d.Err(); err != nil {
			return math.MaxUint32, err
		}
	}

	telemetry.ReportEvent(ctx, "connected to NBD")

	return deviceIndex, nil
}

func (d *DirectPathMount) Close(ctx context.Context) error {
	d.closing.Store(true)
	d.closeOnce.Do(func() { d.closeErr = d.close(ctx) })
	return d.closeErr
}

func (d *DirectPathMount) close(ctx context.Context) error {
	if d.readOnly && d.transportLost.Load() {
		// A stale number is not authority to disconnect a possibly reused
		// kernel device. Keep the source, sockets and pool entry quarantined.
		return ErrTransportOwnershipUnknown
	}
	ctx, span := tracer.Start(ctx, "direct-path-mount-close")
	defer span.End()

	var errs []error

	idx := d.deviceIndex

	// Dynamic IDs can be reused as soon as a socket disconnects. Send the
	// disconnect while this mount still owns its sockets, before closing them;
	// never issue another command for the ID after kernel acknowledgement.
	if idx != math.MaxUint32 && (d.devicePool.dynamic || d.readOnly) {
		if d.readOnly {
			if err := d.checkReadonlyTransport(); err != nil {
				d.transportLost.Store(true)
				return errors.Join(ErrTransportOwnershipUnknown, err)
			}
		}
		if d.devicePool.dynamic {
			if err := d.devicePool.disconnect(d, idx); err != nil {
				return err
			}
		} else if err := disconnectNBDWithTimeout(ctx, idx, disconnectTimeout); err != nil {
			return err
		}
	}

	// First cancel the context, which will stop waiting on pending readAt/writeAt...
	telemetry.ReportEvent(ctx, "canceling context")
	if d.cancelfn != nil {
		d.cancelfn()
	}

	// Close all server socket pairs...
	telemetry.ReportEvent(ctx, "closing socket pairs server")
	for _, v := range d.socksServer {
		err := v.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, fmt.Errorf("error closing server pair: %w", err))
		}
	}

	// Now wait until the handlers return
	telemetry.ReportEvent(ctx, "await handlers return")
	d.handlersWg.Wait()

	// Now wait for any pending responses to be sent
	telemetry.ReportEvent(ctx, "waiting for pending responses")
	for _, d := range d.dispatchers {
		d.Drain()
	}

	// Static allocation reserves the ID until ReleaseDevice.
	if d.devicePool != nil && !d.devicePool.dynamic && !d.readOnly && idx != math.MaxUint32 {
		err := disconnectNBDWithTimeout(ctx, idx, disconnectTimeout)
		if err != nil {
			errs = append(errs, fmt.Errorf("error disconnecting NBD: %w", err))
		}
	}

	// Close all client socket pairs...
	telemetry.ReportEvent(ctx, "closing socket pairs client")
	for _, v := range d.socksClient {
		err := v.Close()
		if err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, fmt.Errorf("error closing socket pair client: %w", err))
		}
	}

	// Release the device back to the pool, retry if it is in use
	if d.devicePool != nil && idx != math.MaxUint32 {
		telemetry.ReportEvent(ctx, "releasing device to the pool")
		err := d.devicePool.ReleaseDevice(ctx, idx, WithInfiniteRetry())
		if err != nil {
			errs = append(errs, fmt.Errorf("error releasing overlay device: %w", err))
		}
	}

	return errors.Join(errs...)
}

func disconnectNBDWithTimeout(ctx context.Context, deviceIndex uint32, timeout time.Duration) error {
	// Now ask to disconnect
	telemetry.ReportEvent(ctx, "disconnecting NBD")
	err := nbdnl.Disconnect(deviceIndex)
	if err != nil {
		return err
	}

	// Wait until it's completely disconnected...
	telemetry.ReportEvent(ctx, "waiting for complete disconnection")
	ctxTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		select {
		case <-ctxTimeout.Done():
			return ctxTimeout.Err()
		default:
		}

		s, err := nbdnl.Status(deviceIndex)
		if err == nil && !s.Connected {
			break
		}
		time.Sleep(100 * time.Nanosecond)
	}

	return nil
}

func closeSocketPairs(socksClient []*os.File, socksServer []io.Closer) error {
	var errs []error
	for _, sock := range socksClient {
		if sock != nil {
			errs = append(errs, sock.Close())
		}
	}
	for _, sock := range socksServer {
		if sock != nil {
			errs = append(errs, sock.Close())
		}
	}

	return errors.Join(errs...)
}
