package memorybacking

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type byteReader struct {
	data   []byte
	short  bool
	closes atomic.Int32
}

func (r *byteReader) Size(context.Context) (int64, error) { return int64(len(r.data)), nil }
func (r *byteReader) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if r.short {
		return 0, nil
	}
	return bytes.NewReader(r.data).ReadAt(p, off)
}
func (r *byteReader) Close() error { r.closes.Add(1); return nil }

func TestLocalSourceBoundsFullReadsAndBorrowedOwnership(t *testing.T) {
	ctx := testContext(t)
	data := make([]byte, testIdentity.LogicalSize)
	for i := range data {
		data[i] = byte(i % 253)
	}
	// An explicit zero extent remains a zero extent; unaccessed nonzero bytes
	// remain nonzero and must never be inferred from residency.
	clear(data[4096:6144])
	r := &byteReader{data: data}
	s, err := NewLocalSource(ctx, testIdentity, r, SourceOptions{MaxReadBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range [][2]int64{{0, 4096}, {4095, 2}, {6140, 10}, {8191, 1}, {4096, 2048}, {8192, 0}} {
		p := make([]byte, rr[1])
		n, err := s.ReadAt(ctx, p, rr[0])
		if err != nil || n != len(p) || !bytes.Equal(p, data[rr[0]:rr[0]+rr[1]]) {
			t.Fatalf("range %v n=%d err=%v", rr, n, err)
		}
	}
	for _, rr := range [][2]int64{{-1, 1}, {8192, 1}, {math.MaxInt64, 2}, {0, 4097}} {
		if _, err := s.ReadAt(ctx, make([]byte, rr[1]), rr[0]); err == nil {
			t.Fatalf("accepted invalid range %v", rr)
		}
	}
	r.short = true
	if _, err := s.ReadAt(ctx, make([]byte, 4), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short read succeeded: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if r.closes.Load() != 0 {
		t.Fatal("borrowed template reader closed")
	}
	if _, err := s.ReadAt(ctx, make([]byte, 1), 0); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	owned, err := NewLocalSource(ctx, testIdentity, r, SourceOptions{Close: r.Close})
	if err != nil {
		t.Fatal(err)
	}
	_ = owned.Close()
	_ = owned.Close()
	if r.closes.Load() != 1 {
		t.Fatal("owned reader close count")
	}
	id := testIdentity
	id.LogicalSize++
	if _, err := NewLocalSource(ctx, id, r, SourceOptions{}); err == nil {
		t.Fatal("accepted wrong size")
	}
}

func TestHTTPSourceDemandRangesAndFaults(t *testing.T) {
	data := make([]byte, testIdentity.LogicalSize)
	for i := range data {
		data[i] = byte(i % 251)
	}
	for _, fault := range []string{"ok", "short", "wrong-range", "changed-version", "ignore-range", "checksum", "trailing", "encoding", "server-error"} {
		t.Run(fault, func(t *testing.T) {
			ctx := testContext(t)
			var requested atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("If-Match") != "\"fixed-version\"" {
					t.Error("request did not pin manifest version")
				}
				w.Header().Set("ETag", "\"fixed-version\"")
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					return
				}
				var start, end int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				requested.Add(end - start + 1)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				payload := append([]byte(nil), data[start:end+1]...)
				switch fault {
				case "wrong-range":
					w.Header().Set("Content-Range", "bytes 0-1/8192")
				case "changed-version":
					w.Header().Set("ETag", "\"new-version\"")
				case "ignore-range":
					w.WriteHeader(200)
					_, _ = w.Write(data)
					return
				case "checksum":
					payload[0] ^= 1
				case "short":
					payload = payload[:len(payload)-1]
				case "trailing":
					payload = append(payload, 1)
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
				case "server-error":
					w.WriteHeader(503)
					return
				}
				// Chunked responses exercise exact-body checks separately from length.
				w.WriteHeader(http.StatusPartialContent)
				w.(http.Flusher).Flush()
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			want := sha256.Sum256(data[4090:4122])
			s, err := NewHTTPSource(ctx, testIdentity, server.URL, HTTPOptions{ETag: "\"fixed-version\"", VerifyRange: func(_ int64, b []byte) error {
				if sha256.Sum256(b) != want {
					return errors.New("digest mismatch")
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			p := make([]byte, 32)
			n, err := s.ReadAt(ctx, p, 4090)
			if fault == "ok" {
				if err != nil || n != len(p) || !bytes.Equal(p, data[4090:4122]) {
					t.Fatalf("n=%d err=%v", n, err)
				}
			} else if err == nil {
				t.Fatalf("fault %s returned success", fault)
			}
			if requested.Load() != 32 {
				t.Fatalf("demand request read %d bytes", requested.Load())
			}
		})
	}
}

func TestHTTPSourceCanceledReadDoesNotCancelPeersAndCloseCancelsReads(t *testing.T) {
	ctx := testContext(t)
	started := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "\"v1\"")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "8192")
			return
		}
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	s, err := NewHTTPSource(ctx, testIdentity, server.URL, HTTPOptions{ETag: "\"v1\"", SourceOptions: SourceOptions{MaxConcurrentReads: 2}})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	one := make(chan error, 1)
	two := make(chan error, 1)
	go func() { _, err := s.ReadAt(canceled, make([]byte, 1), 0); one <- err }()
	<-started
	go func() { _, err := s.ReadAt(ctx, make([]byte, 1), 1); two <- err }()
	<-started
	cancel()
	if err := <-one; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case err := <-two:
		t.Fatalf("peer canceled: %v", err)
	default:
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-two:
		if err == nil {
			t.Fatal("close returned read success")
		}
	case <-ctx.Done():
		t.Fatal("close did not cancel active request")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPManifestAndRequestLimits(t *testing.T) {
	ctx := testContext(t)
	for _, etag := range []string{"", "W/\"weak\"", "unquoted"} {
		if _, err := NewHTTPSource(ctx, testIdentity, "http://unused", HTTPOptions{ETag: etag}); err == nil {
			t.Fatalf("accepted ETag %q", etag)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "\"v1\"")
		w.Header().Set("Content-Length", "4096")
	}))
	defer server.Close()
	if _, err := NewHTTPSource(ctx, testIdentity, server.URL, HTTPOptions{ETag: "\"v1\""}); err == nil {
		t.Fatal("accepted changed logical size")
	}
	// A delayed HEAD observes the caller deadline; it cannot leave a half-ready source.
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer delayed.Close()
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := NewHTTPSource(deadline, testIdentity, delayed.URL, HTTPOptions{ETag: "\"v1\""}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
