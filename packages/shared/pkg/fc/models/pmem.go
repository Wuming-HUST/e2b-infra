package models

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// PmemConfig binds an immutable, fully materialized file to a separate device GPA.
// Keep this extension in sync with PmemConfig in firecracker.yml.
type PmemConfig struct {
	PmemID       string `json:"pmem_id"`
	PathOnHost   string `json:"path_on_host"`
	ContentID    string `json:"content_id"`
	GuestAddress uint64 `json:"guest_address"`
	Size         uint64 `json:"size"`
	FileOffset   uint64 `json:"file_offset"`
	ReadOnly     bool   `json:"read_only"`
}

var pmemDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var pmemID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

const PmemAlignment uint64 = 128 << 20

func (p PmemConfig) ValidateBinding() error {
	if !p.ReadOnly || !pmemID.MatchString(p.PmemID) || !pmemDigest.MatchString(p.ContentID) || !filepath.IsAbs(p.PathOnHost) || p.Size == 0 || p.Size%PmemAlignment != 0 || p.GuestAddress < 1<<32 || p.GuestAddress%PmemAlignment != 0 || p.GuestAddress >= 1<<40 || p.Size > (1<<40)-p.GuestAddress || p.FileOffset%4096 != 0 || p.FileOffset > (1<<63-1)-p.Size {
		return fmt.Errorf("invalid immutable pmem binding")
	}
	return nil
}
