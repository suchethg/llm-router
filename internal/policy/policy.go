// Package policy decides which replica serves a request.
package policy

import (
	"fmt"
	"sync/atomic"

	"github.com/suchethg/llm-router/internal/backend"
)

// Policy picks a backend for a request. Pick is called concurrently from
// every in-flight request, so implementations must be safe for that.
type Policy interface {
	Name() string
	Pick(backends []*backend.Backend) *backend.Backend
}

// New builds a policy by name, as given to the router's -policy flag.
func New(name string) (Policy, error) {
	switch name {
	case "round-robin":
		return &RoundRobin{}, nil
	case "least-load":
		return &LeastLoad{}, nil
	}
	return nil, fmt.Errorf("unknown policy %q (want round-robin or least-load)", name)
}

// RoundRobin cycles through backends in order, ignoring load and content.
// It is the baseline the cache-aware policy is measured against.
type RoundRobin struct{ next atomic.Uint64 }

func (*RoundRobin) Name() string { return "round-robin" }

func (p *RoundRobin) Pick(bs []*backend.Backend) *backend.Backend {
	n := p.next.Add(1) - 1
	return bs[n%uint64(len(bs))]
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

func (*LeastLoad) Pick(bs []*backend.Backend) *backend.Backend {
	best := bs[0]
	for _, b := range bs[1:] {
		if b.Inflight() < best.Inflight() {
			best = b
		}
	}
	return best
}
