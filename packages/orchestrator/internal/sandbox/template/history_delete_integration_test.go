package template

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// This verifies the supported failure boundary; runtime pins are not a durable
// GC reference index. A deleted unread ancestor must fail, never become zeros.
func TestHistoryObjectDeletionRejectsUnreadAncestor(t *testing.T) {
	t.Setenv("STORAGE_PROVIDER", "Local")
	t.Setenv("LOCAL_TEMPLATE_STORAGE_BASE_PATH", t.TempDir())
	persistence, err := storage.GetTemplateStorageProvider(t.Context(), nil)
	require.NoError(t, err)
	metrics, err := blockmetrics.NewMetrics(noop.NewMeterProvider())
	require.NoError(t, err)
	flags := &featureflags.Client{}
	firstID, lastID := uuid.New(), uuid.New()
	first := bytes.Repeat([]byte{0x5a}, 2*storage.MemoryChunkSize)
	last := bytes.Repeat([]byte{0xa7}, 4096)
	for id, data := range map[uuid.UUID][]byte{firstID: first, lastID: last} {
		path := filepath.Join(t.TempDir(), "payload")
		require.NoError(t, os.WriteFile(path, data, 0o600))
		object, err := persistence.OpenSeekable(t.Context(), id.String()+"/"+string(build.Memfile), storage.MemfileObjectType)
		require.NoError(t, err)
		require.NoError(t, object.StoreFile(t.Context(), path))
	}
	logicalSize := uint64(len(first) + len(last))
	h, err := header.NewHeader(header.NewTemplateMetadata(lastID, 4096, logicalSize), []*header.BuildMap{
		{Offset: 0, Length: uint64(len(first)), BuildId: firstID},
		{Offset: uint64(len(first)), Length: uint64(len(last)), BuildId: lastID},
	})
	require.NoError(t, err)
	store, err := build.NewDiffStore(cfg.Config{}, flags, t.TempDir(), time.Hour, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { store.RemoveCache(); store.Close() })
	source, err := NewStorage(t.Context(), store, lastID.String(), build.Memfile, h, persistence, metrics)
	require.NoError(t, err)
	view, err := source.AcquireMemoryView(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, view.Close()) })
	buffer := make([]byte, 4096)
	n, err := view.ReadAt(t.Context(), buffer, 0)
	require.NoError(t, err)
	require.Equal(t, 4096, n)
	require.Equal(t, first[:4096], buffer)
	// Invalidation retires the cache owner while the real pinned source remains.
	store.RemoveCache()
	require.NoError(t, persistence.DeleteObjectsWithPrefix(t.Context(), firstID.String()))
	_, deletedErr := view.ReadAt(t.Context(), buffer, storage.MemoryChunkSize)
	require.Error(t, deletedErr, "deleted unread ancestor must fail")
	// An independent cold store must also reject this published descendant.
	cold, err := build.NewDiffStore(cfg.Config{}, flags, t.TempDir(), time.Hour, time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { cold.RemoveCache(); cold.Close() })
	coldSource, err := NewStorage(t.Context(), cold, lastID.String(), build.Memfile, h, persistence, metrics)
	require.NoError(t, err)
	coldView, coldErr := coldSource.AcquireMemoryView(t.Context())
	if coldView != nil {
		defer coldView.Close()
	}
	require.Error(t, coldErr)
	// Remaining current-generation bytes still exist; failure is specifically
	// the inherited dependency rather than accidental deletion of the child.
	child, err := persistence.OpenSeekable(t.Context(), lastID.String()+"/"+string(build.Memfile), storage.MemfileObjectType)
	require.NoError(t, err)
	n, err = child.ReadAt(t.Context(), buffer, 0)
	require.NoError(t, err)
	require.Equal(t, 4096, n)
	require.Equal(t, last, buffer)
	t.Logf("published descendant=%s; deleted ancestor=%s; pinned unread=%v; cold restore=%v; durable GC retention remains required", lastID, firstID, fmt.Sprint(deletedErr), coldErr)
}
