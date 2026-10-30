package header

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/bits-and-blooms/bitset"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTrackedDeltaGenerationsInheritColdZeroAndEmptyRounds(t *testing.T) {
	const page, pages = 4096, 17
	rng := rand.New(rand.NewSource(20261030))
	base := make([]byte, page*pages)
	_, _ = rng.Read(base)
	id := uuid.New()
	h, err := NewHeader(NewTemplateMetadata(id, page, uint64(len(base))), nil)
	require.NoError(t, err)
	objects := map[uuid.UUID][]byte{id: bytes.Clone(base)}
	want := bytes.Clone(base)
	for generation := 0; generation < 12; generation++ {
		candidates := bitset.New(pages)
		for i := 1; i < pages; i++ { // page zero stays cold and nonzero in every generation.
			if generation%3 != 2 && rng.Intn(3) == 0 {
				candidates.Set(uint(i))
				if i%2 == 0 {
					clear(want[i*page : (i+1)*page])
				} else {
					_, _ = rng.Read(want[i*page : (i+1)*page])
				}
			}
		}
		var packed []byte
		for i, ok := candidates.NextSet(0); ok; i, ok = candidates.NextSet(i + 1) {
			packed = append(packed, want[int(i)*page:(int(i)+1)*page]...)
		}
		id = uuid.New()
		objects[id] = packed
		h, err = (&DiffMetadata{Dirty: candidates, Empty: bitset.New(0), BlockSize: page}).ToDiffHeader(context.Background(), h, id)
		require.NoError(t, err)
		var restored []byte
		for off := int64(0); off < int64(len(want)); off += page {
			mapped, length, build, err := h.GetShiftedMapping(context.Background(), off)
			require.NoError(t, err)
			require.GreaterOrEqual(t, length, int64(page))
			payload, exists := objects[*build]
			require.True(t, exists, "referenced history must exist")
			restored = append(restored, payload[mapped:mapped+page]...)
		}
		require.Equal(t, want, restored, "generation %d", generation)
	}
}
