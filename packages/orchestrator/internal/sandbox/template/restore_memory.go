package template

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/memorybacking"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type memoryImageProvider interface {
	memoryImagePath() (string, bool)
}

// MemoryViewProvider explicitly transfers an independently owned logical source.
// A borrowed localtemplate memfile cannot safely back a shared NBD lifetime.
type MemoryViewProvider interface {
	AcquireMemoryView(context.Context) (block.ReadonlyDevice, error)
}

func AcquireMemoryView(ctx context.Context, source block.ReadonlyDevice) (block.ReadonlyDevice, error) {
	provider, ok := source.(MemoryViewProvider)
	if !ok {
		return nil, fmt.Errorf("shared NBD RAM source does not support owned memory views")
	}
	return provider.AcquireMemoryView(ctx)
}

type restoreMemoryTemplate struct {
	Template
	allowFileBackend bool
}

func withRestoreMemoryPolicy(t Template, allowFileBackend bool) Template {
	return &restoreMemoryTemplate{
		Template:         t,
		allowFileBackend: allowFileBackend,
	}
}

func (t *restoreMemoryTemplate) memoryImagePath() (string, bool) {
	if !t.allowFileBackend {
		return "", false
	}

	provider, ok := t.Template.(memoryImageProvider)
	if !ok {
		return "", false
	}

	return provider.memoryImagePath()
}

// FileMemoryBackendPath returns a fully published shared image only when this
// specific restore is eligible. An unavailable image means the caller must use
// UFFD; it is not an error because background materialization is asynchronous.
func FileMemoryBackendPath(t Template) (string, bool) {
	provider, ok := t.(memoryImageProvider)
	if !ok {
		return "", false
	}

	return provider.memoryImagePath()
}

// MemoryBackingIdentity binds an immutable layered RAM manifest to the exact
// serialized FC state that defines the ordered logical RAM regions. Build IDs
// name immutable objects under the existing storage contract; this digest hashes
// the manifest, not unread remote RAM bytes. It must never be called a RAM hash.
func MemoryBackingIdentity(ctx context.Context, t Template) (memorybacking.SnapshotIdentity, error) {
	source, err := t.Memfile(ctx)
	if err != nil {
		return memorybacking.SnapshotIdentity{}, err
	}
	h := source.Header()
	if h == nil || h.Metadata == nil {
		return memorybacking.SnapshotIdentity{}, fmt.Errorf("memory source has no immutable header")
	}
	data, err := header.Serialize(h.Metadata, h.Mapping)
	if err != nil {
		return memorybacking.SnapshotIdentity{}, err
	}
	size, err := source.Size(ctx)
	if err != nil {
		return memorybacking.SnapshotIdentity{}, err
	}
	if size <= 0 || uint64(size) != h.Metadata.Size || h.Metadata.BlockSize != uint64(source.BlockSize()) {
		return memorybacking.SnapshotIdentity{}, fmt.Errorf("memory source disagrees with manifest")
	}
	state, err := t.Snapfile()
	if err != nil {
		return memorybacking.SnapshotIdentity{}, err
	}
	layoutDigest, err := checksumFile(state.Path())
	if err != nil {
		return memorybacking.SnapshotIdentity{}, fmt.Errorf("hash FC layout state: %w", err)
	}
	manifestDigest := sha256.Sum256(data)
	return memorybacking.SnapshotIdentity{
		Kind: "ram", ContentID: fmt.Sprintf("manifest-sha256:%x", manifestDigest),
		LayoutID:    fmt.Sprintf("fc-state-sha256:%s;page-size:%d", layoutDigest, source.BlockSize()),
		LogicalSize: size,
	}, nil
}

// ValidateCheckpoint rejects a state/header mismatch before any vCPU resumes.
// Legacy immutable templates have no checkpoint marker. Remote RAM payloads
// remain subject to the storage version contract; this does not checksum unread
// remote bytes or replace the target full-logical oracle.
func ValidateCheckpoint(ctx context.Context, t Template) error {
	meta, err := t.Metadata()
	if err != nil {
		return err
	}
	c := meta.Checkpoint
	if c == nil {
		if meta.Rootfs != nil || meta.Version >= 3 {
			return fmt.Errorf("pmem restore requires a new sealed checkpoint")
		}
		return nil
	}
	if meta.Rootfs != nil {
		if err := meta.Rootfs.Validate(); err != nil {
			return err
		}
		if meta.Version != 3 || c.FormatVersion != 3 || c.RootfsLayoutSHA256 != meta.Rootfs.Digest() {
			return fmt.Errorf("invalid pmem checkpoint layout binding")
		}
	} else if c.FormatVersion == 3 || meta.Version >= 3 {
		return fmt.Errorf("missing pmem checkpoint layout")
	}
	format := c.FormatVersion
	if format == 3 {
		if c.RAMExportMode == "tracked_delta" {
			format = 2
		} else {
			format = 1
		}
	}
	legacy := format == 1 && (c.RAMExportMode == "full_logical" || c.RAMExportMode == "uffd_delta")
	tracked := format == 2 && c.RAMExportMode == "tracked_delta"
	if !legacy && !tracked {
		return fmt.Errorf("unsupported checkpoint format or RAM export mode")
	}
	if tracked {
		digest, err := hex.DecodeString(c.RAMPayloadSHA256)
		if err != nil || len(digest) != sha256.Size || c.RAMTrackingVersion != 1 || c.LogicalRAMSHA256 != "" {
			return fmt.Errorf("invalid tracked checkpoint contract or payload checksum")
		}
	}
	if c.RAMExportMode == "full_logical" {
		digest, err := hex.DecodeString(c.LogicalRAMSHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("invalid logical RAM checksum")
		}
	}
	mem, err := t.Memfile(ctx)
	if err != nil {
		return err
	}
	if tracked {
		if err := ValidateTrackedPayloadLayout(mem.Header(), c.RAMPayloadSize); err != nil {
			return err
		}
	}
	root, err := t.Rootfs()
	if err != nil {
		return err
	}
	for _, item := range []struct {
		device block.ReadonlyDevice
		digest string
	}{{mem, c.RAMHeaderSHA256}, {root, c.RootfsHeaderSHA256}} {
		h := item.device.Header()
		if h == nil || h.Metadata == nil {
			return fmt.Errorf("missing checkpoint header")
		}
		b, err := header.Serialize(h.Metadata, h.Mapping)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(b)
		if fmt.Sprintf("%x", digest) != item.digest {
			return fmt.Errorf("checkpoint header checksum mismatch")
		}
	}
	size, err := mem.Size(ctx)
	if err != nil {
		return fmt.Errorf("read checkpoint logical RAM size: %w", err)
	}
	if size < 0 || uint64(size) != c.LogicalRAMSize {
		return fmt.Errorf("checkpoint logical RAM size mismatch: got %d, want %d", size, c.LogicalRAMSize)
	}
	state, err := t.Snapfile()
	if err != nil {
		return err
	}
	digest, err := checksumFile(state.Path())
	if err != nil {
		return err
	}
	if digest != c.FCStateSHA256 {
		return fmt.Errorf("checkpoint FC state checksum mismatch")
	}
	return nil
}

// ValidateTrackedPayloadLayout binds the packed payload length to the extents
// supplied by this generation. Inherited extents require their original objects.
func ValidateTrackedPayloadLayout(h *header.Header, payloadSize uint64) error {
	if h == nil || h.Metadata == nil || h.Metadata.BlockSize != 4096 || h.Metadata.Size == 0 || h.Metadata.Size%4096 != 0 {
		return fmt.Errorf("invalid tracked checkpoint RAM header")
	}
	var packed, covered uint64
	for _, part := range h.Mapping {
		if part == nil || part.Offset != covered || covered > h.Metadata.Size || part.Length == 0 ||
			part.Length > h.Metadata.Size-covered || part.Length%4096 != 0 || part.BuildStorageOffset%4096 != 0 {
			return fmt.Errorf("invalid tracked checkpoint logical extent")
		}
		covered += part.Length
		if part.BuildId != h.Metadata.BuildId {
			continue
		}
		if part.BuildStorageOffset != packed || packed > payloadSize || part.Length > payloadSize-packed {
			return fmt.Errorf("tracked RAM payload extent does not match packed capture")
		}
		packed += part.Length
	}
	if covered != h.Metadata.Size || packed != payloadSize || payloadSize > h.Metadata.Size {
		return fmt.Errorf("tracked RAM payload length differs from header")
	}
	return nil
}
