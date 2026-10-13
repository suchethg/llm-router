package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestForwardsChat(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s", r.Method, r.URL.Path, body)
	}))
	defer backend.Close()
	router := httptest.NewServer(newHandler(mustParse(t, backend.URL)))
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
	router := httptest.NewServer(newHandler(mustParse(t, backend.URL)))
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
	router := httptest.NewServer(newHandler(mustParse(t, "http://127.0.0.1:1")))
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

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
