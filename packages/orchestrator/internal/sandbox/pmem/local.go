package pmem

import (
	"context"
	"os"
	"syscall"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

type localSource struct{ *os.File }

func (s *localSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	return s.File.ReadAt(p, off)
}
func (s *localSource) Size(ctx context.Context) (int64, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	st, e := s.Stat()
	if e != nil {
		return 0, e
	}
	return st.Size(), nil
}

// OpenLocal is for explicitly supplied offline template artifacts. Remote
// templates use StorageProvider with the content-addressed object key.
func OpenLocal(path string) OpenSource {
	return func(ctx context.Context) (storage.SeekableReader, error) {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if e != nil {
			return nil, e
		}
		return &localSource{f}, nil
	}
}
