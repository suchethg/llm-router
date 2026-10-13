// Package policy decides which replica serves a request.
package policy

import (
	"sync/atomic"

	"github.com/suchethg/llm-router/internal/backend"
)

// Policy picks a backend for a request. Pick is called concurrently from
// every in-flight request, so implementations must be safe for that.
type Policy interface {
	Name() string
	Pick(backends []*backend.Backend) *backend.Backend
}

// RoundRobin cycles through backends in order, ignoring load and content.
// It is the baseline the cache-aware policy is measured against.
type RoundRobin struct{ next atomic.Uint64 }

func (*RoundRobin) Name() string { return "round-robin" }

func (p *RoundRobin) Pick(bs []*backend.Backend) *backend.Backend {
	n := p.next.Add(1) - 1
	return bs[n%uint64(len(bs))]
}
