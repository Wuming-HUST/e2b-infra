package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper to create a FileSystemStorageProvider rooted in a temp directory.
func newTempProvider(t *testing.T) *fsStorage {
	t.Helper()

	base := t.TempDir()
	p := newFileSystemStorage(base)

	return p
}

func TestOpenObject_Write_Exists_WriteTo(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)
	ctx := t.Context()

	obj, err := p.OpenBlob(ctx, filepath.Join("sub", "file.txt"), MetadataObjectType)
	require.NoError(t, err)

	contents := []byte("hello world")
	// write via Write
	err = obj.Put(t.Context(), contents)
	require.NoError(t, err)

	// check Size
	exists, err := obj.Exists(t.Context())
	require.NoError(t, err)
	require.True(t, exists)

	// read the entire file back via WriteTo
	data, err := GetBlob(t.Context(), obj)
	require.NoError(t, err)
	require.Equal(t, contents, data)
}

func TestFSPut(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)
	ctx := t.Context()

	// create a separate source file on disk
	srcPath := filepath.Join(t.TempDir(), "src.txt")
	const payload = "copy me please"
	require.NoError(t, os.WriteFile(srcPath, []byte(payload), 0o600))

	obj, err := p.OpenBlob(ctx, "copy/dst.txt", UnknownObjectType)
	require.NoError(t, err)

	require.NoError(t, obj.Put(t.Context(), []byte(payload)))

	data, err := GetBlob(t.Context(), obj)
	require.NoError(t, err)
	require.Equal(t, payload, string(data))
}

func TestDelete(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)
	ctx := t.Context()

	obj, err := p.OpenBlob(ctx, "to/delete.txt", 0)
	require.NoError(t, err)

	err = obj.Put(t.Context(), []byte("bye"))
	require.NoError(t, err)

	exists, err := obj.Exists(t.Context())
	require.NoError(t, err)
	assert.True(t, exists)

	err = p.DeleteObjectsWithPrefix(t.Context(), "to/delete.txt")
	require.NoError(t, err)

	// subsequent Size call should fail with ErrorObjectNotExist
	exists, err = obj.Exists(t.Context())
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestDeleteObjectsWithPrefix(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)
	ctx := t.Context()

	paths := []string{
		"data/a.txt",
		"data/b.txt",
		"data/sub/c.txt",
	}
	for _, pth := range paths {
		obj, err := p.OpenBlob(ctx, pth, UnknownObjectType)
		require.NoError(t, err)
		err = obj.Put(t.Context(), []byte("x"))
		require.NoError(t, err)
	}

	// remove the entire "data" prefix
	require.NoError(t, p.DeleteObjectsWithPrefix(ctx, "data"))

	for _, pth := range paths {
		full := filepath.Join(p.GetDetails()[len("[Local file storage, base path set to "):len(p.GetDetails())-1], pth) // derive basePath
		_, err := os.Stat(full)
		require.True(t, os.IsNotExist(err))
	}
}

func TestWriteToNonExistentObject(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)

	ctx := t.Context()
	obj, err := p.OpenBlob(ctx, "missing/file.txt", UnknownObjectType)
	require.NoError(t, err)

	_, err = GetBlob(t.Context(), obj)
	require.ErrorIs(t, err, ErrObjectNotExist)
}

// jsonBlob returns a deterministic valid JSON document of roughly size bytes.
func jsonBlob(tag string, size int) []byte {
	pad := make([]byte, size)
	for i := range pad {
		pad[i] = byte('a' + (i+len(tag))%26)
	}

	return []byte(`{"tag":"` + tag + `","pad":"` + string(pad) + `"}`)
}

// TestPutConcurrentNeverTearsJSON is the regression test for the metadata
// publish race: two writers Put different full contents of the same object
// concurrently (layer snapshot metadata vs optimize metadata). The atomic
// temp+rename write must guarantee the object always holds exactly one of
// the written versions — never an interleaved, torn document.
func TestPutConcurrentNeverTearsJSON(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)

	ctx := t.Context()
	obj, err := p.OpenBlob(ctx, "template/metadata.json", MetadataObjectType)
	require.NoError(t, err)

	const rounds = 50

	var wg sync.WaitGroup
	versions := []string{"finalize-original", "optimize-prefetch"}

	for range rounds {
		for _, v := range versions {
			wg.Add(1)
			go func(payload []byte) {
				defer wg.Done()
				assert.NoError(t, obj.Put(ctx, payload))
			}(jsonBlob(v, 4096))
		}
	}

	wg.Wait()

	data, err := GetBlob(ctx, obj)
	require.NoError(t, err)

	var parsed struct {
		Tag string `json:"tag"`
	}
	require.NoError(t, json.Unmarshal(data, &parsed), "object torn after concurrent Puts: %d bytes", len(data))
	assert.Contains(t, versions, parsed.Tag)
}

// TestPutShorterRewriteHasNoStaleTail re-pins the original O_TRUNC bug
// (eb976079c) through the atomic write: rewriting with shorter content must
// not leave the previous, longer content behind.
func TestPutShorterRewriteHasNoStaleTail(t *testing.T) {
	t.Parallel()
	p := newTempProvider(t)

	ctx := t.Context()
	obj, err := p.OpenBlob(ctx, "template/metadata.json", MetadataObjectType)
	require.NoError(t, err)

	long := jsonBlob("long", 8192)
	short := jsonBlob("short", 64)

	require.NoError(t, obj.Put(ctx, long))
	require.NoError(t, obj.Put(ctx, short))

	data, err := GetBlob(ctx, obj)
	require.NoError(t, err)
	require.Len(t, data, len(short))

	var parsed struct {
		Tag string `json:"tag"`
	}
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, "short", parsed.Tag)
}
