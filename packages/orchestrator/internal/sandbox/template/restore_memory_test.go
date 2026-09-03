package template

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

func TestFileMemoryBackendPathHonorsPerRestorePolicy(t *testing.T) {
	t.Parallel()

	memoryImagePath := filepath.Join(t.TempDir(), "digest.raw")
	require.NoError(t, os.WriteFile(memoryImagePath, []byte("memory"), 0o444))

	source := &storageTemplate{memoryImage: utils.NewSetOnce[string]()}
	require.NoError(t, source.memoryImage.SetValue(memoryImagePath))

	path, ok := FileMemoryBackendPath(withRestoreMemoryPolicy(source, true))
	assert.True(t, ok)
	assert.Equal(t, memoryImagePath, path)

	path, ok = FileMemoryBackendPath(withRestoreMemoryPolicy(source, false))
	assert.False(t, ok)
	assert.Empty(t, path)
}

func TestFileMemoryBackendPathRejectsUnsafeImage(t *testing.T) {
	t.Parallel()

	writableImagePath := filepath.Join(t.TempDir(), "writable.raw")
	require.NoError(t, os.WriteFile(writableImagePath, []byte("memory"), 0o644))

	tests := map[string]string{
		"missing file":  filepath.Join(t.TempDir(), "missing.raw"),
		"writable file": writableImagePath,
		"directory":     t.TempDir(),
	}

	for name, imagePath := range tests {
		t.Run(name, func(t *testing.T) {
			source := &storageTemplate{memoryImage: utils.NewSetOnce[string]()}
			require.NoError(t, source.memoryImage.SetValue(imagePath))

			path, ok := FileMemoryBackendPath(withRestoreMemoryPolicy(source, true))
			assert.False(t, ok)
			assert.Empty(t, path)
		})
	}
}

func TestFileMemoryBackendPathFallsBackWhileMaterializing(t *testing.T) {
	t.Parallel()

	source := &storageTemplate{memoryImage: utils.NewSetOnce[string]()}
	path, ok := FileMemoryBackendPath(withRestoreMemoryPolicy(source, true))

	assert.False(t, ok)
	assert.Empty(t, path)
}
