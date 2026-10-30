package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type dispatchProvider struct {
	data       []byte
	read       func(context.Context, []byte, int64) (int, error)
	writes     atomic.Int32
	shortWrite bool
}

func (p *dispatchProvider) Size(context.Context) (int64, error) { return int64(len(p.data)), nil }
func (p *dispatchProvider) ReadAt(ctx context.Context, b []byte, off int64) (int, error) {
	if p.read != nil {
		return p.read(ctx, b, off)
	}
	return copy(b, p.data[off:]), nil
}
func (p *dispatchProvider) WriteAt(b []byte, off int64) (int, error) {
	p.writes.Add(1)
	if p.shortWrite {
		return 0, nil
	}
	return copy(p.data[off:], b), nil
}

func sendNBD(t *testing.T, c net.Conn, typ uint32, handle, from uint64, length uint32, data []byte) {
	t.Helper()
	request := Request{NBDRequestMagic, typ, handle, from, length}
	if err := binary.Write(c, binary.BigEndian, request); err != nil {
		t.Fatal(err)
	}
	if err := writeFull(c, data); err != nil {
		t.Fatal(err)
	}
}
func receiveNBD(t *testing.T, c net.Conn, handle uint64, errno uint32, length int) []byte {
	t.Helper()
	var response Response
	if err := binary.Read(c, binary.BigEndian, &response); err != nil {
		t.Fatal(err)
	}
	if response.Magic != NBDResponseMagic || response.Handle != handle || response.Error != errno {
		t.Fatalf("unexpected reply: %+v; handle %d error %d", response, handle, errno)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(c, data); err != nil {
		t.Fatal(err)
	}
	return data
}
func dispatchPair(t *testing.T, p *dispatchProvider, readOnly bool) (net.Conn, *Dispatch, <-chan error) {
	t.Helper()
	server, client := net.Pipe()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	d := NewReadonlyDispatch(server, p)
	if !readOnly {
		d = NewDispatch(server, p)
	}
	done := make(chan error, 1)
	go func() { done <- d.Handle(ctx) }()
	t.Cleanup(func() { cancel(); _ = client.Close(); _ = server.Close(); d.Drain() })
	return client, d, done
}

func TestReadonlyDispatchRejectsMutationsAndPreservesFraming(t *testing.T) {
	p := &dispatchProvider{data: []byte("immutable bytes")}
	client, _, done := dispatchPair(t, p, true)
	// FUA occupies the high type bits and must not bypass mutation rejection.
	sendNBD(t, client, NBDCmdWrite|1<<16, 1, 0, 4, []byte("evil"))
	receiveNBD(t, client, 1, uint32(syscall.EROFS), 0)
	for i, command := range []uint32{NBDCmdTrim, NBDCmdWriteZeroes} {
		sendNBD(t, client, command, uint64(i+2), 0, 4, nil)
		receiveNBD(t, client, uint64(i+2), uint32(syscall.EROFS), 0)
	}
	sendNBD(t, client, NBDCmdRead, 4, 0, uint32(len(p.data)), nil)
	if got := receiveNBD(t, client, 4, 0, len(p.data)); !bytes.Equal(got, p.data) {
		t.Fatalf("data changed: %q", got)
	}
	if p.writes.Load() != 0 {
		t.Fatal("readonly dispatch called WriteAt")
	}
	sendNBD(t, client, NBDCmdDisconnect, 5, 0, 0, nil)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReadonlyDispatchRejectsIncompleteReads(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(context.Context, []byte, int64) (int, error)
	}{
		{"short-nil", func(_ context.Context, b []byte, _ int64) (int, error) { return copy(b, []byte("ab")), nil }},
		{"zero-nil", func(context.Context, []byte, int64) (int, error) { return 0, nil }},
		{"short-eof", func(_ context.Context, b []byte, _ int64) (int, error) { return copy(b, []byte("ab")), io.EOF }},
		{"source-error", func(context.Context, []byte, int64) (int, error) { return 0, errors.New("checksum mismatch") }},
		{"full-with-error", func(_ context.Context, b []byte, _ int64) (int, error) { return len(b), errors.New("version mismatch") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &dispatchProvider{data: make([]byte, 16), read: tc.read}
			client, d, _ := dispatchPair(t, p, true)
			sendNBD(t, client, NBDCmdRead, 1, 0, 4, nil)
			receiveNBD(t, client, 1, uint32(syscall.EIO), 0)
			select {
			case <-d.Done():
			case <-time.After(time.Second):
				t.Fatal("source failure was not broadcast")
			}
			if d.Err() == nil {
				t.Fatal("source failure missing error")
			}
			// Source failure retains the owned kernel socket until mount Close.
			sendNBD(t, client, NBDCmdRead, 2, 0, 4, nil)
			receiveNBD(t, client, 2, uint32(syscall.EIO), 0)
		})
	}
}

func TestReadonlyDispatchValidatesRangeBeforeSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		from   uint64
		length uint32
	}{
		{"past-end", 15, 2}, {"negative-as-unsigned", math.MaxUint64, 1}, {"overflow", math.MaxInt64, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &dispatchProvider{data: make([]byte, 16), read: func(context.Context, []byte, int64) (int, error) {
				t.Error("invalid range reached source")
				return 0, nil
			}}
			client, d, _ := dispatchPair(t, p, true)
			sendNBD(t, client, NBDCmdRead, 1, tc.from, tc.length, nil)
			receiveNBD(t, client, 1, uint32(syscall.EIO), 0)
			select {
			case <-d.Done():
			case <-time.After(time.Second):
				t.Fatal("range error was not broadcast")
			}
			if d.Err() == nil {
				t.Fatal("expected range error")
			}
		})
	}
}

func TestDispatchLegacyWritesAndShortWriteErrors(t *testing.T) {
	for _, short := range []bool{false, true} {
		p := &dispatchProvider{data: make([]byte, 16), shortWrite: short}
		client, _, _ := dispatchPair(t, p, false)
		sendNBD(t, client, NBDCmdWrite, 1, 4, 4, []byte("data"))
		errno := uint32(0)
		if short {
			errno = uint32(syscall.EIO)
		}
		receiveNBD(t, client, 1, errno, 0)
		if p.writes.Load() != 1 {
			t.Fatal("WriteAt was not called")
		}
		if !short && string(p.data[4:8]) != "data" {
			t.Fatal("legacy write lost data")
		}
		sendNBD(t, client, NBDCmdTrim, 2, 0, 4, nil)
		receiveNBD(t, client, 2, 0, 0)
	}
}

func TestDispatchBoundsReadsAndCancelsBlockedSource(t *testing.T) {
	// Cancellation races the parser, pending reads, and the context AfterFunc
	// that wakes transport writes. Repeat to exercise each cancellation order.
	for i := range 32 {
		t.Run(fmt.Sprint(i), testDispatchBoundsReadsAndCancelsBlockedSource)
	}
}

func testDispatchBoundsReadsAndCancelsBlockedSource(t *testing.T) {
	var active, maximum atomic.Int32
	started := make(chan struct{}, dispatchMaxInFlight)
	p := &dispatchProvider{data: make([]byte, 16), read: func(ctx context.Context, b []byte, off int64) (int, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		started <- struct{}{}
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	d := NewReadonlyDispatch(server, p)
	done := make(chan error, 1)
	go func() { done <- d.Handle(ctx) }()
	go func() {
		for i := 0; i < dispatchMaxInFlight+10; i++ {
			if err := binary.Write(client, binary.BigEndian, Request{NBDRequestMagic, NBDCmdRead, uint64(i), 0, 4}); err != nil {
				return
			}
		}
	}()
	defer client.Close()
	defer server.Close()
	defer cancel()
	for i := 0; i < dispatchMaxInFlight; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("read did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not wake parser")
	}
	drained := make(chan struct{})
	go func() { d.Drain(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel left pending operations")
	}
	if maximum.Load() > dispatchMaxInFlight || active.Load() != 0 {
		t.Fatalf("operations not bounded/drained: max=%d active=%d", maximum.Load(), active.Load())
	}
}

func TestDispatchRejectsOversizedRead(t *testing.T) {
	p := &dispatchProvider{data: make([]byte, 16)}
	client, _, done := dispatchPair(t, p, true)
	sendNBD(t, client, NBDCmdRead, 1, 0, dispatchMaxWriteBufferSize+1, nil)
	if err := <-done; err == nil {
		t.Fatal("oversized request accepted")
	}
}

type partialWriter struct {
	bytes.Buffer
	max  int
	zero bool
}

func (w *partialWriter) Write(b []byte) (int, error) {
	if w.zero {
		return 0, nil
	}
	if len(b) > w.max {
		b = b[:w.max]
	}
	return w.Buffer.Write(b)
}
func TestWriteFullHandlesShortTransportWrites(t *testing.T) {
	w := &partialWriter{max: 3}
	if err := writeFull(w, []byte("complete")); err != nil || w.String() != "complete" {
		t.Fatalf("short transport write: %q %v", w.String(), err)
	}
	if err := writeFull(&partialWriter{zero: true}, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero progress: %v", err)
	}
}
