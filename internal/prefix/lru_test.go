package prefix

import "testing"

func TestLRUHitsAreLeadingBlocks(t *testing.T) {
	c := NewLRU(10)
	if hits := c.MatchAndInsert([]uint64{1, 2, 3}); hits != 0 {
		t.Fatalf("cold cache: %d hits", hits)
	}
	if hits := c.MatchAndInsert([]uint64{1, 2, 3, 4}); hits != 3 {
		t.Fatalf("extended prompt: %d hits, want 3", hits)
	}
	// 9 is new, so 3 and 4 don't count even though they're cached.
	if hits := c.MatchAndInsert([]uint64{1, 9, 3, 4}); hits != 1 {
		t.Fatalf("broken prefix: %d hits, want 1", hits)
	}
}

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewLRU(3)
	c.MatchAndInsert([]uint64{1})
	c.MatchAndInsert([]uint64{2})
	c.MatchAndInsert([]uint64{3})
	c.MatchAndInsert([]uint64{1}) // touch 1, so 2 is now oldest
	c.MatchAndInsert([]uint64{4}) // evicts 2

	for h, want := range map[uint64]bool{1: true, 2: false, 3: true, 4: true} {
		if got := cached(c, h); got != want {
			t.Errorf("block %d: cached=%v, want %v", h, got, want)
		}
	}
	if u := c.Usage(); u != 1 {
		t.Errorf("usage = %v, want 1", u)
	}
}

// cached reports whether h is present without changing its recency.
func cached(c *LRU, h uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[h]
	return ok
}
