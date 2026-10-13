// Command router sits in front of vLLM replicas and decides which one serves
// each request.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/suchethg/llm-router/internal/backend"
	"github.com/suchethg/llm-router/internal/policy"
	"github.com/suchethg/llm-router/internal/prefix"
)

// maxBody bounds how much of a request the router buffers. 8 MiB is ~2M
// tokens, far beyond any context window this deployment serves.
const maxBody = 8 << 20

func main() {
	addr := flag.String("addr", ":8090", "listen address (8080 is often taken by Jenkins and similar tools)")
	backends := flag.String("backends", "http://localhost:8001,http://localhost:8002,http://localhost:8003,http://localhost:8004", "comma-separated vLLM base URLs")
	policyName := flag.String("policy", "round-robin", "routing policy: round-robin | least-load | prefix-aware")
	opts := policy.DefaultOptions()
	flag.IntVar(&opts.BlockChars, "block-chars", opts.BlockChars, "chars per KV block; must match the replicas")
	flag.IntVar(&opts.CacheBlocks, "cache-blocks", opts.CacheBlocks, "shadow cache size per replica, in blocks")
	flag.Int64Var(&opts.LoadSlack, "load-slack", opts.LoadSlack, "extra in-flight requests accepted to get a cache hit")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	bs, err := backend.ParseList(*backends)
	if err != nil {
		log.Error("bad -backends", "err", err)
		os.Exit(1)
	}
	pol, err := policy.New(*policyName, opts)
	if err != nil {
		log.Error("bad -policy", "err", err)
		os.Exit(1)
	}

	log.Info("router listening", "addr", *addr, "policy", pol.Name(), "backends", len(bs))
	if err := http.ListenAndServe(*addr, newHandler(bs, pol)); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func newHandler(bs []*backend.Backend, pol policy.Policy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			status := http.StatusBadRequest
			if errors.As(err, new(*http.MaxBytesError)) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, err.Error(), status)
			return
		}
		// If the body isn't a chat request we can parse, route on load alone
		// and let vLLM return its own error for it.
		var chat struct {
			Messages []prefix.Message `json:"messages"`
		}
		if json.Unmarshal(body, &chat) != nil {
			chat.Messages = nil
		}
		// The proxy forwards r.Body, which we just consumed: put it back.
		r.Body = io.NopCloser(bytes.NewReader(body))

		b := pol.Pick(&policy.Request{Prompt: prefix.RenderChat(chat.Messages)}, bs)
		// Held until ServeHTTP returns, i.e. the whole response has streamed
		// or the client has gone away, so decode time counts as load.
		b.Acquire()
		defer b.Release()
		w.Header().Set("X-Routed-To", b.Name)
		b.Proxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
