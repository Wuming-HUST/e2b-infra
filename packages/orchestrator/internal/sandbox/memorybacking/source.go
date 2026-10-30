// Package memorybacking owns immutable snapshot sources and shared device leases.
package memorybacking

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// SnapshotIdentity identifies logical bytes, never an ephemeral path or NBD number.
// LayoutID must identify the ordered logical RAM regions and page size.
type SnapshotIdentity struct {
	Kind        string
	ContentID   string
	LayoutID    string
	LogicalSize int64
}

func (id SnapshotIdentity) Validate() error {
	if id.Kind == "" || id.ContentID == "" || id.LayoutID == "" || id.LogicalSize <= 0 {
		return errors.New("snapshot identity requires kind, content ID, layout ID and positive logical size")
	}
	return nil
}

type SnapshotSource interface {
	Identity() SnapshotIdentity
	ReadAt(context.Context, []byte, int64) (int, error)
	Close() error
}

// LogicalReader is implemented by block.ReadonlyDevice, including layered memfiles.
type LogicalReader interface {
	ReadAt(context.Context, []byte, int64) (int, error)
	Size(context.Context) (int64, error)
}

type SourceOptions struct {
	MaxReadBytes       int
	MaxConcurrentReads int
	// Close transfers ownership of a reader to the source. Nil borrows the reader;
	// this is required when a template owns the shared memfile.
	Close func() error
}

type LocalSource struct {
	id       SnapshotIdentity
	reader   LogicalReader
	limit    int
	slots    chan struct{}
	mu       sync.RWMutex
	closed   bool
	close    func() error
	closeErr error
}

func sourceLimits(o SourceOptions) (int, int, error) {
	if o.MaxReadBytes < 0 || o.MaxConcurrentReads < 0 {
		return 0, 0, errors.New("negative source limit")
	}
	if o.MaxReadBytes == 0 {
		o.MaxReadBytes = 4 << 20
	}
	if o.MaxConcurrentReads == 0 {
		o.MaxConcurrentReads = 8
	}
	return o.MaxReadBytes, o.MaxConcurrentReads, nil
}

func NewLocalSource(ctx context.Context, id SnapshotIdentity, reader LogicalReader, opts SourceOptions) (*LocalSource, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, errors.New("nil snapshot reader")
	}
	limit, concurrency, err := sourceLimits(opts)
	if err != nil {
		return nil, err
	}
	size, err := reader.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot size: %w", err)
	}
	if size != id.LogicalSize {
		return nil, fmt.Errorf("snapshot size %d differs from manifest %d", size, id.LogicalSize)
	}
	return &LocalSource{id: id, reader: reader, limit: limit, slots: make(chan struct{}, concurrency), close: opts.Close}, nil
}

func (s *LocalSource) Identity() SnapshotIdentity { return s.id }

func validateRead(id SnapshotIdentity, p []byte, off int64, limit int) error {
	if off < 0 || off > id.LogicalSize || int64(len(p)) > id.LogicalSize-off {
		return errors.New("snapshot read outside logical size")
	}
	if len(p) > limit {
		return fmt.Errorf("snapshot request exceeds %d byte bound", limit)
	}
	return nil
}

func (s *LocalSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if err := validateRead(s.id, p, off, s.limit); err != nil {
		return 0, err
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-s.slots }()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := s.reader.ReadAt(ctx, p, off)
	if n < 0 || n > len(p) {
		return 0, errors.New("snapshot reader returned invalid count")
	}
	if n != len(p) {
		return n, errors.Join(io.ErrUnexpectedEOF, err)
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

// Close waits for active reads before closing the owned reader, at most once.
func (s *LocalSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if s.close != nil {
			s.closeErr = s.close()
		}
	}
	return s.closeErr
}
