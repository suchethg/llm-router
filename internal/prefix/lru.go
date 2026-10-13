package prefix

import (
	"container/list"
	"sync"
)

// LRU is a bounded set of block hashes with least-recently-used eviction,
// standing in for a replica's KV cache. vLLM keeps finished requests' blocks
// as evictable and reclaims them roughly least-recently-used first.
type LRU struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List // front = most recently used
	items map[uint64]*list.Element
}

func NewLRU(capacity int) *LRU {
	return &LRU{cap: capacity, ll: list.New(), items: make(map[uint64]*list.Element)}
}

// Match returns how many leading blocks are cached, without inserting them
// or changing their recency, so asking "how much would this replica reuse?"
// of every replica doesn't disturb the caches of the ones not chosen.
func (c *LRU) Match(hashes []uint64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, h := range hashes {
		if _, ok := c.items[h]; !ok {
			break
		}
		n++
	}
	return n
}

// MatchAndInsert returns how many leading blocks were already cached, then
// caches all of them (the request's prefill computes the missing ones).
// As in vLLM, a hit only counts while the prefix is unbroken: after the
// first miss, later blocks can't be reused even if present, because their
// hashes chain through the missing block.
func (c *LRU) MatchAndInsert(hashes []uint64) (hits int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	matching := true
	for _, h := range hashes {
		if e, ok := c.items[h]; ok {
			if matching {
				hits++
			}
			c.ll.MoveToFront(e)
			continue
		}
		matching = false
		c.items[h] = c.ll.PushFront(h)
		if c.ll.Len() > c.cap {
			oldest := c.ll.Back()
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(uint64))
		}
	}
	return hits
}

// Usage is the fraction of capacity in use, 0 to 1.
func (c *LRU) Usage() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return float64(c.ll.Len()) / float64(c.cap)
}
