// Command router sits in front of vLLM replicas and decides which one serves
// each request.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/suchethg/llm-router/internal/backend"
	"github.com/suchethg/llm-router/internal/policy"
)

func main() {
	addr := flag.String("addr", ":8090", "listen address (8080 is often taken by Jenkins and similar tools)")
	backends := flag.String("backends", "http://localhost:8001,http://localhost:8002,http://localhost:8003,http://localhost:8004", "comma-separated vLLM base URLs")
	policyName := flag.String("policy", "round-robin", "routing policy: round-robin | least-load")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	bs, err := backend.ParseList(*backends)
	if err != nil {
		log.Error("bad -backends", "err", err)
		os.Exit(1)
	}
	pol, err := policy.New(*policyName)
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
		b := pol.Pick(bs)
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
