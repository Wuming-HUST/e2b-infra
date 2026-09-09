package template

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

const (
	sha256HexLength = sha256.Size * 2
	// V1 references may point at images built with stale bytes in sparse ranges.
	// Keep existing content-addressed files intact for VMs that still map them.
	memoryImageReferenceVersion = "v2"
)

func checksumFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.CopyBuffer(digest, file, make([]byte, storage.MemoryChunkSize)); err != nil {
		return "", fmt.Errorf("checksum memory image %q: %w", path, err)
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}

func validateMaterializedImage(path string, expectedSize int64, expectedDigest string) error {
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
	if info.Mode().Perm()&0o222 != 0 {
		return fmt.Errorf("materialized memory image %q is writable: mode %o", path, info.Mode().Perm())
	}

	actualDigest, err := checksumFile(path)
	if err != nil {
		return err
	}
	if actualDigest != expectedDigest {
		return fmt.Errorf("materialized memory image %q checksum is %s, expected %s", path, actualDigest, expectedDigest)
	}

	return nil
}

func memoryImageReference(outputDir, key string) string {
	return filepath.Join(outputDir, key+"."+memoryImageReferenceVersion+".ref")
}

func readMemoryImageReference(outputDir, key string, expectedSize int64) (string, error) {
	reference := memoryImageReference(outputDir, key)
	data, err := os.ReadFile(reference)
	if err != nil {
		return "", err
	}

	digest := strings.TrimSpace(string(data))
	decoded, decodeErr := hex.DecodeString(digest)
	if len(digest) != sha256HexLength || decodeErr != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("invalid memory image digest in %q", reference)
	}

	path := filepath.Join(outputDir, digest+".raw")
	if err := validateMaterializedImage(path, expectedSize, digest); err != nil {
		return "", err
	}

	return path, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()

	return directory.Sync()
}

func writeMemoryImageReference(outputDir, key, digest string) error {
	temporary, err := os.CreateTemp(outputDir, key+"-*.ref.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	defer temporary.Close()

	if _, err := fmt.Fprintf(temporary, "%s\n", digest); err != nil {
		return err
	}
	if err := temporary.Chmod(0o444); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, memoryImageReference(outputDir, key)); err != nil {
		return err
	}

	return syncDirectory(outputDir)
}

func hashChunk(digest hash.Hash, chunk []byte) error {
	written, err := digest.Write(chunk)
	if err != nil {
		return err
	}
	if written != len(chunk) {
		return fmt.Errorf("short checksum write: got %d, expected %d", written, len(chunk))
	}

	return nil
}

// MaterializeMemoryImage writes the logical layered memory device into one
// immutable, content-addressed sparse file. Publication uses link(2), so a
// file already mapped by Firecracker can never be replaced.
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

	if path, err := readMemoryImageReference(outputDir, key, size); err == nil {
		return path, nil
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
	digest := sha256.New()
	for offset := int64(0); offset < size; {
		length := min(int64(len(buffer)), size-offset)
		chunk := buffer[:length]
		// build.File.ReadAt leaves uuid.Nil mappings untouched. Reusing the
		// previous chunk's bytes would turn logical zeroes into nonzero data.
		clear(chunk)
		n, readErr := source.ReadAt(ctx, chunk, offset)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", fmt.Errorf("read memory image at offset %d: %w", offset, readErr)
		}
		if int64(n) != length {
			return "", fmt.Errorf("short memory image read at offset %d: got %d, expected %d", offset, n, length)
		}
		if err := hashChunk(digest, chunk); err != nil {
			return "", fmt.Errorf("checksum memory image at offset %d: %w", offset, err)
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

	if err := temporary.Chmod(0o444); err != nil {
		return "", fmt.Errorf("make memory image immutable: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync memory image: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close memory image: %w", err)
	}

	digestHex := hex.EncodeToString(digest.Sum(nil))
	finalPath := filepath.Join(outputDir, digestHex+".raw")
	if err := os.Link(temporaryPath, finalPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("publish memory image: %w", err)
		}
		if err := validateMaterializedImage(finalPath, size, digestHex); err != nil {
			return "", err
		}
	} else if err := syncDirectory(outputDir); err != nil {
		return "", fmt.Errorf("sync memory image directory: %w", err)
	}

	if err := writeMemoryImageReference(outputDir, key, digestHex); err != nil {
		return "", fmt.Errorf("publish memory image reference: %w", err)
	}

	return finalPath, nil
}
