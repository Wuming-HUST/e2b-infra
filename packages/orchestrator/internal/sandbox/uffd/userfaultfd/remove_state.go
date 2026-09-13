package userfaultfd

import (
	"sort"
	"sync"
)

// removedPageRanges is monotonic for the lifetime of a restored VM. After
// discard, the snapshot is never a valid source for that address again. Swap
// faults remain the kernel's responsibility; we do not infer residency here.
// mu also orders reading/acknowledging REMOVE against the install ioctl. It is
// never held across source fetches, retry waits, or worker admission.
type removedPageRanges struct {
	mu     sync.RWMutex
	ranges []removedPageRange // sorted, disjoint half-open intervals
	closed bool
}
type removedPageRange struct{ start, end uintptr }

func (s *removedPageRanges) add(start, end uintptr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addLocked(start, end)
}
func (s *removedPageRanges) addLocked(start, end uintptr) {
	if end <= start {
		return
	}
	first := sort.Search(len(s.ranges), func(i int) bool { return s.ranges[i].end >= start })
	last := first
	for last < len(s.ranges) && s.ranges[last].start <= end {
		start = min(start, s.ranges[last].start)
		end = max(end, s.ranges[last].end)
		last++
	}
	if first == last {
		s.ranges = append(s.ranges, removedPageRange{})
		copy(s.ranges[first+1:], s.ranges[first:])
	} else {
		copy(s.ranges[first+1:], s.ranges[last:])
		s.ranges = s.ranges[:len(s.ranges)-(last-first)+1]
	}
	s.ranges[first] = removedPageRange{start, end}
}
func (s *removedPageRanges) contains(addr uintptr) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.containsLocked(addr)
}
func (s *removedPageRanges) with(addr uintptr, fn func(bool) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	return fn(s.containsLocked(addr))
}
func (s *removedPageRanges) containsLocked(addr uintptr) bool {
	i := sort.Search(len(s.ranges), func(i int) bool { return s.ranges[i].end > addr })
	return i < len(s.ranges) && s.ranges[i].start <= addr
}
func (s *removedPageRanges) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ranges)
}
