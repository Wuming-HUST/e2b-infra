package uffd

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// M01: a parent page need never have faulted into the restored process to be
// nonzero. Noop must not consult residency, nor classify unread RAM as Empty.
func TestNoopMemoryUnaccessedParentIsExported(t *testing.T) {
	const blockSize = header.PageSize
	m := NewNoopMemory(3*blockSize, blockSize)
	diff, err := m.DiffMetadata(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, uint(3), diff.Dirty.Count())
	require.Zero(t, diff.Empty.Count())
	for i := uint(0); i < 3; i++ {
		require.True(t, diff.Dirty.Test(i))
	}
}

// M07 software oracle: apply the actual persisted header mapping to packed
// full-logical exports over A -> B -> C. The untouched nonzero first block,
// a write-to-zero block and a changed block must all survive each generation.
func TestNoopMemoryMultiGenerationHeaderOracle(t *testing.T) {
	const blockSize = header.PageSize
	const size = 3 * blockSize
	baseID := uuid.New()
	h, err := header.NewHeader(header.NewTemplateMetadata(baseID, blockSize, size), nil)
	require.NoError(t, err)
	base := bytes.Repeat([]byte{0x51}, size)
	sources := map[uuid.UUID][]byte{baseID: base}
	current := bytes.Clone(base)
	for generation := byte(1); generation <= 3; generation++ {
		clear(current[blockSize : 2*blockSize])
		copy(current[2*blockSize:], bytes.Repeat([]byte{generation}, blockSize))
		diff, err := NewNoopMemory(size, blockSize).DiffMetadata(t.Context(), nil)
		require.NoError(t, err)
		id := uuid.New()
		var packed bytes.Buffer
		for i := uint(0); i < size/blockSize; i++ {
			if diff.Dirty.Test(i) {
				packed.Write(current[int(i)*blockSize : (int(i)+1)*blockSize])
			}
		}
		sources[id] = bytes.Clone(packed.Bytes())
		h, err = diff.ToDiffHeader(t.Context(), h, id)
		require.NoError(t, err)
		restored := make([]byte, size)
		for off := int64(0); off < size; off += blockSize {
			storageOffset, _, sourceID, err := h.GetShiftedMapping(t.Context(), off)
			require.NoError(t, err)
			if *sourceID != uuid.Nil {
				copy(restored[off:off+blockSize], sources[*sourceID][storageOffset:storageOffset+blockSize])
			}
		}
		require.Equal(t, sha256.Sum256(current), sha256.Sum256(restored))
		require.Equal(t, current, restored)
	}
}

func TestNoopMemoryRejectsInvalidLogicalLayout(t *testing.T) {
	for _, layout := range [][2]int64{{-1, 4096}, {4096, 0}, {4097, 4096}} {
		_, err := NewNoopMemory(layout[0], layout[1]).DiffMetadata(t.Context(), nil)
		require.Error(t, err)
	}
}
