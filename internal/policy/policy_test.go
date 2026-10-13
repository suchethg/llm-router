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
