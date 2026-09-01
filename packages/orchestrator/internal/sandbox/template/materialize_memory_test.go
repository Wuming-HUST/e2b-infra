package template

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

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

	assert.Equal(t, filepath.Join(outputDir, "build-123.raw"), path)
	materialized, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, materialized)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o444), info.Mode().Perm())

	secondPath, err := MaterializeMemoryImage(t.Context(), source, outputDir, "build-123")
	require.NoError(t, err)
	assert.Equal(t, path, secondPath)
}

func TestMaterializeMemoryImageRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	_, err := MaterializeMemoryImage(t.Context(), nil, t.TempDir(), "../escape")
	require.Error(t, err)
}
