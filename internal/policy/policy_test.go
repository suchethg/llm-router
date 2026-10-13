package policy

import (
	"fmt"
	"sync"
	"testing"

	"github.com/suchethg/llm-router/internal/backend"
)

func backends(t *testing.T, n int) []*backend.Backend {
	t.Helper()
	var bs []*backend.Backend
	for i := 0; i < n; i++ {
		b, err := backend.New(fmt.Sprintf("http://replica-%d:8000", i))
		if err != nil {
			t.Fatal(err)
		}
		bs = append(bs, b)
	}
	return bs
}

func TestRoundRobinCycles(t *testing.T) {
	bs := backends(t, 3)
	p := &RoundRobin{}
	for i := 0; i < 7; i++ {
		if got, want := p.Pick(bs), bs[i%3]; got != want {
			t.Fatalf("pick %d = %s, want %s", i, got.Name, want.Name)
		}
	}
}

// Under concurrency the order is arbitrary, but the counter must hand out
// each slot exactly once, so every backend gets an equal share.
func TestRoundRobinConcurrentEven(t *testing.T) {
	bs := backends(t, 4)
	p := &RoundRobin{}
	var mu sync.Mutex
	counts := map[*backend.Backend]int{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				b := p.Pick(bs)
				mu.Lock()
				counts[b]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, b := range bs {
		if counts[b] != 1000 {
			t.Errorf("%s got %d picks, want 1000", b.Name, counts[b])
		}
	}
}

func TestLeastLoadPicksIdlest(t *testing.T) {
	bs := backends(t, 3)
	bs[0].Acquire()
	bs[0].Acquire()
	bs[2].Acquire()
	if got := (&LeastLoad{}).Pick(bs); got != bs[1] {
		t.Fatalf("picked %s (inflight %d), want idle %s", got.Name, got.Inflight(), bs[1].Name)
	}
}

// Requests that stay in flight should fill backends evenly, earliest first
// on ties.
func TestLeastLoadSpreadsAsLoadBuilds(t *testing.T) {
	bs := backends(t, 3)
	p := &LeastLoad{}
	for i := 0; i < 6; i++ {
		b := p.Pick(bs)
		if want := bs[i%3]; b != want {
			t.Fatalf("pick %d = %s, want %s", i, b.Name, want.Name)
		}
		b.Acquire()
	}
}

func TestNew(t *testing.T) {
	for _, name := range []string{"round-robin", "least-load"} {
		p, err := New(name)
		if err != nil || p.Name() != name {
			t.Errorf("New(%q) = %v, %v", name, p, err)
		}
	}
	if _, err := New("random"); err == nil {
		t.Error("New(\"random\") should fail")
	}
}
