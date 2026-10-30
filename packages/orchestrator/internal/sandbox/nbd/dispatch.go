package nbd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"syscall"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

var ErrShuttingDown = errors.New("shutting down. Cannot serve any new requests")

type Provider interface {
	storage.SeekableReader
	io.WriterAt
}

const (
	// Bound both each allocation and the number of operations retaining buffers.
	dispatchMaxWriteBufferSize = 32 * 1024 * 1024
	dispatchMaxInFlight        = 4
	dispatchIOTimeout          = 30 * time.Second
)

const (
	NBDCmdRead        = 0
	NBDCmdWrite       = 1
	NBDCmdDisconnect  = 2
	NBDCmdFlush       = 3
	NBDCmdTrim        = 4
	NBDCmdWriteZeroes = 6
)

const (
	NBDRequestMagic  = 0x25609513
	NBDResponseMagic = 0x67446698
)

type Request struct {
	Magic  uint32
	Type   uint32
	Handle uint64
	From   uint64
	Length uint32
}

type Response struct {
	Magic  uint32
	Error  uint32
	Handle uint64
}

type Dispatch struct {
	fp               io.ReadWriter
	writeLock        sync.Mutex
	prov             storage.SeekableReader
	writer           io.WriterAt
	readOnly         bool
	pendingResponses sync.WaitGroup
	shuttingDown     bool
	shuttingDownLock sync.Mutex
	slots            chan struct{}
	fatal            chan error
	failureOnce      sync.Once
	failureDone      chan struct{}
	failureErr       error
	onFailure        func(error)
}

func NewDispatch(fp io.ReadWriter, prov Provider) *Dispatch {
	d := NewReadonlyDispatch(fp, prov)
	d.readOnly = false
	d.writer = prov
	return d
}

// NewReadonlyDispatch never calls a mutating method of the source. It does not
// own or close the source; its lifetime belongs to the backing registry.
func NewReadonlyDispatch(fp io.ReadWriter, prov storage.SeekableReader) *Dispatch {
	return &Dispatch{fp: fp, prov: prov, readOnly: true, slots: make(chan struct{}, dispatchMaxInFlight), fatal: make(chan error, 1), failureDone: make(chan struct{})}
}

func (d *Dispatch) Done() <-chan struct{} { return d.failureDone }
func (d *Dispatch) Err() error {
	select {
	case <-d.failureDone:
		return d.failureErr
	default:
		return nil
	}
}
func (d *Dispatch) recordFailure(err error) {
	d.failureOnce.Do(func() {
		d.failureErr = err
		close(d.failureDone)
		if d.onFailure != nil {
			d.onFailure(err)
		}
	})
}

func (d *Dispatch) Drain() {
	d.shuttingDownLock.Lock()
	d.shuttingDown = true
	d.shuttingDownLock.Unlock()
	d.pendingResponses.Wait()
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n < 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (d *Dispatch) writeResponse(respError uint32, respHandle uint64, chunk []byte) error {
	d.writeLock.Lock()
	defer d.writeLock.Unlock()
	if conn, ok := d.fp.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := conn.SetWriteDeadline(time.Now().Add(dispatchIOTimeout)); err != nil {
			return err
		}
		defer conn.SetWriteDeadline(time.Time{})
	}
	var header [16]byte
	binary.BigEndian.PutUint32(header[:], NBDResponseMagic)
	binary.BigEndian.PutUint32(header[4:], respError)
	binary.BigEndian.PutUint64(header[8:], respHandle)
	if err := writeFull(d.fp, header[:]); err != nil {
		return err
	}
	return writeFull(d.fp, chunk)
}

func (d *Dispatch) fail(err error) {
	d.recordFailure(err)
	select {
	case d.fatal <- err:
	default:
	}
	// Wake a parser blocked in Read. The mount also closes sockets on shutdown.
	if c, ok := d.fp.(io.Closer); ok {
		_ = c.Close()
	}
}

func (d *Dispatch) Handle(ctx context.Context) error {
	// Network reads and writes must wake when the owner cancels an idle mount.
	stop := context.AfterFunc(ctx, func() {
		if c, ok := d.fp.(io.Closer); ok {
			_ = c.Close()
		}
	})
	defer func() {
		stop()
		// Cancellation closes ctx.Done before walking its registered children.
		// The parser can observe that cancellation and return while AfterFunc
		// is still waiting to run; stopping it here would leave response writers
		// blocked on the socket until their I/O deadline. Close synchronously so
		// canceled Handle returning also guarantees its transport is awake.
		if ctx.Err() != nil {
			if c, ok := d.fp.(io.Closer); ok {
				_ = c.Close()
			}
		}
	}()
	var header [28]byte
	for {
		if _, err := io.ReadFull(d.fp, header[:]); err != nil {
			select {
			case fatal := <-d.fatal:
				return fatal
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		r := Request{Magic: binary.BigEndian.Uint32(header[:]), Type: binary.BigEndian.Uint32(header[4:]), Handle: binary.BigEndian.Uint64(header[8:]), From: binary.BigEndian.Uint64(header[16:]), Length: binary.BigEndian.Uint32(header[24:])}
		if r.Magic != NBDRequestMagic {
			return fmt.Errorf("received invalid NBD magic: %#x", r.Magic)
		}
		// Flags occupy the upper 16 bits, including FUA on WRITE. They cannot
		// bypass the readonly check by changing the complete type value.
		command := r.Type & 0xffff
		switch command {
		case NBDCmdDisconnect:
			return nil
		case NBDCmdRead:
			if err := d.cmdRead(ctx, r.Handle, r.From, r.Length); err != nil {
				return err
			}
		case NBDCmdWrite:
			if r.Length > dispatchMaxWriteBufferSize {
				return fmt.Errorf("nbd write request length %d exceeds maximum %d", r.Length, dispatchMaxWriteBufferSize)
			}
			if d.readOnly {
				// Consume the payload without allocating it, so later commands
				// remain framed correctly even for a rejected flagged WRITE.
				if _, err := io.CopyN(io.Discard, d.fp, int64(r.Length)); err != nil {
					return err
				}
				if err := d.writeResponse(uint32(syscall.EROFS), r.Handle, nil); err != nil {
					return err
				}
				continue
			}
			if err := d.admit(ctx); err != nil {
				return err
			}
			data := make([]byte, r.Length)
			if _, err := io.ReadFull(d.fp, data); err != nil {
				d.complete()
				return err
			}
			d.writeAdmitted(ctx, r.Handle, r.From, data)
		case NBDCmdTrim:
			if err := d.cmdTrim(r.Handle, r.From, r.Length); err != nil {
				return err
			}
		case NBDCmdWriteZeroes:
			errno := uint32(syscall.EOPNOTSUPP)
			if d.readOnly {
				errno = uint32(syscall.EROFS)
			}
			if err := d.writeResponse(errno, r.Handle, nil); err != nil {
				return err
			}
		case NBDCmdFlush:
			if err := d.writeResponse(uint32(syscall.EOPNOTSUPP), r.Handle, nil); err != nil {
				return err
			}
		default:
			return fmt.Errorf("nbd not implemented %d", r.Type)
		}
	}
}

func (d *Dispatch) admit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case err := <-d.fatal:
		return err
	}
	d.shuttingDownLock.Lock()
	defer d.shuttingDownLock.Unlock()
	if err := ctx.Err(); err != nil {
		<-d.slots
		return err
	}
	if d.shuttingDown {
		<-d.slots
		return ErrShuttingDown
	}
	d.pendingResponses.Add(1)
	return nil
}

func (d *Dispatch) complete() { <-d.slots; d.pendingResponses.Done() }

func (d *Dispatch) checkRange(ctx context.Context, from uint64, length uint32) error {
	if from > math.MaxInt64 || uint64(length) > uint64(math.MaxInt64)-from {
		return fmt.Errorf("NBD offset overflow")
	}
	size, err := d.prov.Size(ctx)
	if err != nil {
		return err
	}
	if size < 0 || from > uint64(size) || uint64(length) > uint64(size)-from {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (d *Dispatch) cmdRead(ctx context.Context, handle uint64, from uint64, length uint32) error {
	if length > dispatchMaxWriteBufferSize {
		return fmt.Errorf("nbd read request length %d exceeds maximum %d", length, dispatchMaxWriteBufferSize)
	}
	if err := d.admit(ctx); err != nil {
		return err
	}
	go func() {
		defer d.complete()
		readCtx, cancel := context.WithTimeout(ctx, dispatchIOTimeout)
		defer cancel()
		var err error
		if d.readOnly {
			err = d.Err()
		}
		if err == nil {
			err = d.checkRange(readCtx, from, length)
		}
		if err == nil {
			err = readCtx.Err()
		}
		var data []byte
		if err == nil {
			data = make([]byte, length)
			n, readErr := d.prov.ReadAt(readCtx, data, int64(from))
			err = readErr
			if n != len(data) {
				err = errors.Join(err, io.ErrUnexpectedEOF)
			}
			if readCtx.Err() != nil {
				err = errors.Join(err, readCtx.Err())
			}
		}
		if err != nil {
			responseErr := d.writeResponse(uint32(syscall.EIO), handle, nil)
			failure := errors.Join(fmt.Errorf("nbd read at %d length %d: %w", from, length, err), responseErr)
			if responseErr != nil {
				d.fail(failure)
			} else if d.readOnly {
				// Keep the kernel connection owned until the registry drains its
				// users and explicitly disconnects this device. Faults receive EIO;
				// no subsequent request can turn this failed source into success.
				d.recordFailure(failure)
			}
			return
		}
		if err := d.writeResponse(0, handle, data); err != nil {
			d.fail(err)
		}
	}()
	return nil
}

func (d *Dispatch) cmdWrite(ctx context.Context, handle uint64, from uint64, data []byte) error {
	if d.readOnly {
		return d.writeResponse(uint32(syscall.EROFS), handle, nil)
	}
	if err := d.admit(ctx); err != nil {
		return err
	}
	d.writeAdmitted(ctx, handle, from, data)
	return nil
}

func (d *Dispatch) writeAdmitted(ctx context.Context, handle uint64, from uint64, data []byte) {
	go func() {
		defer d.complete()
		err := d.checkRange(ctx, from, uint32(len(data)))
		if err == nil {
			n, writeErr := d.writer.WriteAt(data, int64(from))
			err = writeErr
			if n != len(data) {
				err = errors.Join(err, io.ErrShortWrite)
			}
		}
		errno := uint32(0)
		if err != nil {
			errno = uint32(syscall.EIO)
		}
		if err := d.writeResponse(errno, handle, nil); err != nil {
			d.fail(err)
		}
	}()
}

func (d *Dispatch) cmdTrim(handle uint64, _ uint64, _ uint32) error {
	if d.readOnly {
		return d.writeResponse(uint32(syscall.EROFS), handle, nil)
	}
	// Retain the legacy rootfs advisory-TRIM behavior.
	return d.writeResponse(0, handle, nil)
}
