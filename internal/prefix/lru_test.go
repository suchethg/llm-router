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

func TestLRUMatchIsReadOnly(t *testing.T) {
	c := NewLRU(2)
	c.MatchAndInsert([]uint64{1})
	c.MatchAndInsert([]uint64{2})
	if n := c.Match([]uint64{1, 2, 3}); n != 2 {
		t.Fatalf("Match = %d, want 2", n)
	}
	if cached(c, 3) {
		t.Fatal("Match must not insert")
	}
	// If Match([1]) refreshed 1, inserting 4 would evict 2 instead of 1.
	c.Match([]uint64{1})
	c.MatchAndInsert([]uint64{4})
	if cached(c, 1) || !cached(c, 2) {
		t.Fatal("Match must not change recency")
	}
}

// cached reports whether h is present without changing its recency.
func cached(c *LRU, h uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[h]
	return ok
}
