package template

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type checkpointTemplateStub struct {
	Template
	mem          block.ReadonlyDevice
	root         block.ReadonlyDevice
	state        File
	meta         metadata.Template
	payloadCalls int
}

func (s *checkpointTemplateStub) Memfile(ctx context.Context) (block.ReadonlyDevice, error) {
	s.payloadCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.mem, nil
}
func (s *checkpointTemplateStub) Rootfs() (block.ReadonlyDevice, error) {
	s.payloadCalls++
	return s.root, nil
}
func (s *checkpointTemplateStub) Snapfile() (File, error)              { s.payloadCalls++; return s.state, nil }
func (s *checkpointTemplateStub) Metadata() (metadata.Template, error) { return s.meta, nil }

func checkpointTestSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func checkpointTestHeaderSHA(t *testing.T, h *header.Header) string {
	t.Helper()
	data, err := header.Serialize(h.Metadata, h.Mapping)
	if err != nil {
		t.Fatal(err)
	}
	return checkpointTestSHA(data)
}
func newCheckpointTemplate(t *testing.T) *checkpointTemplateStub {
	t.Helper()
	dir := t.TempDir()
	const page = 4096
	memBytes := bytes.Repeat([]byte{0x5A}, page)
	rootBytes := bytes.Repeat([]byte{0x7B}, page)
	stateBytes := []byte("checkpoint: ordered RAM regions and frozen device state")
	paths := []string{filepath.Join(dir, "memory"), filepath.Join(dir, "rootfs"), filepath.Join(dir, "state")}
	for i, data := range [][]byte{memBytes, rootBytes, stateBytes} {
		if err := os.WriteFile(paths[i], data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mem, err := block.NewLocal(paths[0], page, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mem.Close(); err != nil {
			t.Error(err)
		}
	})
	root, err := block.NewLocal(paths[1], page, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return &checkpointTemplateStub{
		mem: mem, root: root, state: NewLocalFileLink(paths[2]),
		meta: metadata.Template{Version: metadata.CurrentVersion, Checkpoint: &metadata.Checkpoint{
			FormatVersion: 1, RAMExportMode: "full_logical", LogicalRAMSize: page, LogicalRAMSHA256: checkpointTestSHA(memBytes),
			RAMHeaderSHA256: checkpointTestHeaderSHA(t, mem.Header()), RootfsHeaderSHA256: checkpointTestHeaderSHA(t, root.Header()),
			RootfsDiffSHA256: checkpointTestSHA(nil), FCStateSHA256: checkpointTestSHA(stateBytes),
		}},
	}
}

func TestCheckpointValidateAcceptsSealedFullAndDelta(t *testing.T) {
	for _, mode := range []string{"full_logical", "uffd_delta"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newCheckpointTemplate(t)
			fixture.meta.Checkpoint.RAMExportMode = mode
			if mode == "uffd_delta" {
				fixture.meta.Checkpoint.LogicalRAMSHA256 = ""
			}
			if err := ValidateCheckpoint(t.Context(), fixture); err != nil {
				t.Fatalf("valid %s checkpoint rejected: %v", mode, err)
			}
		})
	}
}

func TestCheckpointValidateRejectsStateHeaderAndSizeMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *checkpointTemplateStub)
		want   string
	}{
		{"state-bytes", func(t *testing.T, s *checkpointTemplateStub) {
			if err := os.WriteFile(s.state.Path(), []byte("different FC checkpoint"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "FC state checksum mismatch"},
		{"memory-header", func(_ *testing.T, s *checkpointTemplateStub) { s.mem.Header().Metadata.Generation++ }, "header checksum mismatch"},
		{"rootfs-header", func(_ *testing.T, s *checkpointTemplateStub) { s.root.Header().Metadata.Generation++ }, "header checksum mismatch"},
		{"logical-size", func(_ *testing.T, s *checkpointTemplateStub) { s.meta.Checkpoint.LogicalRAMSize += 4096 }, "logical RAM size mismatch"},
		{"missing-memory-header-hash", func(_ *testing.T, s *checkpointTemplateStub) { s.meta.Checkpoint.RAMHeaderSHA256 = "" }, "header checksum mismatch"},
		{"missing-rootfs-header-hash", func(_ *testing.T, s *checkpointTemplateStub) { s.meta.Checkpoint.RootfsHeaderSHA256 = "" }, "header checksum mismatch"},
		{"missing-state-hash", func(_ *testing.T, s *checkpointTemplateStub) { s.meta.Checkpoint.FCStateSHA256 = "" }, "FC state checksum mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCheckpointTemplate(t)
			tc.mutate(t, fixture)
			err := ValidateCheckpoint(t.Context(), fixture)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mismatch not rejected at boundary %q: %v", tc.want, err)
			}
		})
	}
}

func TestCheckpointValidateRejectsUnsupportedFormatAndUnprovenMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*metadata.Checkpoint)
	}{
		{"format", func(c *metadata.Checkpoint) { c.FormatVersion = 2 }},
		{"tracked-delta", func(c *metadata.Checkpoint) { c.RAMExportMode = "tracked_delta" }},
		{"unknown-mode", func(c *metadata.Checkpoint) { c.RAMExportMode = "" }},
		{"invalid-RAM-hex", func(c *metadata.Checkpoint) { c.LogicalRAMSHA256 = strings.Repeat("g", 64) }},
		{"short-RAM-checksum", func(c *metadata.Checkpoint) { c.LogicalRAMSHA256 = "ab" }},
		{"missing-RAM-checksum", func(c *metadata.Checkpoint) { c.LogicalRAMSHA256 = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCheckpointTemplate(t)
			tc.mutate(fixture.meta.Checkpoint)
			if err := ValidateCheckpoint(t.Context(), fixture); err == nil {
				t.Fatal("unsupported or unproven checkpoint accepted")
			}
		})
	}
}

func TestCheckpointValidateAllowsLegacyWithoutOpeningPayloads(t *testing.T) {
	// All unimplemented methods would panic through the nil embedded Template.
	// No checkpoint marker means immutable legacy templates retain compatibility.
	legacy := &checkpointTemplateStub{meta: metadata.Template{Version: metadata.CurrentVersion}}
	if err := ValidateCheckpoint(t.Context(), legacy); err != nil {
		t.Fatalf("legacy template rejected: %v", err)
	}
	if legacy.payloadCalls != 0 {
		t.Fatalf("legacy restore opened %d payloads", legacy.payloadCalls)
	}
}
