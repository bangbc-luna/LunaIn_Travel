package maps

import (
	"container/list"
	"context"
	"math"
	"sync"
	"time"
)

type cacheKey struct{ fromLat, fromLng, toLat, toLng int64 }

type cacheEntry struct {
	key     cacheKey
	route   Route
	expires time.Time
}

type cached struct {
	inner Provider
	ttl   time.Duration
	max   int
	now   func() time.Time

	mu    sync.Mutex
	items map[cacheKey]*list.Element // element value: *cacheEntry
	order *list.List                 // front = oldest
}

// NewCached wraps p with an in-memory cache keyed by the four coordinates
// rounded to 5 decimals. Errors are never cached; when max entries are held
// the oldest one is dropped. Name() is the wrapped provider's.
func NewCached(p Provider, ttl time.Duration, max int, now func() time.Time) Provider {
	if now == nil {
		now = time.Now
	}
	if max < 1 {
		max = 1
	}
	return &cached{inner: p, ttl: ttl, max: max, now: now, items: map[cacheKey]*list.Element{}, order: list.New()}
}

func (c *cached) Name() string { return c.inner.Name() }

func round5(v float64) int64 { return int64(math.Round(v * 1e5)) }

func (c *cached) Route(ctx context.Context, from, to Point) (Route, error) {
	k := cacheKey{round5(from.Lat), round5(from.Lng), round5(to.Lat), round5(to.Lng)}
	c.mu.Lock()
	if el, ok := c.items[k]; ok {
		e := el.Value.(*cacheEntry)
		if c.now().Before(e.expires) {
			c.mu.Unlock()
			return e.route, nil
		}
		c.order.Remove(el)
		delete(c.items, k)
	}
	c.mu.Unlock()

	r, err := c.inner.Route(ctx, from, to)
	if err != nil {
		return Route{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok { // a concurrent call stored it first
		c.order.Remove(el)
		delete(c.items, k)
	}
	for len(c.items) >= c.max {
		oldest := c.order.Front()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
	c.items[k] = c.order.PushBack(&cacheEntry{key: k, route: r, expires: c.now().Add(c.ttl)})
	return r, nil
}
