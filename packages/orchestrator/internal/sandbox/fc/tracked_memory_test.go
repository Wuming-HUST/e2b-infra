package fc

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/shared/pkg/fc/models"
	"github.com/stretchr/testify/require"
)

func trackedResponse() *models.DirtyMemory {
	return &models.DirtyMemory{Bitmap: []uint64{1, 1}, TrackingVersion: 1, TrackingMode: "bitmap",
		TrackingComplete: true, CaptureReady: true, LogicalSize: 65 * 4096, PageSize: 4096}
}

func TestTrackedMemoryContractRejectsIncompleteCandidates(t *testing.T) {
	tests := map[string]func(*models.DirtyMemory){
		"old_fc":          func(m *models.DirtyMemory) { *m = models.DirtyMemory{Bitmap: m.Bitmap} },
		"wrong_version":   func(m *models.DirtyMemory) { m.TrackingVersion = 2 },
		"resident_policy": func(m *models.DirtyMemory) { m.TrackingMode = "hybrid" },
		"untracked":       func(m *models.DirtyMemory) { m.TrackingComplete = false },
		"not_frozen":      func(m *models.DirtyMemory) { m.CaptureReady = false },
		"wrong_size":      func(m *models.DirtyMemory) { m.LogicalSize++ },
		"wrong_page":      func(m *models.DirtyMemory) { m.PageSize = 2 << 20 },
		"truncated":       func(m *models.DirtyMemory) { m.Bitmap = m.Bitmap[:1] },
		"extra_word":      func(m *models.DirtyMemory) { m.Bitmap = append(m.Bitmap, 0) },
		"tail_bits":       func(m *models.DirtyMemory) { m.Bitmap[1] = 2 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m := trackedResponse()
			mutate(m)
			_, err := validateTrackedMemory(m, 65*4096, 4096)
			require.Error(t, err)
		})
	}
	_, err := validateTrackedMemory(nil, 4096, 4096)
	require.Error(t, err)
	_, err = validateTrackedMemory(trackedResponse(), 65*4096, 8192)
	require.Error(t, err)
}

func TestTrackedMemoryPreservesColdAndSwappedCandidateBits(t *testing.T) {
	// Selection consumes only the FC contract, never host residency. Bits at
	// both ends survive; unset cold parent pages remain inherited, not Empty.
	m := trackedResponse()
	d, err := validateTrackedMemory(m, 65*4096, 4096)
	require.NoError(t, err)
	require.EqualValues(t, 2, d.Dirty.Count())
	require.True(t, d.Dirty.Test(64))
	require.Zero(t, d.Empty.Count())
	m.Bitmap = []uint64{0, 0}
	d, err = validateTrackedMemory(m, 65*4096, 4096)
	require.NoError(t, err)
	require.Zero(t, d.Dirty.Count())
}

func TestTrackedMemoryLoadEnablesLoggingOnlyWhenRequested(t *testing.T) {
	for _, track := range []bool{false, true} {
		t.Run(map[bool]string{true: "tracked", false: "full"}[track], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fc.sock")
			listener, err := net.Listen("unix", path)
			require.NoError(t, err)
			bodies := make(chan map[string]any, 1)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "PUT" || r.URL.Path != "/snapshot/load" {
					http.Error(w, "unexpected API", 404)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				bodies <- body
				w.WriteHeader(204)
			})}
			go server.Serve(listener)
			defer server.Close()
			backend := NewFileSnapshotMemoryBackend("/dev/nbd42")
			backend.TrackDirty = track
			require.NoError(t, newApiClient(path).loadSnapshot(context.Background(), backend, template.NewLocalFileLink("/state")))
			body := <-bodies
			require.Equal(t, track, body["enable_diff_snapshots"] == true)
			require.NotEqual(t, true, body["resume_vm"])
		})
	}
}
