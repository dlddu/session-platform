package k8s

import (
	"context"
	"fmt"
	"sync"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// MemoryAuthPool is an in-memory AuthPool for isolated tests, the pool
// counterpart of StubOrchestrator. It keeps AC-L2's exclusivity and ordering
// but has no clock: a held slot is reclaimable as soon as its holder is stale.
type MemoryAuthPool struct {
	mu      sync.Mutex
	holders []string
	states  []*bool
}

var _ AuthPool = (*MemoryAuthPool)(nil)

// NewMemoryAuthPool returns a pool of the given size.
func NewMemoryAuthPool(slots int) *MemoryAuthPool {
	return &MemoryAuthPool{holders: make([]string, slots), states: make([]*bool, slots)}
}

func (p *MemoryAuthPool) Acquire(ctx context.Context, sessionID string, stale StaleHolder) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for slot, holder := range p.holders {
		if holder == sessionID {
			return slot, nil
		}
	}
	best, bestRank := -1, 0
	for slot, holder := range p.holders {
		if holder != "" {
			if stale == nil {
				continue
			}
			gone, err := stale(ctx, holder)
			if err != nil {
				return 0, err
			}
			if !gone {
				continue
			}
		}
		rank := 1
		if state := p.states[slot]; state != nil && *state {
			rank = 0
		} else if state != nil {
			rank = 2
		}
		if best < 0 || rank < bestRank {
			best, bestRank = slot, rank
		}
	}
	if best < 0 {
		return 0, fmt.Errorf("%w: all %d slots are held", session.ErrAuthPoolExhausted, len(p.holders))
	}
	p.holders[best] = sessionID
	return best, nil
}

func (p *MemoryAuthPool) Release(_ context.Context, sessionID string, signedIn *bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for slot, holder := range p.holders {
		if holder == sessionID {
			p.holders[slot] = ""
			if signedIn != nil {
				state := *signedIn
				p.states[slot] = &state
			}
		}
	}
	return nil
}

// Holder reports who holds a slot, "" when free.
func (p *MemoryAuthPool) Holder(slot int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.holders[slot]
}

// HeldBy reports the slot a session holds.
func (p *MemoryAuthPool) HeldBy(sessionID string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for slot, holder := range p.holders {
		if holder == sessionID {
			return slot, true
		}
	}
	return 0, false
}
