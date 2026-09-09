package template

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	blockmetrics "github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block/metrics"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	featureflags "github.com/e2b-dev/infra/packages/shared/pkg/feature-flags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func TestMaterializeMemoryImageLayeredRanges(t *testing.T) {
	// Use the real layered File -> StorageDiff -> Chunker path. A raw local
	// source cannot expose shifted storage offsets or sparse mapping semantics.
	t.Setenv("STORAGE_PROVIDER", "Local")
	t.Setenv("LOCAL_TEMPLATE_STORAGE_BASE_PATH", t.TempDir())
	persistence, err := storage.GetTemplateStorageProvider(t.Context(), nil)
	require.NoError(t, err)
	metrics, err := blockmetrics.NewMetrics(noop.NewMeterProvider())
	require.NoError(t, err)

	const (
		logicalSize  = 2 * storage.MemoryChunkSize
		shift        = storage.MemoryChunkSize - 3956736
		secondLength = 790528
		split        = storage.MemoryChunkSize + secondLength
	)
	for _, sparseTail := range []bool{false, true} {
		t.Run(fmt.Sprintf("sparse_tail=%t", sparseTail), func(t *testing.T) {
			firstID, lastID := uuid.New(), uuid.New()
			first := bytes.Repeat([]byte{0x5a}, split-shift)
			last := bytes.Repeat([]byte{0xa5}, logicalSize-split)
			for id, data := range map[uuid.UUID][]byte{firstID: first, lastID: last} {
				rawPath := filepath.Join(t.TempDir(), "memfile")
				require.NoError(t, os.WriteFile(rawPath, data, 0o600))
				object, err := persistence.OpenSeekable(t.Context(), id.String()+"/"+string(build.Memfile), storage.MemfileObjectType)
				require.NoError(t, err)
				require.NoError(t, object.StoreFile(t.Context(), rawPath))
			}
			if sparseTail {
				lastID = uuid.Nil
			}
			h, err := header.NewHeader(header.NewTemplateMetadata(uuid.New(), header.PageSize, logicalSize), []*header.BuildMap{
				{Offset: 0, Length: shift, BuildId: uuid.Nil},
				{Offset: shift, Length: split - shift, BuildId: firstID, BuildStorageOffset: 0},
				{Offset: split, Length: logicalSize - split, BuildId: lastID, BuildStorageOffset: 0},
			})
			require.NoError(t, err)
			off, length, _, err := h.GetShiftedMapping(t.Context(), storage.MemoryChunkSize)
			require.NoError(t, err)
			require.Equal(t, int64(3956736), off)
			require.Equal(t, int64(secondLength), length)

			store, err := build.NewDiffStore(cfg.Config{}, &featureflags.Client{}, t.TempDir(), time.Hour, time.Hour)
			require.NoError(t, err)
			t.Cleanup(func() {
				store.RemoveCache()
				store.Close()
			})
			source, err := NewStorage(t.Context(), store, h.Metadata.BuildId.String(), build.Memfile, h, persistence, metrics)
			require.NoError(t, err)
			outputDir := t.TempDir()
			path, err := MaterializeMemoryImage(t.Context(), source, outputDir, "layered")
			require.NoError(t, err)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			expected := make([]byte, logicalSize)
			copy(expected[shift:split], first)
			if !sparseTail {
				copy(expected[split:], last)
			}
			require.Equal(t, len(expected), len(got))
			// Compare hashes to keep failures concise even for multi-MiB images.
			require.Equal(t, sha256.Sum256(expected), sha256.Sum256(got))
			require.Equal(t, fmt.Sprintf("%x.raw", sha256.Sum256(expected)), filepath.Base(path))
		})
	}
}

func TestMaterializeMemoryImagePublishesImmutableSparseFile(t *testing.T) {
	t.Parallel()

	data := make([]byte, 3*header.PageSize)
	copy(data[header.PageSize:2*header.PageSize], []byte("private-template-page"))
	sourcePath := filepath.Join(t.TempDir(), "source.raw")
	require.NoError(t, os.WriteFile(sourcePath, data, 0o600))

	source, err := block.NewLocal(sourcePath, header.PageSize, uuid.New())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })

	outputDir := filepath.Join(t.TempDir(), "images")
	path, err := MaterializeMemoryImage(t.Context(), source, outputDir, "build-123")
	require.NoError(t, err)

	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	assert.Equal(t, filepath.Join(outputDir, digest+".raw"), path)
	materialized, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, materialized)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o444), info.Mode().Perm())

	secondPath, err := MaterializeMemoryImage(t.Context(), source, outputDir, "build-123")
	require.NoError(t, err)
	assert.Equal(t, path, secondPath)
	reference, err := os.ReadFile(filepath.Join(outputDir, "build-123.v2.ref"))
	require.NoError(t, err)
	assert.Equal(t, digest+"\n", string(reference))
}

func TestMaterializeMemoryImageRejectsCorruptedPublishedFile(t *testing.T) {
	t.Parallel()

	data := []byte("memory-image")
	sourcePath := filepath.Join(t.TempDir(), "source.raw")
	require.NoError(t, os.WriteFile(sourcePath, data, 0o600))
	source, err := block.NewLocal(sourcePath, header.PageSize, uuid.New())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })

	outputDir := filepath.Join(t.TempDir(), "images")
	path, err := MaterializeMemoryImage(t.Context(), source, outputDir, "build-123")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o644))
	require.NoError(t, os.WriteFile(path, []byte("corrupt-data"), 0o444))
	require.NoError(t, os.Chmod(path, 0o444))

	_, err = MaterializeMemoryImage(t.Context(), source, outputDir, "build-123")
	require.ErrorContains(t, err, "checksum")
}

func TestMaterializeMemoryImageRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	_, err := MaterializeMemoryImage(t.Context(), nil, t.TempDir(), "../escape")
	require.Error(t, err)
}

func TestMaterializeMemoryImageIgnoresLegacyReference(t *testing.T) {
	t.Parallel()

	data := []byte("correct-image")
	sourcePath := filepath.Join(t.TempDir(), "source.raw")
	require.NoError(t, os.WriteFile(sourcePath, data, 0o600))
	source, err := block.NewLocal(sourcePath, header.PageSize, uuid.New())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })

	// An image produced with stale bytes still has a valid self-checksum.
	// The old reference must not bypass materialization with the corrected reader.
	oldData := bytes.Repeat([]byte{0x5a}, len(data))
	oldDigest := fmt.Sprintf("%x", sha256.Sum256(oldData))
	outputDir := t.TempDir()
	oldPath := filepath.Join(outputDir, oldDigest+".raw")
	require.NoError(t, os.WriteFile(oldPath, oldData, 0o444))
	oldReference := filepath.Join(outputDir, "build-123.ref")
	require.NoError(t, os.WriteFile(oldReference, []byte(oldDigest+"\n"), 0o444))

	path, err := MaterializeMemoryImage(t.Context(), source, outputDir, "build-123")
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, got)
	// Old content-addressed files may still be mapped by running VMs.
	old, err := os.ReadFile(oldPath)
	require.NoError(t, err)
	require.Equal(t, oldData, old)
	reference, err := os.ReadFile(oldReference)
	require.NoError(t, err)
	require.Equal(t, oldDigest+"\n", string(reference))
}
