//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bits-and-blooms/bitset"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/memorybacking"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/nbd"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
)

func TestSharingRemoteShortReadIntegration(t *testing.T) { //nolint:paralleltest
	t.Setenv("E2B_SHARING_REMOTE_FAILURE", "short")
	TestPmemFactoryIntegration(t)
}

func TestSharingRemoteDelayIntegration(t *testing.T) { //nolint:paralleltest
	t.Setenv("E2B_SHARING_REMOTE_FAILURE", "delay")
	TestPmemFactoryIntegration(t)
}

type sharingRemoteBacking struct {
	memorybacking.Backing
	remote *memorybacking.HTTPSource
	server *httptest.Server
	closed atomic.Bool
}

func (b *sharingRemoteBacking) Close(ctx context.Context) error {
	if err := b.Backing.Close(ctx); err != nil {
		return err // retain the source until disconnect is confirmed
	}
	err := b.remote.Close()
	b.server.Close()
	b.closed.Store(true)
	return err
}

// Inserts a real HTTPSource at the backing factory seam. The production owned
// view, registry, readonly NBD mount, two Factory restores and exit watchers all
// retain their usual code paths. No platform service is mocked by the runner.
func sharingRemoteFailure(t *testing.T, ctx context.Context, dir, kind string, factory *Factory, pool *nbd.DevicePool, flags *featureflags.Client, resume func(string) *Sandbox) {
	t.Helper()
	require.Contains(t, []string{"short", "delay"}, kind)
	var inject atomic.Bool
	var requests, injected atomic.Int64
	var backing *sharingRemoteBacking
	var traceMu sync.Mutex
	var trace []map[string]any
	require.NoError(t, factory.memoryBackings.Drain(ctx)) // no leases exist yet
	factory.memoryBackings = memorybacking.NewRegistry(nil, func(createCtx context.Context, original memorybacking.SnapshotSource) (memorybacking.Backing, error) {
		id := original.Identity()
		const etag = `"immutable-test-generation"`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-Match") != etag {
				http.Error(w, "unpinned generation", http.StatusPreconditionFailed)
				return
			}
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.FormatInt(id.LogicalSize, 10))
				return
			}
			var first, last int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &first, &last); err != nil || first < 0 || last < first || last >= id.LogicalSize || last-first >= 32<<20 {
				http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			requests.Add(1)
			fault := inject.Load()
			traceMu.Lock()
			trace = append(trace, map[string]any{"offset": first, "length": last - first + 1, "injected": fault})
			traceMu.Unlock()
			if fault {
				injected.Add(1)
				if kind == "delay" {
					<-r.Context().Done()
					return
				}
			}
			w.Header().Set("Content-Length", strconv.FormatInt(last-first+1, 10))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, id.LogicalSize))
			w.WriteHeader(http.StatusPartialContent)
			if fault {
				_, _ = w.Write([]byte{0x61}) // declare a full range, then actually cut the body
				return
			}
			data := make([]byte, last-first+1)
			n, err := original.ReadAt(r.Context(), data, first)
			if err != nil || n != len(data) {
				return
			}
			_, _ = w.Write(data)
		}))
		remote, err := memorybacking.NewHTTPSource(createCtx, id, server.URL, memorybacking.HTTPOptions{
			ETag: etag, RequestTimeout: time.Second,
			SourceOptions: memorybacking.SourceOptions{MaxReadBytes: 32 << 20, MaxConcurrentReads: 8},
		})
		if err != nil {
			server.Close()
			return nil, err
		}
		mounted, err := newSharedNBDFactory(pool, flags)(createCtx, remote)
		backing = &sharingRemoteBacking{Backing: mounted, remote: remote, server: server}
		return backing, err
	})
	left, right := resume("http-left"), resume("http-right")
	lm := left.Resources.memory.(*leasedMemory)
	rm := right.Resources.memory.(*leasedMemory)
	require.Equal(t, lm.lease.Path(), rm.lease.Path())
	require.NotNil(t, backing)
	require.Greater(t, requests.Load(), int64(0), "real VM restore must fetch HTTP ranges")
	for _, sbx := range []*Sandbox{left, right} {
		sbx.Checks.Stop()
		require.NoError(t, sbx.process.Pause(ctx))
		require.NoError(t, sbx.process.CreateSnapshot(ctx, filepath.Join(dir, sbx.Runtime.SandboxID+"-frozen.snap")))
	}
	before := requests.Load()
	inject.Store(true)
	start := time.Now()
	// A full exporter necessarily reaches unread snapshot RAM. The transport
	// must fail it, and broadcast to BOTH real VMs instead of supplying zeros.
	exportCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, exportErr := left.process.ExportMemory(exportCtx, bitset.New(256*256).SetAll(), filepath.Join(dir, "must-not-publish.ram"), 4096)
	if output != nil {
		_ = output.Close()
	}
	require.Error(t, exportErr)
	require.Greater(t, injected.Load(), int64(0))
	rows := make([]map[string]any, 0, 2)
	for _, sbx := range []*Sandbox{left, right} {
		lease := sbx.Resources.memory.(*leasedMemory).lease
		select {
		case <-lease.Done():
		case <-exportCtx.Done():
			t.Fatal("shared source failure was not broadcast to every lease")
		}
		require.Error(t, lease.Err())
		select {
		case <-sbx.process.Exit.Done():
		case <-exportCtx.Done():
			t.Fatal("memory failure did not stop real FC")
		}
		select {
		case <-sbx.exit.Done():
		case <-exportCtx.Done():
			t.Fatal("sandbox exit did not reach a terminal state")
		}
		pid, err := sbx.process.Pid()
		require.NoError(t, err)
		_, statErr := os.Stat(fmt.Sprintf("/proc/%d", pid))
		require.True(t, errors.Is(statErr, os.ErrNotExist))
		closeErr := sbx.Close(ctx)
		rows = append(rows, map[string]any{"pid": pid, "process_exited": true, "lease_error": lease.Err().Error(), "close_error": fmt.Sprint(closeErr)})
	}
	// The lifecycle preserves the injected failure. Test the device registry
	// separately so an expected VM error cannot hide a failed NBD disconnect.
	lifecycleErr := factory.lifecycle.drain(ctx)
	require.Error(t, lifecycleErr)
	require.NoError(t, factory.memoryBackings.Drain(ctx))
	require.True(t, backing.closed.Load())
	traceMu.Lock()
	defer traceMu.Unlock()
	sharingReceipt(t, filepath.Join(dir, "remote-failure-result.json"), map[string]any{
		"result": "passed", "fault": kind, "same_device": lm.lease.Path(), "vms": rows,
		"requests_before_fault": before, "injected_requests": injected.Load(), "requests": trace,
		"export_error": exportErr.Error(), "failure_to_terminal_seconds": time.Since(start).Seconds(),
		"registry_drained_after_process_exit": true, "backing_closed": backing.closed.Load(),
		"lifecycle_error": strings.TrimSpace(lifecycleErr.Error()), "zero_fill_accepted": false})
}
