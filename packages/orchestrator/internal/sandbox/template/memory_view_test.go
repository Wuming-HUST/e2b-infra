package template

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/memorybacking"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type borrowedMemoryView struct{ block.ReadonlyDevice }

func TestOwnedMemoryViewRejectsBorrowedSource(t *testing.T) {
	t.Parallel()
	_, err := AcquireMemoryView(t.Context(), &borrowedMemoryView{})
	require.ErrorContains(t, err, "does not support owned memory views")
}

func TestOwnedMemoryViewSourceSurvivesTemplateAndCacheClose(t *testing.T) {
	t.Parallel()
	store, err := build.NewDiffStore(cfg.Config{}, nil, t.TempDir(), time.Hour, time.Hour)
	require.NoError(t, err)
	t.Cleanup(store.RemoveCache)
	id := uuid.New()
	local, err := build.NewLocalDiffFile(t.TempDir(), id.String(), build.Memfile)
	require.NoError(t, err)
	want := bytes.Repeat([]byte{0x68}, 4096)
	_, err = local.Write(want)
	require.NoError(t, err)
	diff, err := local.CloseToDiff(4096)
	require.NoError(t, err)
	require.NoError(t, store.Add(diff))
	h, err := header.NewHeader(&header.Metadata{Version: 3, Size: 4096, BlockSize: 4096, BuildId: id}, nil)
	require.NoError(t, err)
	template, err := NewStorage(t.Context(), store, id.String(), build.Memfile, h, nil, blockmetrics.Metrics{})
	require.NoError(t, err)
	view, err := AcquireMemoryView(t.Context(), template)
	require.NoError(t, err)
	source, err := memorybacking.NewLocalSource(t.Context(), memorybacking.SnapshotIdentity{
		Kind: "ram", ContentID: "owned-view", LayoutID: "test-layout", LogicalSize: 4096,
	}, view, memorybacking.SourceOptions{Close: view.Close})
	require.NoError(t, err)
	require.NoError(t, diff.Close()) // Original snapshot cleanup cannot steal ownership.
	require.NoError(t, template.Close())
	store.RemoveCache()
	got := make([]byte, 4096)
	n, err := source.ReadAt(t.Context(), got, 0)
	require.NoError(t, err)
	require.Equal(t, len(got), n)
	require.Equal(t, want, got)
	require.NoError(t, source.Close())
	require.NoError(t, source.Close())
	path, err := diff.CachePath()
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) }, time.Second, time.Millisecond)
}
