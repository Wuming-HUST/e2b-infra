//go:build linux

// page-sharing-source serves one immutable raw RAM snapshot for the isolated
// two-VM test. Kernel IndexAny allocation never selects an existing NBD number.
// Stop all mapping users before sending SIGTERM; failures retain PID receipts.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/memorybacking"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/stresstest"
)

type rawReader struct {
	file         *os.File
	size         int64
	reads, bytes atomic.Int64
}

func (r *rawReader) Size(context.Context) (int64, error) { return r.size, nil }
func (r *rawReader) ReadAt(ctx context.Context, dst []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.file.ReadAt(dst, off)
	r.reads.Add(1)
	r.bytes.Add(int64(n))
	return n, err
}

type sourceDevice struct{ memorybacking.SnapshotSource }

func (s sourceDevice) Size(context.Context) (int64, error) { return s.Identity().LogicalSize, nil }
func (s sourceDevice) BlockSize() int64                    { return 4096 }
func (s sourceDevice) Header() *header.Header              { return nil }
func (s sourceDevice) Close() error                        { return nil } // registry owns source
func (s sourceDevice) Slice(ctx context.Context, off, length int64) ([]byte, error) {
	if length < 0 || length > 32<<20 {
		return nil, errors.New("invalid source slice bound")
	}
	dst := make([]byte, int(length))
	_, err := s.ReadAt(ctx, dst, off)
	return dst, err
}

type backing struct {
	mount *nbd.DirectPathMount
	path  string
	ready chan struct{}
}

func (b *backing) Path() string                    { return b.path }
func (b *backing) Ready() <-chan struct{}          { return b.ready }
func (b *backing) Done() <-chan struct{}           { return b.mount.Done() }
func (b *backing) Err() error                      { return b.mount.Err() }
func (b *backing) Close(ctx context.Context) error { return b.mount.Close(ctx) }

var outputMu sync.Mutex

func emit(value any) {
	outputMu.Lock()
	defer outputMu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(value)
}

func quarantine(path string, err error) {
	emit(map[string]any{"event": "quarantined", "pid": os.Getpid(), "path": path, "error": err.Error()})
	for {
		time.Sleep(time.Hour)
	}
}

func run() error {
	path := flag.String("source", "", "immutable regular raw RAM snapshot")
	layout := flag.String("layout-id", "ram-linear-4k-v1", "logical region/page-size identity")
	flag.Parse()
	if *path == "" || os.Getpagesize() != 4096 {
		return errors.New("--source and a 4096-byte host page size are required")
	}
	// This selects the existing dynamic IndexAny pool, without Populate or a
	// preselected index. It never enumerates/disconnects unrelated devices.
	if err := os.Setenv(stresstest.Env, "true"); err != nil {
		return err
	}
	f, err := os.Open(*path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size()%4096 != 0 {
		f.Close()
		return errors.Join(errors.New("source must be a nonempty 4K-aligned regular file"), err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		f.Close()
		return err
	}
	id := memorybacking.SnapshotIdentity{Kind: "ram", ContentID: "sha256:" + hex.EncodeToString(digest.Sum(nil)), LayoutID: *layout, LogicalSize: info.Size()}
	reader := &rawReader{file: f, size: info.Size()}
	pool, err := nbd.NewDevicePool()
	if err != nil {
		f.Close()
		return err
	}
	registry := memorybacking.NewRegistry(func(ctx context.Context, requested memorybacking.SnapshotIdentity) (memorybacking.SnapshotSource, error) {
		if requested != id {
			return nil, errors.New("unexpected source identity")
		}
		return memorybacking.NewLocalSource(ctx, id, reader, memorybacking.SourceOptions{
			MaxReadBytes: 32 << 20, MaxConcurrentReads: 8, Close: f.Close,
		})
	}, func(ctx context.Context, source memorybacking.SnapshotSource) (memorybacking.Backing, error) {
		mount := nbd.NewReadonlyDirectPathMount(sourceDevice{source}, pool, &featureflags.Client{})
		b := &backing{mount: mount, ready: make(chan struct{})}
		index, err := mount.Open(ctx)
		if err != nil {
			return b, err
		}
		b.path = nbd.GetDevicePath(index)
		close(b.ready)
		return b, nil
	})
	signalContext, cancelSignal := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancelSignal()
	createContext, cancelCreate := context.WithTimeout(signalContext, 30*time.Second)
	defer cancelCreate()
	first, err := registry.Acquire(createContext, id)
	if err != nil {
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if drainErr := registry.Drain(shutdown); drainErr != nil {
			quarantine("", errors.Join(err, drainErr))
		}
		_ = f.Close() // opener may not have accepted ownership on validation failure
		return errors.Join(err, pool.Close(shutdown))
	}
	second, err := registry.Acquire(createContext, id)
	if err != nil {
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		closeErr := errors.Join(first.Release(shutdown), registry.Drain(shutdown))
		if closeErr != nil {
			quarantine(first.Path(), errors.Join(err, closeErr))
		}
		return errors.Join(err, pool.Close(shutdown))
	}
	if first.Path() != second.Path() {
		quarantine(first.Path(), errors.New("registry returned different devices for one identity"))
	}
	emit(map[string]any{"event": "ready", "pid": os.Getpid(), "path": first.Path(), "content_id": id.ContentID,
		"layout_id": id.LayoutID, "logical_size": id.LogicalSize, "leases": 2})
	commands := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			select {
			case commands <- scanner.Text():
			case <-signalContext.Done():
				return
			}
		}
	}()
	failure := first.Done()
	for signalContext.Err() == nil {
		select {
		case <-signalContext.Done():
		case <-failure:
			emit(map[string]any{"event": "failure", "error": fmt.Sprint(first.Err()), "pid": os.Getpid()})
			failure = nil // retain source until the runner stops all mapping users
		case command := <-commands:
			if command == "release-one" {
				if err := first.Release(context.Background()); err != nil {
					return err
				}
				emit(map[string]any{"event": "lease_released", "path": second.Path(), "remaining": 1})
			}
		}
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	err = errors.Join(first.Release(shutdown), second.Release(shutdown), registry.Drain(shutdown))
	if err == nil {
		err = pool.Close(shutdown)
	}
	if err != nil {
		// Keep source FDs and uncertain ownership intact for investigation. The
		// operator may inspect and explicitly terminate this helper afterwards.
		quarantine(second.Path(), err)
	}
	emit(map[string]any{"event": "closed", "reads": reader.reads.Load(), "read_bytes": reader.bytes.Load()})
	return nil
}

func main() {
	if err := run(); err != nil {
		emit(map[string]any{"event": "error", "pid": os.Getpid(), "error": err.Error()})
		os.Exit(1)
	}
}
