package policy

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/suchethg/llm-router/internal/backend"
	"github.com/suchethg/llm-router/internal/prefix"
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
		if got, want := p.Pick(nil, bs), bs[i%3]; got != want {
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
				b := p.Pick(nil, bs)
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
	if got := (&LeastLoad{}).Pick(nil, bs); got != bs[1] {
		t.Fatalf("picked %s (inflight %d), want idle %s", got.Name, got.Inflight(), bs[1].Name)
	}
}

// Requests that stay in flight should fill backends evenly, earliest first
// on ties.
func TestLeastLoadSpreadsAsLoadBuilds(t *testing.T) {
	bs := backends(t, 3)
	p := &LeastLoad{}
	for i := 0; i < 6; i++ {
		b := p.Pick(nil, bs)
		if want := bs[i%3]; b != want {
			t.Fatalf("pick %d = %s, want %s", i, b.Name, want.Name)
		}
		b.Acquire()
	}
}

func TestNew(t *testing.T) {
	for _, name := range []string{"round-robin", "least-load", "prefix-aware"} {
		p, err := New(name, DefaultOptions())
		if err != nil || p.Name() != name {
			t.Errorf("New(%q) = %v, %v", name, p, err)
		}
	}
	if _, err := New("random", DefaultOptions()); err == nil {
		t.Error("New(\"random\") should fail")
	}
}

// conversation builds the rendered prompt for turn n of a chat whose system
// prompt is sys: each turn appends a user message and an assistant reply.
func conversation(sys, id string, n int) *Request {
	msgs := []prefix.Message{{Role: "system", Content: sys}}
	for i := 1; i <= n; i++ {
		msgs = append(msgs, prefix.Message{Role: "user", Content: strings.Repeat(fmt.Sprintf("%s question %d ", id, i), 10)})
		if i < n {
			msgs = append(msgs, prefix.Message{Role: "assistant", Content: strings.Repeat("answer ", 20)})
		}
	}
	return &Request{Prompt: prefix.RenderChat(msgs)}
}

var sysA = strings.Repeat("You are a support agent. ", 40) // ~1000 chars, 15 blocks

func TestPrefixAwareKeepsConversationOnOneReplica(t *testing.T) {
	bs := backends(t, 4)
	p := NewPrefixAware(DefaultOptions())
	first := p.Pick(conversation(sysA, "alice", 1), bs)
	for turn := 2; turn <= 6; turn++ {
		if got := p.Pick(conversation(sysA, "alice", turn), bs); got != first {
			t.Fatalf("turn %d went to %s, want %s", turn, got.Name, first.Name)
		}
	}
}

// Unrelated conversations have no overlap, so they fall back to least-load
// and spread out while they're in flight.
func TestPrefixAwareSpreadsUnrelatedConversations(t *testing.T) {
	bs := backends(t, 4)
	p := NewPrefixAware(DefaultOptions())
	seen := map[*backend.Backend]bool{}
	for i := 0; i < 4; i++ {
		b := p.Pick(conversation(fmt.Sprintf("system prompt %d ", i)+sysA, "u", 1), bs)
		b.Acquire()
		seen[b] = true
	}
	if len(seen) != 4 {
		t.Fatalf("4 unrelated conversations used %d replicas, want 4", len(seen))
	}
}

// A new conversation that shares a system prompt should go where that
// system prompt is already cached, if the load allows.
func TestPrefixAwareReusesSharedSystemPrompt(t *testing.T) {
	bs := backends(t, 4)
	p := NewPrefixAware(DefaultOptions())
	home := p.Pick(conversation(sysA, "alice", 1), bs)
	home.Acquire() // alice still in flight: home is busier than the rest
	if got := p.Pick(conversation(sysA, "bob", 1), bs); got != home {
		t.Fatalf("bob went to %s, want %s where the system prompt is cached", got.Name, home.Name)
	}
}

func TestPrefixAwareLoadSlackOverridesCache(t *testing.T) {
	bs := backends(t, 2)
	opts := DefaultOptions()
	opts.LoadSlack = 2
	p := NewPrefixAware(opts)

	home := p.Pick(conversation(sysA, "alice", 1), bs)
	for i := 0; i < 2; i++ {
		home.Acquire()
	}
	if got := p.Pick(conversation(sysA, "alice", 2), bs); got != home {
		t.Fatalf("at slack (2 extra) the cache should still win, went to %s", got.Name)
	}
	home.Acquire() // now 3 more than the idle replica
	if got := p.Pick(conversation(sysA, "alice", 3), bs); got == home {
		t.Fatalf("beyond slack, picked overloaded %s (inflight %d)", home.Name, home.Inflight())
	}
}

func TestPrefixAwareTieGoesToLowerLoad(t *testing.T) {
	bs := backends(t, 3)
	bs[0].Acquire()
	bs[2].Acquire()
	p := NewPrefixAware(DefaultOptions())
	if got := p.Pick(conversation(sysA, "alice", 1), bs); got != bs[1] {
		t.Fatalf("no overlap anywhere: picked %s, want idle %s", got.Name, bs[1].Name)
	}
}

// Run under -race: concurrent picks must not corrupt the shadow index.
func TestPrefixAwareConcurrent(t *testing.T) {
	bs := backends(t, 4)
	p := NewPrefixAware(DefaultOptions())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for turn := 1; turn <= 20; turn++ {
				b := p.Pick(conversation(sysA, fmt.Sprint("user", g), turn), bs)
				b.Acquire()
				b.Release()
			}
		}(g)
	}
	wg.Wait()
}
