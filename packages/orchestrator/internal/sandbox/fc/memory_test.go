package fc

import (
	"testing"

	"github.com/bits-and-blooms/bitset"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/uffd/memory"
)

func TestExportMemoryRangesUsesLogicalOrderAcrossRegions(t *testing.T) {
	// Logical order deliberately differs from both protocol array and HVA order.
	m := memory.NewMapping([]memory.Region{
		{Offset: 4096, Size: 4096, BaseHostVirtAddr: 0x10000},
		{Offset: 1000, Size: 3096, BaseHostVirtAddr: 0x40000},
		{Offset: 0, Size: 1000, BaseHostVirtAddr: 0x20000},
	})
	ranges, err := exportMemoryRanges(m, bitset.New(2).Set(0).Set(1), 4096)
	require.NoError(t, err)
	require.Equal(t, []block.Range{{Start: 0x20000, Size: 1000}, {Start: 0x40000, Size: 3096}, {Start: 0x10000, Size: 4096}}, ranges)
	ranges, err = exportMemoryRanges(m, bitset.New(2).Set(1), 4096)
	require.NoError(t, err)
	require.Equal(t, []block.Range{{Start: 0x10000, Size: 4096}}, ranges)
}

func TestExportMemoryRangesRejectsInvalidLayout(t *testing.T) {
	for _, regions := range [][]memory.Region{
		nil,
		{{Offset: 4096, Size: 4096, BaseHostVirtAddr: 0x10000}},                                                  // missing logical RAM
		{{Offset: 0, Size: 4097, BaseHostVirtAddr: 0x10000}},                                                     // incomplete block
		{{Offset: 0, Size: 4096, BaseHostVirtAddr: 0x10000}, {Offset: 0, Size: 4096, BaseHostVirtAddr: 0x20000}}, // overlap
	} {
		_, err := exportMemoryRanges(memory.NewMapping(regions), bitset.New(1).Set(0), 4096)
		require.Error(t, err)
	}
	m := memory.NewMapping([]memory.Region{{Size: 4096, BaseHostVirtAddr: 0x10000}})
	_, err := exportMemoryRanges(m, bitset.New(2).Set(1), 4096)
	require.Error(t, err)
	_, err = exportMemoryRanges(m, nil, 4096)
	require.Error(t, err)
}
