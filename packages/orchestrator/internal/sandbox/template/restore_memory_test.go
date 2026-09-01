package template

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

func TestFileMemoryBackendPathHonorsPerRestorePolicy(t *testing.T) {
	t.Parallel()

	source := &storageTemplate{memoryImage: utils.NewSetOnce[string]()}
	require.NoError(t, source.memoryImage.SetValue("/cache/digest.raw"))

	path, ok := FileMemoryBackendPath(withRestoreMemoryPolicy(source, true))
	assert.True(t, ok)
	assert.Equal(t, "/cache/digest.raw", path)

	path, ok = FileMemoryBackendPath(withRestoreMemoryPolicy(source, false))
	assert.False(t, ok)
	assert.Empty(t, path)
}

func TestFileMemoryBackendPathFallsBackWhileMaterializing(t *testing.T) {
	t.Parallel()

	source := &storageTemplate{memoryImage: utils.NewSetOnce[string]()}
	path, ok := FileMemoryBackendPath(withRestoreMemoryPolicy(source, true))

	assert.False(t, ok)
	assert.Empty(t, path)
}
