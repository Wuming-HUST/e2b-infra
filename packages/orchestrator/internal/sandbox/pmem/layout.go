// Package pmem implements the R1 fully materialized readonly rootfs lower.
package pmem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/e2b-dev/infra/packages/shared/pkg/fc/models"
)

const Mode = "pmem-dax-overlay"

// Image is a complete, section-aligned namespace image, including its padding.
// ObjectPath is derived from the digest; arbitrary URLs and host paths are forbidden.
type Image struct {
	SHA256 string `json:"sha256"`
	Size   uint64 `json:"size"`
}

func (i Image) ObjectPath() string { return "pmem/sha256/" + i.SHA256 + ".raw" }

type Layout struct {
	FormatVersion uint32 `json:"format_version"`
	Mode          string `json:"mode"`
	Lower         Image  `json:"lower"`
	GuestAddress  uint64 `json:"guest_address"`
	// DataOffset is zero for a raw namespace, or 2 MiB for an offline PFN
	// namespace whose struct pages live in private guest RAM (PFN_MODE_RAM).
	DataOffset uint64 `json:"data_offset,omitempty"`
}

func (l Layout) Binding(path string) models.PmemConfig {
	return models.PmemConfig{PmemID: "rootfs-lower", PathOnHost: path, ContentID: l.Lower.SHA256, GuestAddress: l.GuestAddress, Size: l.Lower.Size, ReadOnly: true}
}
func (l Layout) Validate() error {
	if l.FormatVersion != 1 || l.Mode != Mode {
		return fmt.Errorf("unsupported pmem rootfs format")
	}
	if l.DataOffset != 0 && l.DataOffset != 2<<20 {
		return fmt.Errorf("unsupported pmem namespace data offset")
	}
	if l.DataOffset >= l.Lower.Size {
		return fmt.Errorf("pmem namespace has no filesystem data")
	}
	return l.Binding("/binding").ValidateBinding()
}
func (l Layout) Digest() string {
	b, _ := json.Marshal(l)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
