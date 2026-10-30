package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/build"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func checkpointHeaderDigest(h *header.Header) (string, error) {
	if h == nil || h.Metadata == nil {
		return "", errors.New("checkpoint has no header")
	}
	b, err := header.Serialize(h.Metadata, h.Mapping)
	if err != nil {
		return "", err
	}
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:]), nil
}

func checkpointFileDigest(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	b := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(b)
		if n > 0 {
			_, _ = h.Write(b[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	// All checkpoint payloads must be durable before publishing metadata.
	if err := f.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sealSnapshotMetadata publishes the metadata commit marker only after RAM,
// device state, rootfs diff and their headers are complete. The RAM digest was
// calculated during the one process_vm_readv capture, never by rescanning RAM.
func sealSnapshotMetadata(ctx context.Context, s *Snapshot, m metadata.Template) error {
	checkpoint := &metadata.Checkpoint{FormatVersion: 1, RAMExportMode: "uffd_delta"}
	var err error
	checkpoint.RAMHeaderSHA256, err = checkpointHeaderDigest(s.MemfileDiffHeader)
	if err != nil {
		return err
	}
	checkpoint.RootfsHeaderSHA256, err = checkpointHeaderDigest(s.RootfsDiffHeader)
	if err != nil {
		return err
	}
	checkpoint.FCStateSHA256, err = checkpointFileDigest(ctx, s.Snapfile.Path())
	if err != nil {
		return err
	}
	if _, empty := s.RootfsDiff.(*build.NoDiff); empty {
		digest := sha256.Sum256(nil)
		checkpoint.RootfsDiffSHA256 = hex.EncodeToString(digest[:])
	} else {
		rootfsPath, err := s.RootfsDiff.CachePath()
		if err != nil {
			return err
		}
		checkpoint.RootfsDiffSHA256, err = checkpointFileDigest(ctx, rootfsPath)
		if err != nil {
			return err
		}
	}
	memPath, err := s.MemfileDiff.CachePath()
	if err != nil {
		return err
	}
	// Sync the captured RAM file without reading it a second time.
	mem, err := os.Open(memPath)
	if err != nil {
		return err
	}
	err = errors.Join(mem.Sync(), mem.Close())
	if err != nil {
		return err
	}
	h := s.MemfileDiffHeader
	checkpoint.LogicalRAMSize = h.Metadata.Size
	if s.RAMExportMode == "tracked_delta" {
		checkpoint.FormatVersion = 2
		checkpoint.RAMExportMode = "tracked_delta"
		checkpoint.RAMTrackingVersion = 1
		size, err := s.MemfileDiff.Size(ctx)
		if err != nil {
			return fmt.Errorf("read tracked RAM payload size: %w", err)
		}
		if size < 0 {
			return errors.New("negative tracked RAM payload size")
		}
		checkpoint.RAMPayloadSize = uint64(size)
		if err := template.ValidateTrackedPayloadLayout(h, checkpoint.RAMPayloadSize); err != nil {
			return err
		}
		captured, ok := s.MemfileDiff.(interface {
			ProcessMemorySHA256() ([sha256.Size]byte, bool)
		})
		if !ok {
			return errors.New("tracked checkpoint has no captured payload digest")
		}
		digest, ok := captured.ProcessMemorySHA256()
		if !ok {
			return errors.New("tracked checkpoint has no captured payload digest")
		}
		checkpoint.RAMPayloadSHA256 = hex.EncodeToString(digest[:])
		m.Checkpoint = checkpoint
		return publishCheckpointMetadata(ctx, m, s.Metafile.Path())
	}
	var covered uint64
	full := true
	for _, part := range h.Mapping {
		if part.Offset != covered || part.BuildId != h.Metadata.BuildId || part.BuildStorageOffset != covered || part.Length > h.Metadata.Size-covered {
			full = false
			break
		}
		covered += part.Length
	}
	if full && covered == h.Metadata.Size {
		size, err := s.MemfileDiff.Size(ctx)
		if err != nil {
			return err
		}
		if size < 0 || uint64(size) != h.Metadata.Size {
			return errors.New("full logical RAM capture length differs from manifest")
		}
		captured, ok := s.MemfileDiff.(interface {
			ProcessMemorySHA256() ([sha256.Size]byte, bool)
		})
		if !ok {
			return errors.New("full logical checkpoint has no captured RAM digest")
		}
		digest, ok := captured.ProcessMemorySHA256()
		if !ok {
			return errors.New("full logical checkpoint has no captured RAM digest")
		}
		checkpoint.RAMExportMode = "full_logical"
		checkpoint.LogicalRAMSHA256 = hex.EncodeToString(digest[:])
	}
	m.Checkpoint = checkpoint
	return publishCheckpointMetadata(ctx, m, s.Metafile.Path())
}

func publishCheckpointMetadata(ctx context.Context, m metadata.Template, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := m.ToFile(name); err != nil {
		return err
	}
	if _, err := checkpointFileDigest(ctx, name); err != nil {
		return err
	}
	// Link is a no-replace publication: an active snapshot cannot be overwritten.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(name, path); err != nil {
		return fmt.Errorf("publish checkpoint metadata: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
