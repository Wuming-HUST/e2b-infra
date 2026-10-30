package memorybacking

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPOptions pins an immutable version using a strong ETag from the manifest.
// Without VerifyRange, ETag/size/protocol checks detect version changes and short
// reads, but are not cryptographic verification of unread (or malicious) bytes.
// VerifyRange may enforce hashes from a trusted manifest; its ranges must match
// the requests issued by the caller. Errors fail the read, never synthesize zeros.
type HTTPOptions struct {
	SourceOptions
	Client      *http.Client
	ETag        string
	VerifyRange func(off int64, data []byte) error
	// RequestTimeout bounds each metadata/range request even with a custom client.
	// Zero uses 30 seconds; negative values are rejected.
	RequestTimeout time.Duration
}

type HTTPSource struct {
	id        SnapshotIdentity
	url, etag string
	client    *http.Client
	limit     int
	slots     chan struct{}
	verify    func(int64, []byte) error
	timeout   time.Duration
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	closed    bool
}

func NewHTTPSource(ctx context.Context, id SnapshotIdentity, url string, opts HTTPOptions) (*HTTPSource, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if len(opts.ETag) < 2 || !strings.HasPrefix(opts.ETag, "\"") || !strings.HasSuffix(opts.ETag, "\"") {
		return nil, errors.New("manifest must pin a strong quoted ETag")
	}
	limit, concurrency, err := sourceLimits(opts.SourceOptions)
	if err != nil {
		return nil, err
	}
	if opts.RequestTimeout < 0 {
		return nil, errors.New("negative HTTP request timeout")
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 30 * time.Second
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	headCtx, cancelHead := context.WithTimeout(ctx, opts.RequestTimeout)
	defer cancelHead()
	req, err := http.NewRequestWithContext(headCtx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("If-Match", opts.ETag)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") != opts.ETag || resp.ContentLength != id.LogicalSize {
		return nil, fmt.Errorf("remote manifest mismatch: status=%d size=%d etag=%q", resp.StatusCode, resp.ContentLength, resp.Header.Get("ETag"))
	}
	sourceCtx, cancel := context.WithCancel(context.Background())
	return &HTTPSource{id: id, url: url, etag: opts.ETag, client: client, limit: limit, slots: make(chan struct{}, concurrency), verify: opts.VerifyRange, timeout: opts.RequestTimeout, ctx: sourceCtx, cancel: cancel}, nil
}

func (s *HTTPSource) Identity() SnapshotIdentity { return s.id }

func (s *HTTPSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if err := validateRead(s.id, p, off, s.limit); err != nil {
		return 0, err
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.ctx.Done():
		return 0, io.ErrClosedPipe
	}
	defer func() { <-s.slots }()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	readCtx, cancel := context.WithTimeout(ctx, s.timeout)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := readCtx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(readCtx, http.MethodGet, s.url, nil)
	if err != nil {
		return 0, err
	}
	end := off + int64(len(p)) - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	req.Header.Set("If-Match", s.etag)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	wantRange := fmt.Sprintf("bytes %d-%d/%d", off, end, s.id.LogicalSize)
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != wantRange || resp.Header.Get("ETag") != s.etag || (resp.ContentLength != -1 && resp.ContentLength != int64(len(p))) || (resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") {
		return 0, fmt.Errorf("invalid remote range response: status=%d range=%q length=%s etag=%q", resp.StatusCode, resp.Header.Get("Content-Range"), strconv.FormatInt(resp.ContentLength, 10), resp.Header.Get("ETag"))
	}
	n, err := io.ReadFull(resp.Body, p)
	if err != nil {
		return n, err
	}
	var extra [1]byte
	next, err := resp.Body.Read(extra[:])
	if next != 0 || !errors.Is(err, io.EOF) {
		return n, errors.New("remote range body has trailing data or failed termination")
	}
	if s.verify != nil {
		if err := s.verify(off, p); err != nil {
			return n, fmt.Errorf("remote range checksum: %w", err)
		}
	}
	return n, nil
}

// Close cancels requests before waiting for them; it does not own the HTTP client.
func (s *HTTPSource) Close() error {
	s.cancel()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
