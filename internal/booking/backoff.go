package booking

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	backoffBase = time.Minute
	backoffMax  = time.Hour
	// poisonFailures: failures after which a row is logged as poison.
	poisonFailures = 5
)

// jobBackoff remembers rows that failed in a periodic job so the next runs
// skip them for an exponentially growing wait (1m, 2m, 4m ... 1h) instead of
// retrying them first every run and starving the rest of the batch. In
// memory, per Service instance, safe for concurrent use.
type jobBackoff struct {
	mu sync.Mutex
	m  map[uuid.UUID]backoffState
}

type backoffState struct {
	failures int
	until    time.Time
}

// skip returns the ids still waiting at now (never nil). Entries long past
// their wait are forgotten.
func (b *jobBackoff) skip(now time.Time) []uuid.UUID {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []uuid.UUID{}
	for id, st := range b.m {
		switch {
		case st.until.After(now):
			out = append(out, id)
		case now.Sub(st.until) > backoffMax:
			delete(b.m, id)
		}
	}
	return out
}

// fail records one more failure of id at now and returns the failure count.
func (b *jobBackoff) fail(id uuid.UUID, now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[uuid.UUID]backoffState{}
	}
	st := b.m[id]
	st.failures++
	wait := backoffMax
	if st.failures <= 7 { // 1m << 6 = 64m already exceeds the cap
		wait = min(backoffBase<<(st.failures-1), backoffMax)
	}
	st.until = now.Add(wait)
	b.m[id] = st
	return st.failures
}

// ok forgets id after a success.
func (b *jobBackoff) ok(id uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, id)
}
