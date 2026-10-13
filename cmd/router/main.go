// Command router sits in front of vLLM replicas and decides which one serves
// each request. For now it forwards everything to a single backend.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	backend := flag.String("backend", "http://localhost:8000", "vLLM base URL")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	target, err := url.Parse(*backend)
	if err != nil {
		log.Error("bad -backend", "err", err)
		os.Exit(1)
	}

	log.Info("router listening", "addr", *addr, "backend", target.String())
	if err := http.ListenAndServe(*addr, newHandler(target)); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func newHandler(target *url.URL) http.Handler {
	// ReverseProxy flushes text/event-stream responses on every write, so
	// vLLM's streamed tokens pass through as they are generated.
	proxy := httputil.NewSingleHostReverseProxy(target)

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
