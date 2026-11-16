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

// MemoryTier is the session's memory multiplier; an absent one is 1.
func (s *Session) MemoryTier() int {
	if s.MemoryMultiplier > 1 {
		return s.MemoryMultiplier
	}
	return 1
}

// SetMemoryTier stores m, tier 1 as the omitted default.
func (s *Session) SetMemoryTier(m int) {
	s.MemoryMultiplier = 0
	if m > 1 {
		s.MemoryMultiplier = m
	}
}

// LegacyDoubleMemory is the tier flag session records and manifests carried
// before MemoryMultiplier: true meant tier 2. It is only ever read — decoders
// fold it into MemoryMultiplier — so a record rewritten after that drops it.
type LegacyDoubleMemory struct {
	DoubleMemory bool `json:"doubleMemory,omitempty"`
}

// Fold sets *multiplier to 2 when it is unset and the legacy flag is true.
func (l LegacyDoubleMemory) Fold(multiplier *int) {
	if *multiplier == 0 && l.DoubleMemory {
		*multiplier = 2
	}
}
