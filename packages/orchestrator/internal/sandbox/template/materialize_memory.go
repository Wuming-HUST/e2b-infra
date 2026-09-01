package template

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

func validateMaterializedImage(path string, expectedSize int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("materialized memory image %q is not a regular file", path)
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("materialized memory image %q has size %d, expected %d", path, info.Size(), expectedSize)
	}

	return nil
}

// MaterializeMemoryImage writes the logical layered memory device into one
// immutable sparse file. Publication uses link(2), so concurrent writers can
// never replace an image already mapped by Firecracker processes.
func MaterializeMemoryImage(ctx context.Context, source block.ReadonlyDevice, outputDir, key string) (string, error) {
	if key == "" || filepath.Base(key) != key {
		return "", fmt.Errorf("invalid memory image key %q", key)
	}
	if source == nil {
		return "", errors.New("memory image source is nil")
	}

	size, err := source.Size(ctx)
	if err != nil {
		return "", fmt.Errorf("get memory image size: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return "", fmt.Errorf("create memory image directory: %w", err)
	}

	finalPath := filepath.Join(outputDir, key+".raw")
	if err := validateMaterializedImage(finalPath, size); err == nil {
		return finalPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	temporary, err := os.CreateTemp(outputDir, key+"-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary memory image: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	defer temporary.Close()

	if err := temporary.Truncate(size); err != nil {
		return "", fmt.Errorf("size temporary memory image: %w", err)
	}

	buffer := make([]byte, storage.MemoryChunkSize)
	zeroes := make([]byte, storage.MemoryChunkSize)
	for offset := int64(0); offset < size; {
		length := min(int64(len(buffer)), size-offset)
		chunk := buffer[:length]
		n, readErr := source.ReadAt(ctx, chunk, offset)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", fmt.Errorf("read memory image at offset %d: %w", offset, readErr)
		}
		if int64(n) != length {
			return "", fmt.Errorf("short memory image read at offset %d: got %d, expected %d", offset, n, length)
		}

		if !bytes.Equal(chunk, zeroes[:length]) {
			written, writeErr := temporary.WriteAt(chunk, offset)
			if writeErr != nil {
				return "", fmt.Errorf("write memory image at offset %d: %w", offset, writeErr)
			}
			if int64(written) != length {
				return "", fmt.Errorf("short memory image write at offset %d: got %d, expected %d", offset, written, length)
			}
		}
		offset += length
	}

	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync memory image: %w", err)
	}
	if err := temporary.Chmod(0o444); err != nil {
		return "", fmt.Errorf("make memory image immutable: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close memory image: %w", err)
	}

	if err := os.Link(temporaryPath, finalPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("publish memory image: %w", err)
		}
		if err := validateMaterializedImage(finalPath, size); err != nil {
			return "", err
		}
	}

	return finalPath, nil
}
