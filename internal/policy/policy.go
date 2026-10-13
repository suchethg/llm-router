// Package policy decides which replica serves a request.
package policy

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/suchethg/llm-router/internal/backend"
	"github.com/suchethg/llm-router/internal/prefix"
)

// Request is what a policy may look at when choosing a backend.
type Request struct {
	Prompt string // chat messages rendered with prefix.RenderChat
}

// Reasons a policy gives for its choice, used as a metric label.
const (
	ReasonRoundRobin   = "round-robin"
	ReasonLeastLoad    = "least-load"
	ReasonCacheHit     = "cache_hit"     // longest cached prefix among eligible replicas
	ReasonNoOverlap    = "no_overlap"    // nothing cached anywhere: fell back to least-load
	ReasonLoadOverride = "load_override" // a longer prefix was cached on a replica over the slack
)

// Decision is a policy's choice plus what the router reports about it.
type Decision struct {
	Backend       *backend.Backend
	PromptBlocks  int // full blocks in the prompt
	MatchedBlocks int // blocks predicted to be cached on Backend
	Reason        string
}

// Policy picks a backend for a request. Pick is called concurrently from
// every in-flight request, so implementations must be safe for that.
type Policy interface {
	Name() string
	Pick(req *Request, backends []*backend.Backend) Decision
}

// Options tunes the prefix-aware policy; the baselines ignore it.
type Options struct {
	BlockChars  int   // chars per block; must match the replicas' block size
	CacheBlocks int   // shadow index size per replica, in blocks
	LoadSlack   int64 // extra in-flight requests accepted to get a cache hit
}

func DefaultOptions() Options {
	// LoadSlack 4 is half the replicas' default max-num-seqs of 8.
	return Options{BlockChars: prefix.DefaultBlockChars, CacheBlocks: 2048, LoadSlack: 4}
}

// New builds a policy by name, as given to the router's -policy flag.
func New(name string, opts Options) (Policy, error) {
	switch name {
	case "round-robin":
		return &RoundRobin{}, nil
	case "least-load":
		return &LeastLoad{}, nil
	case "prefix-aware":
		return NewPrefixAware(opts), nil
	}
	return nil, fmt.Errorf("unknown policy %q (want round-robin, least-load or prefix-aware)", name)
}

// RoundRobin cycles through backends in order, ignoring load and content.
// It is the baseline the cache-aware policy is measured against.
type RoundRobin struct{ next atomic.Uint64 }

func (*RoundRobin) Name() string { return "round-robin" }

func (p *RoundRobin) Pick(_ *Request, bs []*backend.Backend) Decision {
	n := p.next.Add(1) - 1
	return Decision{Backend: bs[n%uint64(len(bs))], Reason: ReasonRoundRobin}
}

// LeastLoad picks the backend with the fewest in-flight requests, ties going
// to the earliest. It balances load but ignores the cache, so comparing it
// with the cache-aware policy separates the two effects.
//
// Pick and the caller's Acquire aren't one atomic step, so requests that
// arrive in the same instant can all pick the same idle backend. Later picks
// see the inflated count and steer away, so the skew is brief; locking every
// pick to prevent it would serialize all routing.
type LeastLoad struct{}

func (*LeastLoad) Name() string { return "least-load" }

func (*LeastLoad) Pick(_ *Request, bs []*backend.Backend) Decision {
	best := bs[0]
	for _, b := range bs[1:] {
		if b.Inflight() < best.Inflight() {
			best = b
		}
	}
	return Decision{Backend: best, Reason: ReasonLeastLoad}
}

// PrefixAware routes each request to the replica most likely to have its
// prompt prefix in KV cache, as long as that replica isn't overloaded.
//
// The router can't see replica caches, so it keeps a shadow LRU per replica
// of the blocks it has sent there. Only replicas within LoadSlack in-flight
// requests of the least-loaded one are eligible; among them it picks the
// longest cached prefix, breaking ties by lower load. With no overlap
// anywhere this is least-load, which spreads new conversations.
//
// The shadow is an estimate: it drifts if a replica restarts or evicts
// differently, and corrects itself as new traffic is recorded.
type PrefixAware struct {
	opts Options

	// Held across match, choose and record, so two turns of a conversation
	// arriving together see each other's effect on the shadow.
	mu     sync.Mutex
	shadow map[*backend.Backend]*prefix.LRU
}

func NewPrefixAware(opts Options) *PrefixAware {
	return &PrefixAware{opts: opts, shadow: make(map[*backend.Backend]*prefix.LRU)}
}

func (*PrefixAware) Name() string { return "prefix-aware" }

func (p *PrefixAware) Pick(req *Request, bs []*backend.Backend) Decision {
	hashes := prefix.BlockHashes(req.Prompt, p.opts.BlockChars) // outside the lock

	p.mu.Lock()
	defer p.mu.Unlock()

	// Snapshot loads once: they change concurrently, and the eligibility
	// check and tie-break must agree on the same values. Taking minLoad
	// from the snapshot also guarantees at least one backend is eligible.
	loads := make([]int64, len(bs))
	for i, b := range bs {
		loads[i] = b.Inflight()
	}
	minLoad := slices.Min(loads)

	best, bestMatch, longestAnywhere := -1, -1, 0
	for i, b := range bs {
		m := p.shadowOf(b).Match(hashes)
		longestAnywhere = max(longestAnywhere, m)
		if loads[i] > minLoad+p.opts.LoadSlack {
			continue
		}
		if m > bestMatch || (m == bestMatch && loads[i] < loads[best]) {
			best, bestMatch = i, m
		}
	}
	p.shadowOf(bs[best]).MatchAndInsert(hashes)

	d := Decision{Backend: bs[best], PromptBlocks: len(hashes), MatchedBlocks: bestMatch, Reason: ReasonCacheHit}
	switch {
	case longestAnywhere > bestMatch:
		d.Reason = ReasonLoadOverride
	case bestMatch == 0:
		d.Reason = ReasonNoOverlap
	}
	return d
}

// shadowOf must be called with p.mu held.
func (p *PrefixAware) shadowOf(b *backend.Backend) *prefix.LRU {
	c, ok := p.shadow[b]
	if !ok {
		c = prefix.NewLRU(p.opts.CacheBlocks)
		p.shadow[b] = c
	}
	return c
}
