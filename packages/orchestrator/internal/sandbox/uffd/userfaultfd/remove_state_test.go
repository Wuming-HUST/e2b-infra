package userfaultfd

import (
	"sync"
	"testing"
)

func TestRemovedPageRangesMergesAndChecksBoundaries(t *testing.T) {
	var state removedPageRanges
	state.add(0x1000, 0x3000)
	state.add(0x3000, 0x5000)

	if got := state.len(); got != 1 {
		t.Fatalf("merged range count = %d, want 1", got)
	}

	for _, tc := range []struct {
		addr uintptr
		want bool
	}{
		{0x0fff, false}, {0x1000, true}, {0x2fff, true},
		{0x3000, true}, {0x4fff, true}, {0x5000, false},
	} {
		if got := state.contains(tc.addr); got != tc.want {
			t.Errorf("contains(%#x) = %t, want %t", tc.addr, got, tc.want)
		}
	}
}

func TestRemovedPageRangesConcurrentAccess(t *testing.T) {
	var state removedPageRanges
	var wg sync.WaitGroup
	for i := uintptr(0); i < 64; i++ {
		start := i * 0x10000
		wg.Add(1)
		go func() {
			defer wg.Done()
			state.add(start, start+0x1000)
			_ = state.contains(start)
		}()
	}
	wg.Wait()
	if got := state.len(); got != 64 {
		t.Fatalf("range count = %d, want 64", got)
	}
}
