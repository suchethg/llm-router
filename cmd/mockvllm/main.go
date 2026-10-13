// Command mockvllm is a CPU-only stand-in for a vLLM OpenAI-compatible
// server, so the router can be developed and benchmarked without GPUs.
//
// It reproduces the parts of vLLM that affect routing, not the model:
//   - the same endpoints and JSON/SSE shapes, so clients can't tell it apart;
//   - prefill time that grows with prompt length, paid before the first token;
//   - a per-token decode delay after that;
//   - a cap on concurrently running requests (--max-num-seqs), with the rest
//     queued, which is what makes load balancing matter.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

type config struct {
	name           string
	model          string
	maxSeqs        int
	prefillFixed   time.Duration // per-request overhead before prefill
	prefillPerTok  time.Duration // compute per prompt token
	decodePerTok   time.Duration // time per output token
	defaultMaxToks int
}

func main() {
	addr := flag.String("addr", ":8001", "listen address")
	var c config
	flag.StringVar(&c.name, "name", "mock-1", "replica name, returned in the X-Replica header")
	flag.StringVar(&c.model, "model", "mock-llama-3.1-8b", "served model name")
	flag.IntVar(&c.maxSeqs, "max-seqs", 8, "max concurrently running requests, like vLLM --max-num-seqs")
	flag.DurationVar(&c.prefillFixed, "prefill-fixed", 5*time.Millisecond, "fixed overhead before prefill")
	flag.DurationVar(&c.prefillPerTok, "prefill-per-token", 200*time.Microsecond, "prefill time per prompt token")
	flag.DurationVar(&c.decodePerTok, "decode-per-token", 20*time.Millisecond, "time per output token")
	flag.IntVar(&c.defaultMaxToks, "default-max-tokens", 32, "output tokens when the request sets no max_tokens")
	flag.Parse()

	log.Printf("mockvllm %s listening on %s (max-seqs=%d)", c.name, *addr, c.maxSeqs)
	log.Fatal(http.ListenAndServe(*addr, newServer(c)))
}

type server struct {
	cfg   config
	slots chan struct{} // one token per running request
	// Prefill is compute-bound, so concurrent prefills on one GPU slow each
	// other down; holding a lock models that. Decode is memory-bound and
	// batched, so it runs concurrently.
	prefill sync.Mutex
}

func newServer(c config) http.Handler {
	s := &server{cfg: c, slots: make(chan struct{}, c.maxSeqs)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
	Stream    bool      `json:"stream"`
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages must not be empty", http.StatusBadRequest)
		return
	}
	maxToks := req.MaxTokens
	if maxToks <= 0 {
		maxToks = s.cfg.defaultMaxToks
	}
	promptToks := promptTokens(req.Messages)

	// Wait for a running slot; give up if the client goes away.
	select {
	case s.slots <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.slots }()

	s.prefill.Lock()
	time.Sleep(s.cfg.prefillFixed + time.Duration(promptToks)*s.cfg.prefillPerTok)
	s.prefill.Unlock()

	w.Header().Set("X-Replica", s.cfg.name)
	id := fmt.Sprintf("chatcmpl-%s-%d", s.cfg.name, time.Now().UnixNano())
	created := time.Now().Unix()

	if !req.Stream {
		time.Sleep(time.Duration(maxToks) * s.cfg.decodePerTok)
		writeJSON(w, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": s.cfg.model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       message{Role: "assistant", Content: fakeText(maxToks)},
				"finish_reason": "length",
			}},
			"usage": map[string]int{
				"prompt_tokens": promptToks, "completion_tokens": maxToks, "total_tokens": promptToks + maxToks,
			},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(delta map[string]string, finish any) bool {
		chunk, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": s.cfg.model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
			return false // client disconnected
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	// Like vLLM: a role-only chunk, then one chunk per token, then a final
	// chunk carrying finish_reason, then [DONE].
	if !send(map[string]string{"role": "assistant"}, nil) {
		return
	}
	for i := 0; i < maxToks; i++ {
		if i > 0 {
			time.Sleep(s.cfg.decodePerTok)
		}
		if !send(map[string]string{"content": fakeToken(i)}, nil) {
			return
		}
	}
	if !send(map[string]string{}, "length") {
		return
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (s *server) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"object": "list",
		"data":   []map[string]any{{"id": s.cfg.model, "object": "model", "owned_by": "mockvllm"}},
	})
}

// promptTokens estimates prompt length at ~4 characters per token, a common
// rule of thumb for English text with Llama-family tokenizers.
func promptTokens(msgs []message) int {
	chars := 0
	for _, m := range msgs {
		chars += len(m.Role) + len(m.Content)
	}
	return chars / 4
}

var vocab = []string{"the ", "model ", "is ", "a ", "mock ", "reply ", "with ", "tokens "}

func fakeToken(i int) string { return vocab[i%len(vocab)] }

func fakeText(n int) string {
	b := make([]byte, 0, n*6)
	for i := 0; i < n; i++ {
		b = append(b, fakeToken(i)...)
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
