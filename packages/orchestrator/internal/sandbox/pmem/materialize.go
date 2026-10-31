package pmem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// Cache never evicts or replaces a published inode. Active mappings can outlive
// template cache entries. Reclamation must occur during an explicit offline GC.
// A cancelled waiter does not cancel materialization shared with other VMs.
type Cache struct {
	dir  string
	mu   sync.Mutex
	jobs map[Image]*imageJob
}
type imageJob struct {
	done chan struct{}
	path string
	err  error
}
type OpenSource func(context.Context) (storage.SeekableReader, error)

func NewCache(dir string) *Cache { return &Cache{dir: dir, jobs: make(map[Image]*imageJob)} }

func (c *Cache) Materialize(ctx context.Context, image Image, open OpenSource) (string, error) {
	l := Layout{FormatVersion: 1, Mode: Mode, Lower: image, GuestAddress: 1 << 38}
	if err := l.Validate(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	j := c.jobs[image]
	if j == nil {
		j = &imageJob{done: make(chan struct{})}
		c.jobs[image] = j
		go func() {
			work, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			j.path, j.err = c.materialize(work, image, open)
			c.mu.Lock()
			if j.err != nil {
				delete(c.jobs, image)
			}
			close(j.done)
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-j.done:
	}
	if j.err != nil {
		return "", j.err
	}
	// Revalidate cached bytes before binding, including externally altered files.
	if err := validateFile(ctx, j.path, image); err != nil {
		return "", err
	}
	return j.path, nil
}

func validateFile(ctx context.Context, path string, image Image) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || uint64(st.Size()) != image.Size || st.Mode().Perm()&0222 != 0 {
		return fmt.Errorf("invalid materialized lower size/type/mode")
	}
	h := sha256.New()
	buffer := make([]byte, 4<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, e := f.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != image.SHA256 {
		return fmt.Errorf("pmem lower SHA256 mismatch")
	}
	return nil
}

func (c *Cache) materialize(ctx context.Context, image Image, open OpenSource) (string, error) {
	if c.dir == "" {
		return "", fmt.Errorf("pmem cache directory is empty")
	}
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return "", err
	}
	final := filepath.Join(c.dir, image.SHA256+".raw")
	if err := validateFile(ctx, final, image); err == nil {
		return final, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	source, err := open(ctx)
	if err != nil {
		return "", err
	}
	if closer, ok := source.(io.Closer); ok {
		defer closer.Close()
	}
	if source == nil {
		return "", fmt.Errorf("pmem source is nil")
	}
	size, err := source.Size(ctx)
	if err != nil {
		return "", err
	}
	if size < 0 || uint64(size) != image.Size {
		return "", fmt.Errorf("pmem lower source size mismatch")
	}
	f, err := os.CreateTemp(c.dir, ".lower-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	buffer := make([]byte, 4<<20)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk := buffer[:min(int64(len(buffer)), size-offset)]
		clear(chunk)
		n, e := source.ReadAt(ctx, chunk, offset)
		if e != nil && !errors.Is(e, io.EOF) {
			return "", e
		}
		if n != len(chunk) {
			return "", io.ErrUnexpectedEOF
		}
		_, _ = h.Write(chunk)
		if _, err = f.Write(chunk); err != nil {
			return "", err
		}
		offset += int64(n)
	}
	if hex.EncodeToString(h.Sum(nil)) != image.SHA256 {
		return "", fmt.Errorf("pmem lower SHA256 mismatch")
	}
	if err = f.Chmod(0444); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Link(f.Name(), final); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err = validateFile(ctx, final, image); err != nil {
		return "", err
	}
	dir, err := os.Open(c.dir)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return "", err
	}
	return final, nil
}
