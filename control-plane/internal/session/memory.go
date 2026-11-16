package session

// MemoryMultipliers are the memory tiers a session can run at: the workload
// container's memory request/limit is the platform default times the tier.
var MemoryMultipliers = []int{1, 2, 4, 8}

// ValidMemoryMultiplier reports whether m is one of MemoryMultipliers.
func ValidMemoryMultiplier(m int) bool {
	for _, v := range MemoryMultipliers {
		if v == m {
			return true
		}
	}
	return false
}

// MemoryTier is the session's memory multiplier. Records written before
// memoryMultiplier existed carry only doubleMemory, so that flag still counts.
func (s *Session) MemoryTier() int {
	if s.MemoryMultiplier > 1 {
		return s.MemoryMultiplier
	}
	if s.DoubleMemory {
		return 2
	}
	return 1
}

// SetMemoryTier stores m. doubleMemory stays true exactly at tier 2 so a
// client that reads only that field still recognises a doubled session.
func (s *Session) SetMemoryTier(m int) {
	s.MemoryMultiplier = 0
	if m > 1 {
		s.MemoryMultiplier = m
	}
	s.DoubleMemory = m == 2
}
