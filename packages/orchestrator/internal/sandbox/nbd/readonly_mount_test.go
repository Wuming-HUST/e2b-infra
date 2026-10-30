//go:build linux

package nbd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/Merovius/nbd/nbdnl"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type readonlyMountSource struct {
	dispatchProvider
	closes atomic.Int32
}

func (p *readonlyMountSource) Close() error { p.closes.Add(1); return nil }
func (p *readonlyMountSource) Slice(_ context.Context, off, length int64) ([]byte, error) {
	return p.data[off : off+length], nil
}
func (*readonlyMountSource) BlockSize() int64       { return 4096 }
func (*readonlyMountSource) Header() *header.Header { return nil }

func TestReadonlyMountProtectsSourceAndFlags(t *testing.T) {
	p := &readonlyMountSource{dispatchProvider: dispatchProvider{data: make([]byte, 4096)}}
	m := NewReadonlyDirectPathMount(p, nil, nil)
	if m.serverFlags()&nbdnl.FlagReadOnly == 0 {
		t.Fatal("missing kernel readonly flag")
	}
	if _, err := m.Backend.WriteAt([]byte("bad"), 0); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("adapter allowed write: %v", err)
	}
	if err := m.Backend.Close(); err != nil {
		t.Fatal(err)
	}
	if p.closes.Load() != 0 || p.writes.Load() != 0 {
		t.Fatal("mount adapter changed/closed registry-owned source")
	}
	legacy := NewDirectPathMount(p, nil, nil)
	if legacy.serverFlags()&nbdnl.FlagReadOnly != 0 {
		t.Fatal("legacy rootfs became readonly")
	}
}

func TestReadonlyMountFailureBroadcast(t *testing.T) {
	m := NewReadonlyDirectPathMount(&readonlyMountSource{}, nil, nil)
	if m.Err() != nil {
		t.Fatal("new mount has an error")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			<-m.Done()
			if !errors.Is(m.Err(), syscall.EIO) {
				t.Errorf("holder missed terminal error: %v", m.Err())
			}
		})
	}
	m.fail(syscall.EIO)
	m.fail(errors.New("later error"))
	wg.Wait()
}

func TestReadonlyMountCloseIsIdempotent(t *testing.T) {
	p := &readonlyMountSource{}
	m := NewReadonlyDirectPathMount(p, &DevicePool{dynamic: true}, nil)
	for i := 0; i < 2; i++ {
		if err := m.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if p.closes.Load() != 0 {
		t.Fatal("Close closed registry-owned source")
	}
	select {
	case <-m.Done():
		t.Fatal("normal Close broadcast failure")
	default:
	}
}

func TestReadonlyMountQuarantinesLostTransport(t *testing.T) {
	p := &readonlyMountSource{}
	// Nil pool proves no pool disconnect/release is attempted for an unknown
	// connection. Keep source ownership available for registry quarantine.
	m := NewReadonlyDirectPathMount(p, nil, nil)
	m.deviceIndex = 123
	m.transportLost.Store(true)
	for i := 0; i < 2; i++ {
		if err := m.Close(context.Background()); !errors.Is(err, ErrTransportOwnershipUnknown) {
			t.Fatalf("lost transport not quarantined: %v", err)
		}
	}
	if p.closes.Load() != 0 {
		t.Fatal("quarantined source was closed")
	}
}

func TestReadonlyMountQuarantinesUnobservedSocketEOF(t *testing.T) {
	for _, closeServer := range []bool{false, true} {
		t.Run(fmt.Sprint(closeServer), func(t *testing.T) {
			fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			peer := os.NewFile(uintptr(fds[0]), "kernel-peer")
			file := os.NewFile(uintptr(fds[1]), "service")
			conn, err := net.FileConn(file)
			file.Close()
			if err != nil {
				peer.Close()
				t.Fatal(err)
			}
			defer peer.Close()
			defer conn.Close()
			m := NewReadonlyDirectPathMount(&readonlyMountSource{}, &DevicePool{dynamic: true}, nil)
			m.deviceIndex = 123
			m.socksServer = []io.Closer{conn}
			if err := m.checkReadonlyTransport(); err != nil {
				t.Fatalf("healthy transport: %v", err)
			}
			// No dispatch handler runs: Close itself must notice the loss.
			if closeServer {
				conn.Close()
			} else {
				peer.Close()
			}
			if err := m.Close(context.Background()); !errors.Is(err, ErrTransportOwnershipUnknown) {
				t.Fatalf("unobserved transport loss was not quarantined: %v", err)
			}
		})
	}
}

func TestReadonlyMountReadinessRejectsRegularFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "not-a-block-device")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateReadonlyReady(file.Name(), 4096); err == nil {
		t.Fatal("regular path was accepted as a ready NBD device")
	}
}
