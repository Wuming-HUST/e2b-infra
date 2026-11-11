package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type fsStorage struct {
	basePath string
	opened   map[string]*os.File
}

var _ StorageProvider = (*fsStorage)(nil)

type fsObject struct {
	path string
}

var (
	_ Seekable        = (*fsObject)(nil)
	_ Blob            = (*fsObject)(nil)
	_ StreamingReader = (*fsObject)(nil)
)

type fsRangeReadCloser struct {
	io.Reader

	file *os.File
}

func (r *fsRangeReadCloser) Close() error {
	return r.file.Close()
}

func newFileSystemStorage(basePath string) *fsStorage {
	return &fsStorage{
		basePath: basePath,
		opened:   make(map[string]*os.File),
	}
}

func (s *fsStorage) DeleteObjectsWithPrefix(_ context.Context, prefix string) error {
	filePath := s.getPath(prefix)

	return os.RemoveAll(filePath)
}

func (s *fsStorage) GetDetails() string {
	return fmt.Sprintf("[Local file storage, base path set to %s]", s.basePath)
}

func (s *fsStorage) UploadSignedURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", fmt.Errorf("file system storage does not support signed URLs")
}

func (s *fsStorage) OpenSeekable(_ context.Context, path string, _ SeekableObjectType) (Seekable, error) {
	dir := filepath.Dir(s.getPath(path))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	return &fsObject{
		path: s.getPath(path),
	}, nil
}

func (s *fsStorage) OpenBlob(_ context.Context, path string, _ ObjectType) (Blob, error) {
	dir := filepath.Dir(s.getPath(path))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	return &fsObject{
		path: s.getPath(path),
	}, nil
}

func (s *fsStorage) getPath(path string) string {
	return filepath.Join(s.basePath, path)
}

func (o *fsObject) WriteTo(_ context.Context, dst io.Writer) (int64, error) {
	handle, err := o.getHandle(true)
	if err != nil {
		return 0, err
	}

	defer handle.Close()

	return io.Copy(dst, handle)
}

func (o *fsObject) Put(_ context.Context, data []byte) error {
	return o.writeAtomic(bytes.NewReader(data))
}

func (o *fsObject) StoreFile(_ context.Context, path string) error {
	r, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", path, err)
	}
	defer r.Close()

	return o.writeAtomic(r)
}

// writeAtomic replaces the object content atomically: it writes the full
// content to a temporary file in the same directory and renames it over the
// target. Readers and concurrent writers never observe a partial object:
// without this, two in-flight Puts of the same object (e.g. a layer snapshot
// uploading original template metadata while the optimize phase uploads the
// final metadata with the prefetch mapping) could interleave their writes and
// leave a torn JSON file even with O_TRUNC.
func (o *fsObject) writeAtomic(r io.Reader) error {
	dir := filepath.Dir(o.path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(o.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file for atomic write: %w", err)
	}
	tmpName := tmp.Name()

	abort := func(cause error) error {
		tmp.Close()
		_ = os.Remove(tmpName)
		return cause
	}

	if _, err := io.Copy(tmp, r); err != nil {
		return abort(fmt.Errorf("failed to write temp file %s: %w", tmpName, err))
	}

	if err := tmp.Sync(); err != nil {
		return abort(fmt.Errorf("failed to sync temp file %s: %w", tmpName, err))
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to close temp file %s: %w", tmpName, err)
	}

	// os.CreateTemp creates files with 0600; keep the object readable.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to chmod temp file %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, o.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to rename %s over %s: %w", tmpName, o.path, err)
	}

	return nil
}


func (o *fsObject) OpenRangeReader(_ context.Context, off, length int64) (io.ReadCloser, error) {
	f, err := o.getHandle(true)
	if err != nil {
		return nil, err
	}

	return &fsRangeReadCloser{
		Reader: io.NewSectionReader(f, off, length),
		file:   f,
	}, nil
}

func (o *fsObject) ReadAt(_ context.Context, buff []byte, off int64) (n int, err error) {
	handle, err := o.getHandle(true)
	if err != nil {
		return 0, err
	}
	defer handle.Close()

	return handle.ReadAt(buff, off)
}

func (o *fsObject) Exists(_ context.Context) (bool, error) {
	_, err := os.Stat(o.path)
	if os.IsNotExist(err) {
		return false, nil
	}

	return err == nil, err
}

func (o *fsObject) Size(_ context.Context) (int64, error) {
	handle, err := o.getHandle(true)
	if err != nil {
		return 0, err
	}
	defer handle.Close()

	fileInfo, err := handle.Stat()
	if err != nil {
		return 0, err
	}

	return fileInfo.Size(), nil
}

func (o *fsObject) Delete(_ context.Context) error {
	return os.Remove(o.path)
}

func (o *fsObject) getHandle(checkExistence bool) (*os.File, error) {
	if checkExistence {
		info, err := os.Stat(o.path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, ErrObjectNotExist
			}

			return nil, err
		}

		if info.IsDir() {
			return nil, fmt.Errorf("path %s is a directory", o.path)
		}
	}

	handle, err := os.OpenFile(o.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}

	return handle, nil
}

