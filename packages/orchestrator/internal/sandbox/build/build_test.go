package build

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func TestLogicalReadAtUnalignedCrossLayerAndZeroExtent(t *testing.T) {
	const page = header.PageSize
	parentID, childID := uuid.New(), uuid.New()
	h, err := header.NewHeader(header.NewTemplateMetadata(parentID, page, 3*page), []*header.BuildMap{
		{Offset: 0, Length: page, BuildId: parentID},
		{Offset: page, Length: page, BuildId: uuid.Nil},
		{Offset: 2 * page, Length: page, BuildId: childID, BuildStorageOffset: page},
	})
	require.NoError(t, err)
	sources := map[uuid.UUID][]byte{
		parentID: bytes.Repeat([]byte{0x33}, page),
		childID:  append(make([]byte, page), bytes.Repeat([]byte{0x55}, page)...),
	}
	reader := func(_ context.Context, id uuid.UUID, dst []byte, off int64) (int, error) {
		// Force short reads, including ones not aligned to a header block.
		count := min(517, len(dst))
		return copy(dst[:count], sources[id][off:]), nil
	}
	p := bytes.Repeat([]byte{0xff}, 2*page+777)
	n, err := readLogicalAt(t.Context(), h, p, 511, reader)
	require.NoError(t, err)
	require.Equal(t, len(p), n)
	want := append(bytes.Repeat([]byte{0x33}, page), make([]byte, page)...)
	want = append(want, bytes.Repeat([]byte{0x55}, page)...)
	require.Equal(t, want[511:511+len(p)], p)
	// Reading beyond logical EOF returns the prefix, preserving the unused tail.
	p = bytes.Repeat([]byte{0xff}, 777)
	n, err = readLogicalAt(t.Context(), h, p, 3*page-512, reader)
	require.Equal(t, 512, n)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, bytes.Repeat([]byte{0x55}, 512), p[:512])
	require.Equal(t, bytes.Repeat([]byte{0xff}, 777-512), p[512:])
}

func TestLogicalReadAtRejectsZeroProgressAndTruncatedSource(t *testing.T) {
	id := uuid.New()
	h, err := header.NewHeader(header.NewTemplateMetadata(id, header.PageSize, header.PageSize), nil)
	require.NoError(t, err)
	for _, sourceErr := range []error{nil, io.ErrUnexpectedEOF} {
		n, err := readLogicalAt(t.Context(), h, make([]byte, 512), 17,
			func(context.Context, uuid.UUID, []byte, int64) (int, error) { return 0, sourceErr })
		require.Zero(t, n)
		if sourceErr == nil {
			require.ErrorIs(t, err, io.ErrNoProgress)
		} else {
			require.ErrorIs(t, err, sourceErr)
		}
	}
	_, err = readLogicalAt(t.Context(), h, make([]byte, 1), -1, nil)
	require.Error(t, err)
}
