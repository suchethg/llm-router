// Package backend describes the vLLM replicas the router can send traffic to.
package backend

import (
	"fmt"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
)

// Backend is one vLLM replica.
type Backend struct {
	Name  string // host:port, used in logs and the X-Routed-To header
	URL   *url.URL
	Proxy *httputil.ReverseProxy

	// Requests this router has sent and not yet finished streaming. Tracked
	// locally rather than scraped from vllm:num_requests_running, which is
	// seconds stale: in that window every request would see the same "idle"
	// replica and pile onto it. With several router replicas, each sees
	// only its own share of the load.
	inflight atomic.Int64
}

func (b *Backend) Inflight() int64 { return b.inflight.Load() }
func (b *Backend) Acquire()        { b.inflight.Add(1) }
func (b *Backend) Release()        { b.inflight.Add(-1) }

// New builds a backend from a base URL such as "http://10.0.0.5:8000".
func New(rawURL string) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("backend %q: %w", rawURL, err)
	}
	// url.Parse accepts "localhost:8000" as scheme "localhost" with no host,
	// which would fail on every request instead of at startup.
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("backend %q: want http(s)://host:port", rawURL)
	}
	// ReverseProxy flushes text/event-stream responses on every write, so
	// vLLM's streamed tokens pass through as they are generated.
	return &Backend{Name: u.Host, URL: u, Proxy: httputil.NewSingleHostReverseProxy(u)}, nil
}

// ParseList parses a comma-separated list of base URLs.
func ParseList(list string) ([]*Backend, error) {
	var out []*Backend
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		b, err := New(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	return out, nil
}
