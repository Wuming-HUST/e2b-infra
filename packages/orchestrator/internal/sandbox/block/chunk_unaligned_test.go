package block

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type recordingChunkReader struct {
	*bytes.Reader
	mu      sync.Mutex
	offsets []int64
}

func (r *recordingChunkReader) ReadAt(_ context.Context, b []byte, off int64) (int, error) {
	r.mu.Lock()
	r.offsets = append(r.offsets, off)
	r.mu.Unlock()
	return r.Reader.ReadAt(b, off)
}

func (r *recordingChunkReader) Size(context.Context) (int64, error) {
	return r.Reader.Size(), nil
}

func TestFullFetchChunker_UnalignedRange(t *testing.T) {
	for _, tc := range []struct {
		name      string
		blockSize int64
		off       int64
		length    int
		fetches   []int64
	}{
		{"reported_range_4KiB_blocks", header.PageSize, 3956736, 790528, []int64{0, storage.MemoryChunkSize}},
		{"inside_4MiB_block", storage.MemoryChunkSize, header.PageSize, int(header.PageSize), []int64{0}},
		{"unaligned_to_page", header.PageSize, header.PageSize - 1, 2, []int64{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, 2*storage.MemoryChunkSize)
			for i := range data {
				data[i] = byte(i / int(header.PageSize) % 251)
			}
			upstream := &recordingChunkReader{Reader: bytes.NewReader(data)}
			chunker, err := NewFullFetchChunker(int64(len(data)), tc.blockSize, upstream,
				filepath.Join(t.TempDir(), "cache"), newTestMetrics(t))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, chunker.Close()) })

			got := make([]byte, tc.length)
			n, err := chunker.ReadAt(t.Context(), got, tc.off)
			t.Logf("cache blockSize=%d, upstream offsets=%v", tc.blockSize, upstream.offsets)
			require.NoError(t, err)
			require.Equal(t, len(got), n)
			require.Equal(t, data[tc.off:tc.off+int64(tc.length)], got)
			slices.Sort(upstream.offsets)
			require.Equal(t, tc.fetches, upstream.offsets)

			// Re-reading the same range must use the completed cache entries.
			n, err = chunker.ReadAt(t.Context(), got, tc.off)
			require.NoError(t, err)
			require.Equal(t, len(got), n)
			require.Equal(t, tc.fetches, upstream.offsets)
		})
	}
}

func TestCache_UnalignedSliceRequiresCoveredBlocks(t *testing.T) {
	cache, err := NewCache(2*header.PageSize, header.PageSize, filepath.Join(t.TempDir(), "cache"), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })

	// An empty read needs no cache entries, even at an unaligned offset.
	empty, err := cache.Slice(header.PageSize-1, 0)
	require.NoError(t, err)
	require.Empty(t, empty)

	_, err = cache.WriteAt(bytes.Repeat([]byte{0x5a}, int(header.PageSize)), 0)
	require.NoError(t, err)
	_, err = cache.Slice(header.PageSize-1, 2)
	require.ErrorAs(t, err, &BytesNotAvailableError{})

	_, err = cache.WriteAt(bytes.Repeat([]byte{0xa5}, int(header.PageSize)), header.PageSize)
	require.NoError(t, err)
	got, err := cache.Slice(header.PageSize-1, 2)
	require.NoError(t, err)
	require.Equal(t, []byte{0x5a, 0xa5}, got)
}

func TestCache_UnalignedSliceAtEOF(t *testing.T) {
	data := bytes.Repeat([]byte{0x5a}, int(header.PageSize)+17)
	cache, err := NewCache(int64(len(data)), header.PageSize, filepath.Join(t.TempDir(), "cache"), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })
	_, err = cache.WriteAt(data, 0)
	require.NoError(t, err)
	got, err := cache.Slice(header.PageSize+1, 2*header.PageSize)
	require.NoError(t, err)
	require.Equal(t, data[header.PageSize+1:], got)
}

func TestStreamingChunker_UnalignedRange(t *testing.T) {
	data := bytes.Repeat([]byte{0x5a}, storage.MemoryChunkSize)
	chunker, err := NewStreamingChunker(int64(len(data)), header.PageSize,
		&fastUpstream{data: data, blockSize: header.PageSize},
		filepath.Join(t.TempDir(), "cache"), newTestMetrics(t), 0, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, chunker.Close()) })
	got := make([]byte, 2)
	n, err := chunker.ReadAt(t.Context(), got, header.PageSize-1)
	require.NoError(t, err)
	require.Equal(t, len(got), n)
	require.Equal(t, data[header.PageSize-1:header.PageSize+1], got)
}
