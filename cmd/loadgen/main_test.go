package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suchethg/llm-router/internal/prefix"
)

func TestPercentileIndex(t *testing.T) {
	for _, tc := range []struct {
		n    int
		p    float64
		want int
	}{
		{1, 0.99, 0},
		{10, 0.50, 4},   // 5th smallest
		{10, 0.90, 8},   // 9th smallest
		{100, 0.99, 98}, // 99th smallest
		{101, 0.99, 99}, // ceil(99.99) = 100th smallest
	} {
		if got := percentileIndex(tc.n, tc.p); got != tc.want {
			t.Errorf("percentileIndex(%d, %v) = %d, want %d", tc.n, tc.p, got, tc.want)
		}
	}
}

func TestParseCounters(t *testing.T) {
	text := `# HELP vllm:prefix_cache_hits_total Prompt tokens served from the prefix cache.
# TYPE vllm:prefix_cache_hits_total counter
vllm:prefix_cache_hits_total{model_name="m"} 240
vllm:prefix_cache_queries_total{model_name="m"} 504
vllm:num_requests_running{model_name="m"} 3
vllm:prefix_cache_hits_total_created{model_name="m"} 1.7e9
`
	c, err := parseCounters(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if c.hits != 240 || c.queries != 504 {
		t.Fatalf("got %+v, want hits 240 queries 504", c)
	}
}

// fakeReplica streams a fixed reply and checks that every request carries
// a well-formed history: system, then alternating user/assistant, with each
// assistant turn equal to the reply it streamed.
type fakeReplica struct {
	mu       sync.Mutex
	requests int
	bad      []string
	hits     float64
}

var tokens = []string{"hello ", "world "}

const reply = "hello world "

func (f *fakeReplica) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/metrics" {
		f.mu.Lock()
		fmt.Fprintf(w, "vllm:prefix_cache_hits_total %v\nvllm:prefix_cache_queries_total %v\n", f.hits, 4*f.hits)
		f.mu.Unlock()
		return
	}
	var req struct{ Messages []prefix.Message }
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.requests++
	f.hits++
	if problem := checkHistory(req.Messages); problem != "" {
		f.bad = append(f.bad, problem)
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant"}}]}`+"\n\n")
	for _, tok := range tokens {
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%q}}]}`+"\n\n", tok)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func checkHistory(msgs []prefix.Message) string {
	if len(msgs) < 2 || msgs[0].Role != "system" || len(msgs)%2 != 0 {
		return fmt.Sprintf("bad shape: %d messages", len(msgs))
	}
	for i := 1; i < len(msgs); i++ {
		want := "user"
		if i%2 == 0 {
			want = "assistant"
		}
		if msgs[i].Role != want {
			return fmt.Sprintf("message %d is %s, want %s", i, msgs[i].Role, want)
		}
		if want == "assistant" && msgs[i].Content != reply {
			return fmt.Sprintf("assistant turn %q, want the streamed reply %q", msgs[i].Content, reply)
		}
	}
	return ""
}

func testConfig(url string) config {
	return config{
		target: url, metrics: []string{url + "/metrics"},
		rate: 50, duration: 600 * time.Millisecond, warmup: 200 * time.Millisecond,
		turns: 3, think: 10 * time.Millisecond,
		sysPrompts: 2, sysChars: 100, userChars: 20, maxTokens: 4, seed: 1,
	}
}

func TestRunReplaysHistory(t *testing.T) {
	f := &fakeReplica{}
	srv := httptest.NewServer(f)
	defer srv.Close()

	sum, err := run(context.Background(), testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.bad) > 0 {
		t.Fatalf("%d malformed requests, first: %s", len(f.bad), f.bad[0])
	}
	if sum.Requests == 0 || sum.Errors != 0 {
		t.Fatalf("requests=%d errors=%d", sum.Requests, sum.Errors)
	}
	// Warmup requests reach the server but aren't counted.
	if sum.Requests >= f.requests {
		t.Fatalf("summary counted %d of %d requests; warmup should be excluded", sum.Requests, f.requests)
	}
	// The fake reports queries = 4 x hits, so any window gives 25%.
	if sum.HitRate == nil || *sum.HitRate != 0.25 {
		t.Fatalf("hit rate %v, want 0.25", sum.HitRate)
	}
	if sum.TTFTms["p99"] <= 0 {
		t.Fatalf("no TTFT recorded: %+v", sum.TTFTms)
	}
}

// The same seed must give the same workload, so policies are compared on
// identical traffic.
func TestSameSeedSameWorkload(t *testing.T) {
	prompts := func() []string {
		var mu sync.Mutex
		var got []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct{ Messages []prefix.Message }
			json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			got = append(got, prefix.RenderChat(req.Messages[:2])) // system + first user message
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError) // end each conversation after turn 1
		}))
		defer srv.Close()
		c := testConfig(srv.URL)
		c.metrics = nil
		run(context.Background(), c)
		slices.Sort(got) // arrival order varies; the set of prompts must not
		return got
	}
	a, b := prompts(), prompts()
	if len(a) == 0 || strings.Join(a, "|") != strings.Join(b, "|") {
		t.Fatalf("two runs with the same seed sent different first turns (%d vs %d)", len(a), len(b))
	}
}

func TestReportCombinesRuns(t *testing.T) {
	hr := func(v float64) *float64 { return &v }
	sums := []summary{
		{Label: "round-robin", RequestsPerSec: 5, TTFTms: map[string]float64{"p50": 90, "p90": 200, "p99": 500}, HitRate: hr(0.70)},
		{Label: "prefix-aware", RequestsPerSec: 5, Errors: 1, TTFTms: map[string]float64{"p50": 60, "p90": 100, "p99": 200}, HitRate: hr(0.84)},
		{Label: "prefix-aware", RequestsPerSec: 6, Errors: 2, TTFTms: map[string]float64{"p50": 70, "p90": 110, "p99": 260}, HitRate: hr(0.86)},
	}
	var b strings.Builder
	printReport(&b, sums)
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got:\n%s", b.String())
	}
	rr, pa := strings.Fields(lines[1]), lines[2]
	if rr[0] != "round-robin" || rr[1] != "1" || rr[6] != "500" || rr[7] != "70.0" {
		t.Errorf("single run row = %q", lines[1])
	}
	for _, want := range []string{"prefix-aware", " 2 ", "5.5", " 3 ", "65 (60-70)", "230 (200-260)", "85.0 (84.0-86.0)"} {
		if !strings.Contains(pa, want) {
			t.Errorf("combined row %q missing %q", pa, want)
		}
	}
}

func TestReportNeedsFiles(t *testing.T) {
	var b strings.Builder
	if err := report(&b, nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("report with no files: err=%v, want a usage error", err)
	}
	if b.Len() != 0 {
		t.Fatalf("printed %q, want nothing", b.String())
	}
	if err := report(&b, []string{"does-not-exist.json"}); err == nil {
		t.Fatal("missing file should be an error")
	}
}
