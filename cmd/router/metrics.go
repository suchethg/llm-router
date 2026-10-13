package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/suchethg/llm-router/internal/backend"
	"github.com/suchethg/llm-router/internal/policy"
)

// metrics is what the router exports on /metrics. Labels are limited to
// replica, policy, reason and status code, so series stay few.
type metrics struct {
	reg *prometheus.Registry

	requests       *prometheus.CounterVec // backend, policy, reason, code
	promptBlocks   *prometheus.CounterVec // backend
	predictedHits  *prometheus.CounterVec // backend
	ttfb           *prometheus.HistogramVec
	duration       *prometheus.HistogramVec
	routingLatency prometheus.Histogram
}

func newMetrics(bs []*backend.Backend) *metrics {
	m := &metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "router_requests_total",
			Help: "Chat requests by chosen backend, policy, routing reason and HTTP status.",
		}, []string{"backend", "policy", "reason", "code"}),
		promptBlocks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "router_prompt_blocks_total",
			Help: "Full prompt blocks routed to each backend.",
		}, []string{"backend"}),
		predictedHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "router_predicted_hit_blocks_total",
			Help: "Prompt blocks the router predicted were already cached on the chosen backend.",
		}, []string{"backend"}),
		ttfb: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "router_time_to_first_byte_seconds",
			Help:    "From request arrival to the first response byte (first SSE chunk when streaming), successful requests only.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 14), // 5ms to ~41s
		}, []string{"backend"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "router_request_duration_seconds",
			Help:    "From request arrival until the response finished streaming, successful requests only.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 12), // 50ms to ~102s
		}, []string{"backend"}),
		routingLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "router_routing_overhead_seconds",
			Help:    "Time the router spends reading, parsing, hashing and picking before proxying.",
			Buckets: prometheus.ExponentialBuckets(0.00005, 2, 14), // 50us to ~0.4s
		}),
	}
	m.reg.MustRegister(m.requests, m.promptBlocks, m.predictedHits, m.ttfb, m.duration, m.routingLatency)
	for _, b := range bs {
		m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "router_inflight_requests",
			Help:        "Requests the router has sent to a backend and not yet finished streaming.",
			ConstLabels: prometheus.Labels{"backend": b.Name},
		}, func() float64 { return float64(b.Inflight()) }))
	}
	return m
}

// observe records one finished request.
func (m *metrics) observe(pol string, d policy.Decision, rec *recorder, arrived time.Time) {
	name := d.Backend.Name
	m.requests.WithLabelValues(name, pol, d.Reason, strconv.Itoa(rec.status())).Inc()
	m.promptBlocks.WithLabelValues(name).Add(float64(d.PromptBlocks))
	m.predictedHits.WithLabelValues(name).Add(float64(d.MatchedBlocks))
	if rec.status() < 300 && !rec.firstByte.IsZero() {
		m.ttfb.WithLabelValues(name).Observe(rec.firstByte.Sub(arrived).Seconds())
		m.duration.WithLabelValues(name).Observe(time.Since(arrived).Seconds())
	}
}

// recorder wraps a ResponseWriter to capture the status code and when the
// first byte was written.
type recorder struct {
	http.ResponseWriter
	code      int
	firstByte time.Time
}

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.firstByte.IsZero() {
		r.firstByte = time.Now()
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController (which ReverseProxy uses to flush
// each streamed chunk) reach the underlying writer's Flush.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) status() int {
	if r.code == 0 {
		return http.StatusOK // Write without WriteHeader implies 200
	}
	return r.code
}
