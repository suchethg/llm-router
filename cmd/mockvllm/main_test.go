package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig() config {
	return config{name: "t", model: "m", maxSeqs: 4, defaultMaxToks: 3}
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNonStreaming(t *testing.T) {
	srv := httptest.NewServer(newServer(testConfig()))
	defer srv.Close()

	resp := post(t, srv.URL, `{"messages":[{"role":"user","content":"0123456789abcdef"}],"max_tokens":5}`)
	defer resp.Body.Close()
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 || out.Choices[0].Message.Role != "assistant" {
		t.Fatalf("bad response: %+v", out)
	}
	// "user" + 16 chars = 20 chars = 5 tokens.
	if out.Usage.PromptTokens != 5 || out.Usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	if resp.Header.Get("X-Replica") != "t" {
		t.Fatalf("X-Replica = %q", resp.Header.Get("X-Replica"))
	}
}

func TestStreamingFormat(t *testing.T) {
	srv := httptest.NewServer(newServer(testConfig()))
	defer srv.Close()

	resp := post(t, srv.URL, `{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			events = append(events, data)
		}
	}
	// role chunk + 3 tokens (default max) + finish chunk + [DONE]
	if len(events) != 6 || events[5] != "[DONE]" {
		t.Fatalf("got %d events: %v", len(events), events)
	}
	if !strings.Contains(events[0], `"role":"assistant"`) || !strings.Contains(events[4], `"finish_reason":"length"`) {
		t.Fatalf("bad first/last chunk: %s / %s", events[0], events[4])
	}
}

// The first token can't arrive before prefill finishes, and prefill grows
// with prompt length.
func TestPrefillDelaysFirstToken(t *testing.T) {
	c := testConfig()
	c.prefillPerTok = time.Millisecond
	srv := httptest.NewServer(newServer(c))
	defer srv.Close()

	ttft := func(promptChars int) time.Duration {
		body := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", promptChars) + `"}],"stream":true}`
		start := time.Now()
		resp := post(t, srv.URL, body)
		defer resp.Body.Close()
		bufio.NewReader(resp.Body).ReadString('\n') // first event
		return time.Since(start)
	}
	short, long := ttft(4), ttft(400) // ~2 vs ~101 prompt tokens
	if long < 100*time.Millisecond {
		t.Fatalf("long prompt TTFT %v, want >= 100ms", long)
	}
	if long-short < 80*time.Millisecond {
		t.Fatalf("TTFT short=%v long=%v: prefill should scale with prompt", short, long)
	}
}

// With max-seqs=1, a second concurrent request must wait for the first.
func TestMaxSeqsQueues(t *testing.T) {
	c := testConfig()
	c.maxSeqs = 1
	c.decodePerTok = 50 * time.Millisecond
	srv := httptest.NewServer(newServer(c))
	defer srv.Close()

	body := `{"messages":[{"role":"user","content":"hi"}],"max_tokens":2}`
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, srv.URL, body).Body.Close()
		}()
	}
	wg.Wait()
	// Each request decodes for 2*50ms; serialized that's >= 200ms.
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("2 requests with max-seqs=1 took %v, want >= 200ms (queued)", d)
	}
}

func TestRejectsBadRequests(t *testing.T) {
	srv := httptest.NewServer(newServer(testConfig()))
	defer srv.Close()
	for _, body := range []string{`not json`, `{"messages":[]}`} {
		resp := post(t, srv.URL, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestModels(t *testing.T) {
	srv := httptest.NewServer(newServer(testConfig()))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 1 || out.Data[0].ID != "m" {
		t.Fatalf("models = %+v", out)
	}
}
