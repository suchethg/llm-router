// Command loadgen replays a multi-turn chat workload against an
// OpenAI-compatible endpoint and reports TTFT percentiles and the replicas'
// prefix-cache hit rate.
//
// Load is open-loop: new conversations arrive at a fixed average rate
// (Poisson) regardless of how fast the system answers. A closed-loop client
// that waits for each reply sends less exactly when the system slows down,
// which hides tail latency (coordinated omission). Within a conversation,
// turns are sequential: the user reads the reply, thinks, and sends the
// full history plus a new message, as chat clients do.
//
// The same -seed produces the same arrivals, prompts and think times, so
// every routing policy is measured on an identical workload.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/suchethg/llm-router/internal/prefix"
)

type config struct {
	target     string
	model      string
	metrics    []string // replica /metrics URLs; empty skips hit rate
	rate       float64  // new conversations per second
	duration   time.Duration
	warmup     time.Duration
	turns      int
	think      time.Duration // mean pause between a reply and the next turn
	sysPrompts int
	sysChars   int
	userChars  int
	maxTokens  int
	seed       int64
	label      string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "report" {
		report(os.Args[2:])
		return
	}

	var c config
	var metrics, jsonOut string
	flag.StringVar(&c.target, "target", "http://localhost:8090", "router (or single replica) base URL")
	flag.StringVar(&c.model, "model", "mock-llama-3.1-8b", "model name sent in requests")
	flag.StringVar(&metrics, "metrics", "http://localhost:8001/metrics,http://localhost:8002/metrics,http://localhost:8003/metrics,http://localhost:8004/metrics", "comma-separated replica /metrics URLs for hit rate (empty to skip)")
	flag.Float64Var(&c.rate, "rate", 2, "new conversations per second (Poisson arrivals)")
	flag.DurationVar(&c.duration, "duration", 2*time.Minute, "how long to start new turns")
	flag.DurationVar(&c.warmup, "warmup", 30*time.Second, "initial period excluded from results (caches start cold)")
	flag.IntVar(&c.turns, "turns", 8, "turns per conversation")
	flag.DurationVar(&c.think, "think", 2*time.Second, "mean think time between turns (exponential)")
	flag.IntVar(&c.sysPrompts, "sys-prompts", 8, "distinct system prompts shared across conversations")
	flag.IntVar(&c.sysChars, "sys-chars", 2000, "system prompt length in chars (~4 chars/token)")
	flag.IntVar(&c.userChars, "user-chars", 400, "user message length in chars")
	flag.IntVar(&c.maxTokens, "max-tokens", 64, "max_tokens per request")
	flag.Int64Var(&c.seed, "seed", 1, "random seed; same seed, same workload")
	flag.StringVar(&c.label, "label", "", "name for this run in the summary, e.g. the policy")
	flag.StringVar(&jsonOut, "json", "", "also write the summary as JSON to this file")
	flag.Parse()
	for _, m := range strings.Split(metrics, ",") {
		if m = strings.TrimSpace(m); m != "" {
			c.metrics = append(c.metrics, m)
		}
	}
	if c.warmup >= c.duration {
		log.Fatal("-warmup must be shorter than -duration")
	}

	sum, err := run(context.Background(), c)
	if err != nil {
		log.Fatal(err)
	}
	sum.print(os.Stdout)
	if jsonOut != "" {
		b, _ := json.MarshalIndent(sum, "", "  ")
		if err := os.WriteFile(jsonOut, append(b, '\n'), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

type sample struct {
	start   time.Time
	ttft    time.Duration
	e2e     time.Duration
	failed  bool
	replica string // X-Routed-To, when going through the router
}

func run(ctx context.Context, c config) (summary, error) {
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1024}}
	sys := make([]string, c.sysPrompts)
	for i := range sys {
		sys[i] = fmt.Sprintf("[system prompt %d] ", i) + text(rand.New(rand.NewSource(c.seed*1000+int64(i))), c.sysChars)
	}

	start := time.Now()
	warmupEnd := start.Add(c.warmup)
	stopTurns := start.Add(c.duration)

	var (
		mu      sync.Mutex
		samples []sample
		wg      sync.WaitGroup
		before  counters
		scraped = make(chan error, 1)
	)
	// Read the cache counters at the end of warmup, so cold-cache misses
	// don't count against any policy.
	go func() {
		time.Sleep(time.Until(warmupEnd))
		var err error
		before, err = scrape(client, c.metrics)
		scraped <- err
	}()

	arrivals := rand.New(rand.NewSource(c.seed))
	for id, at := 0, start; ; id++ {
		at = at.Add(time.Duration(arrivals.ExpFloat64() / c.rate * float64(time.Second)))
		if at.After(stopTurns) {
			break
		}
		time.Sleep(time.Until(at))
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(c.seed*1_000_003 + int64(id)))
			msgs := []prefix.Message{{Role: "system", Content: sys[rng.Intn(len(sys))]}}
			for turn := 1; turn <= c.turns && time.Now().Before(stopTurns); turn++ {
				msgs = append(msgs, prefix.Message{Role: "user", Content: fmt.Sprintf("[conversation %d turn %d] ", id, turn) + text(rng, c.userChars)})
				s, reply := chat(ctx, client, c, msgs)
				mu.Lock()
				samples = append(samples, s)
				mu.Unlock()
				if s.failed {
					return
				}
				msgs = append(msgs, prefix.Message{Role: "assistant", Content: reply})
				if turn < c.turns {
					time.Sleep(time.Duration(rng.ExpFloat64() * float64(c.think)))
				}
			}
		}(id)
	}
	// No new turns start after -duration, but turns already sent finish,
	// so slow requests near the end aren't cut off and dropped.
	wg.Wait()
	if err := <-scraped; err != nil {
		return summary{}, fmt.Errorf("scrape metrics at end of warmup: %w", err)
	}
	after, err := scrape(client, c.metrics)
	if err != nil {
		return summary{}, fmt.Errorf("scrape metrics: %w", err)
	}

	var measured []sample
	for _, s := range samples {
		if !s.start.Before(warmupEnd) {
			measured = append(measured, s)
		}
	}
	return summarize(c, measured, c.duration-c.warmup, after.minus(before)), nil
}

// chat sends one streaming request and returns its timing and the reply,
// which becomes part of the next turn's history.
func chat(ctx context.Context, client *http.Client, c config, msgs []prefix.Message) (sample, string) {
	body, _ := json.Marshal(map[string]any{"model": c.model, "messages": msgs, "max_tokens": c.maxTokens, "stream": true})
	s := sample{start: time.Now()}
	req, _ := http.NewRequestWithContext(ctx, "POST", c.target+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		s.failed = true
		return s, ""
	}
	defer resp.Body.Close()
	s.replica = resp.Header.Get("X-Routed-To")
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		s.failed = true
		return s, ""
	}

	var reply strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		// TTFT is the first token of content, not the role-only chunk.
		if tok := chunk.Choices[0].Delta.Content; tok != "" {
			if s.ttft == 0 {
				s.ttft = time.Since(s.start)
			}
			reply.WriteString(tok)
		}
	}
	s.e2e = time.Since(s.start)
	s.failed = sc.Err() != nil || s.ttft == 0
	return s, reply.String()
}

type summary struct {
	Label              string             `json:"label"`
	Requests           int                `json:"requests"`
	Errors             int                `json:"errors"`
	RequestsPerSec     float64            `json:"requests_per_sec"`
	TTFTms             map[string]float64 `json:"ttft_ms"`
	E2Ems              map[string]float64 `json:"e2e_ms"`
	HitRate            *float64           `json:"prefix_cache_hit_rate,omitempty"`
	RequestsPerReplica map[string]int     `json:"requests_per_replica,omitempty"`
}

func summarize(c config, ss []sample, window time.Duration, cache counters) summary {
	sum := summary{Label: c.label, RequestsPerReplica: map[string]int{}}
	var ttft, e2e []time.Duration
	for _, s := range ss {
		sum.Requests++
		if s.failed {
			sum.Errors++
			continue
		}
		ttft = append(ttft, s.ttft)
		e2e = append(e2e, s.e2e)
		if s.replica != "" {
			sum.RequestsPerReplica[s.replica]++
		}
	}
	sum.RequestsPerSec = float64(sum.Requests) / window.Seconds()
	sum.TTFTms, sum.E2Ems = stats(ttft), stats(e2e)
	if cache.queries > 0 {
		hr := cache.hits / cache.queries
		sum.HitRate = &hr
	}
	return sum
}

func (s summary) print(w io.Writer) {
	fmt.Fprintf(w, "%s: %d requests (%.1f/s), %d errors\n", s.Label, s.Requests, s.RequestsPerSec, s.Errors)
	fmt.Fprintf(w, "  TTFT ms   p50 %7.1f   p90 %7.1f   p99 %7.1f\n", s.TTFTms["p50"], s.TTFTms["p90"], s.TTFTms["p99"])
	fmt.Fprintf(w, "  E2E  ms   p50 %7.1f   p90 %7.1f   p99 %7.1f\n", s.E2Ems["p50"], s.E2Ems["p90"], s.E2Ems["p99"])
	if s.HitRate != nil {
		fmt.Fprintf(w, "  prefix cache hit rate %.1f%%\n", *s.HitRate*100)
	}
	if len(s.RequestsPerReplica) > 0 {
		fmt.Fprintf(w, "  per replica %v\n", s.RequestsPerReplica)
	}
}

// stats returns nearest-rank percentiles in milliseconds.
func stats(ds []time.Duration) map[string]float64 {
	out := map[string]float64{}
	if len(ds) == 0 {
		return out
	}
	slices.Sort(ds)
	for name, p := range map[string]float64{"p50": 0.50, "p90": 0.90, "p99": 0.99} {
		out[name] = ms(ds[percentileIndex(len(ds), p)])
	}
	return out
}

// percentileIndex is the nearest-rank method: the smallest value with at
// least p of the samples at or below it.
func percentileIndex(n int, p float64) int {
	return max(0, int(math.Ceil(p*float64(n)))-1)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// report prints a side-by-side table of summaries written with -json.
// Several files with the same label (repeated runs of one policy) are
// combined into one row showing the mean and the min-max range.
func report(files []string) {
	var sums []summary
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			log.Fatal(err)
		}
		var s summary
		if err := json.Unmarshal(b, &s); err != nil {
			log.Fatalf("%s: %v", f, err)
		}
		sums = append(sums, s)
	}
	printReport(os.Stdout, sums)
}

func printReport(w io.Writer, sums []summary) {
	var labels []string
	byLabel := map[string][]summary{}
	for _, s := range sums {
		if _, seen := byLabel[s.Label]; !seen {
			labels = append(labels, s.Label)
		}
		byLabel[s.Label] = append(byLabel[s.Label], s)
	}

	fmt.Fprintf(w, "%-14s %4s %7s %6s  %-18s %-18s %-18s %-18s\n",
		"policy", "runs", "req/s", "errors", "TTFT p50 ms", "TTFT p90 ms", "TTFT p99 ms", "hit rate %")
	for _, label := range labels {
		runs := byLabel[label]
		pick := func(f func(summary) float64) []float64 {
			var out []float64
			for _, s := range runs {
				out = append(out, f(s))
			}
			return out
		}
		errs := 0
		for _, s := range runs {
			errs += s.Errors
		}
		hit := "-"
		if runs[0].HitRate != nil {
			hit = spread(pick(func(s summary) float64 { return *s.HitRate * 100 }), "%.1f")
		}
		fmt.Fprintf(w, "%-14s %4d %7.1f %6d  %-18s %-18s %-18s %-18s\n",
			label, len(runs), mean(pick(func(s summary) float64 { return s.RequestsPerSec })), errs,
			spread(pick(func(s summary) float64 { return s.TTFTms["p50"] }), "%.0f"),
			spread(pick(func(s summary) float64 { return s.TTFTms["p90"] }), "%.0f"),
			spread(pick(func(s summary) float64 { return s.TTFTms["p99"] }), "%.0f"),
			hit)
	}
}

// spread formats one value as-is, or several as "mean (min-max)".
func spread(vs []float64, format string) string {
	if len(vs) == 1 {
		return fmt.Sprintf(format, vs[0])
	}
	return fmt.Sprintf(format+" ("+format+"-"+format+")", mean(vs), slices.Min(vs), slices.Max(vs))
}

func mean(vs []float64) float64 {
	total := 0.0
	for _, v := range vs {
		total += v
	}
	return total / float64(len(vs))
}

// counters are summed over replicas. vLLM counts in tokens; the ratio is
// the hit rate either way.
type counters struct{ hits, queries float64 }

func (a counters) minus(b counters) counters { return counters{a.hits - b.hits, a.queries - b.queries} }

func scrape(client *http.Client, urls []string) (counters, error) {
	var total counters
	for _, u := range urls {
		resp, err := client.Get(u)
		if err != nil {
			return total, err
		}
		c, err := parseCounters(resp.Body)
		resp.Body.Close()
		if err != nil {
			return total, fmt.Errorf("%s: %w", u, err)
		}
		total.hits += c.hits
		total.queries += c.queries
	}
	return total, nil
}

// parseCounters reads Prometheus text format lines such as
// `vllm:prefix_cache_hits_total{model_name="m"} 240`.
func parseCounters(r io.Reader) (counters, error) {
	var c counters
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name := line[:strings.IndexAny(line+" ", "{ ")]
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		switch name {
		case "vllm:prefix_cache_hits_total":
			c.hits += v
		case "vllm:prefix_cache_queries_total":
			c.queries += v
		}
	}
	return c, sc.Err()
}

var words = strings.Fields("the a model router cache prefix token block replica latency request " +
	"stream kernel tensor batch queue schedule memory compute attention layer weight gpu node")

// text returns n chars of random words.
func text(rng *rand.Rand, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(words[rng.Intn(len(words))])
		b.WriteByte(' ')
	}
	return b.String()[:n]
}
