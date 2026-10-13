package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/suchethg/llm-router/internal/backend"
	"github.com/suchethg/llm-router/internal/policy"
)

func TestForwardsChat(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s", r.Method, r.URL.Path, body)
	}))
	defer backend.Close()
	router := httptest.NewServer(newRouter(t, backend.URL))
	defer router.Close()

	resp, err := http.Post(router.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if want := `POST /v1/chat/completions {"x":1}`; string(got) != want {
		t.Fatalf("backend saw %q, want %q", got, want)
	}
}

// The client must receive the first event while the backend is still
// generating, not after the response completes.
func TestStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release // hold the response open until the client has seen "first"
		fmt.Fprint(w, "data: second\n\n")
	}))
	defer backend.Close()
	router := httptest.NewServer(newRouter(t, backend.URL))
	defer router.Close()

	resp, err := http.Post(router.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first line = %q, %v", line, err)
	}
	close(release)
}

func TestUnknownRoute(t *testing.T) {
	router := httptest.NewServer(newRouter(t, "http://127.0.0.1:1"))
	defer router.Close()

	resp, err := http.Get(router.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
}

// Each replica answers with its own name. With round-robin, 8 requests over
// 4 replicas must land exactly twice on each, and X-Routed-To must name the
// replica that actually answered.
func TestSpreadsAcrossBackends(t *testing.T) {
	var urls []string
	nameOf := map[string]string{} // host:port -> replica name
	for i := 0; i < 4; i++ {
		name := fmt.Sprint("replica-", i)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, name)
		}))
		defer srv.Close()
		urls = append(urls, srv.URL)
		nameOf[strings.TrimPrefix(srv.URL, "http://")] = name
	}
	router := httptest.NewServer(newRouter(t, urls...))
	defer router.Close()

	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		resp, err := http.Post(router.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		served, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if routed := nameOf[resp.Header.Get("X-Routed-To")]; routed != string(served) {
			t.Fatalf("X-Routed-To says %q but %q answered", routed, served)
		}
		counts[string(served)]++
	}
	for _, name := range nameOf {
		if counts[name] != 2 {
			t.Errorf("%s served %d requests, want 2 (counts %v)", name, counts[name], counts)
		}
	}
}

// The in-flight count must cover the whole stream and drop back to zero
// when it ends, including when the client disconnects mid-stream.
func TestInflightCoversStreamAndReleases(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			fmt.Fprint(w, "data: [DONE]\n\n")
		case <-r.Context().Done(): // client went away
		}
	}))
	defer upstream.Close()
	bs := parse(t, upstream.URL)
	router := httptest.NewServer(newHandler(bs, &policy.RoundRobin{}))
	defer router.Close()

	open := func() *http.Response {
		resp := post(t, router.URL, `{}`)
		bufio.NewReader(resp.Body).ReadString('\n') // stream has started
		return resp
	}

	// Finished normally.
	resp := open()
	if n := bs[0].Inflight(); n != 1 {
		t.Fatalf("inflight mid-stream = %d, want 1", n)
	}
	close(release)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	waitInflight(t, bs[0], 0)

	// Client disconnects mid-stream.
	release = make(chan struct{})
	resp = open()
	resp.Body.Close()
	waitInflight(t, bs[0], 0)
}

// The router releases after ServeHTTP returns, which races with the
// client's view of the response ending, so poll briefly.
func waitInflight(t *testing.T, b *backend.Backend, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for b.Inflight() != want {
		if time.Now().After(deadline) {
			t.Fatalf("inflight = %d, want %d", b.Inflight(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func parse(t *testing.T, urls ...string) []*backend.Backend {
	t.Helper()
	bs, err := backend.ParseList(strings.Join(urls, ","))
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

func newRouter(t *testing.T, urls ...string) http.Handler {
	t.Helper()
	return newHandler(parse(t, urls...), &policy.RoundRobin{})
}
