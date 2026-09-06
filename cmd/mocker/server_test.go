package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	s := NewServer(cfg)
	// Deterministic by default: no real sleeping, no random failures.
	s.sleep = func(time.Duration) {}
	s.randFloat = func() float64 { return 0.5 }
	s.pickErrStatus = func() int { return http.StatusTooManyRequests }
	return s
}

func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestNonStreamingCompletion(t *testing.T) {
	s := newTestServer(t, Config{Tokens: 8})
	rec := post(t, s, `{"model":"mock-small","messages":[{"role":"user","content":"hello there friend"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}

	var resp chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q", resp.Object)
	}
	if !strings.HasPrefix(resp.ID, "chatcmpl-") {
		t.Errorf("id = %q, want chatcmpl- prefix", resp.ID)
	}
	if resp.Model != "mock-small" {
		t.Errorf("model = %q, want the requested model echoed back", resp.Model)
	}
	if resp.Created == 0 {
		t.Error("created is unset")
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	c := resp.Choices[0]
	if c.Message.Role != "assistant" || c.Message.Content == "" {
		t.Errorf("message = %#v", c.Message)
	}
	if c.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", c.FinishReason)
	}
	if resp.Usage == nil {
		t.Fatal("usage is missing")
	}
	if resp.Usage.CompletionTokens != 8 {
		t.Errorf("completion_tokens = %d, want 8", resp.Usage.CompletionTokens)
	}
	if resp.Usage.PromptTokens <= 0 {
		t.Errorf("prompt_tokens = %d, want a plausible positive count", resp.Usage.PromptTokens)
	}
	if got, want := resp.Usage.TotalTokens, resp.Usage.PromptTokens+resp.Usage.CompletionTokens; got != want {
		t.Errorf("total_tokens = %d, want %d", got, want)
	}
	if n := len(strings.Fields(c.Message.Content)); n != 8 {
		t.Errorf("content has %d words, want 8 to match completion_tokens", n)
	}
}

func TestMaxTokensTruncates(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		maxTokens  string
		wantTokens int
		wantFinish string
	}{
		{"under the cap", 10, `,"max_tokens":3`, 3, "length"},
		{"over the cap", 10, `,"max_tokens":50`, 10, "stop"},
		{"absent", 10, "", 10, "stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, Config{Tokens: tt.configured})
			rec := post(t, s, `{"model":"m","messages":[{"role":"user","content":"hi"}]`+tt.maxTokens+`}`)

			var resp chatResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Usage.CompletionTokens != tt.wantTokens {
				t.Errorf("completion_tokens = %d, want %d", resp.Usage.CompletionTokens, tt.wantTokens)
			}
			if resp.Choices[0].FinishReason != tt.wantFinish {
				t.Errorf("finish_reason = %q, want %q", resp.Choices[0].FinishReason, tt.wantFinish)
			}
		})
	}
}

// sseEvents splits an SSE body into its `data:` payloads.
func sseEvents(t *testing.T, body io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			t.Fatalf("malformed SSE line: %q", line)
		}
		out = append(out, data)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

func TestStreamingCompletion(t *testing.T) {
	const tokens = 5
	s := newTestServer(t, Config{Tokens: tokens})
	rec := post(t, s, `{"model":"mock-small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	events := sseEvents(t, rec.Body)
	// role chunk + N token chunks + finish chunk + usage chunk + [DONE]
	if want := tokens + 4; len(events) != want {
		t.Fatalf("got %d events, want %d:\n%s", len(events), want, strings.Join(events, "\n"))
	}

	if got := events[len(events)-1]; got != "[DONE]" {
		t.Errorf("last event = %q, want [DONE]", got)
	}

	var open chunkResponse
	if err := json.Unmarshal([]byte(events[0]), &open); err != nil {
		t.Fatalf("decode opening chunk: %v", err)
	}
	if open.Object != "chat.completion.chunk" || open.Choices[0].Delta.Role != "assistant" {
		t.Errorf("opening chunk = %#v", open)
	}

	var text strings.Builder
	id := open.ID
	for i, raw := range events[1 : 1+tokens] {
		var c chunkResponse
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatalf("decode token chunk %d: %v", i, err)
		}
		if c.ID != id {
			t.Errorf("chunk %d id = %q, want %q — all chunks share one completion id", i, c.ID, id)
		}
		if c.Choices[0].FinishReason != nil {
			t.Errorf("chunk %d carries a finish_reason before the end", i)
		}
		if c.Usage != nil {
			t.Errorf("chunk %d carries usage before the end", i)
		}
		text.WriteString(c.Choices[0].Delta.Content)
	}
	if n := len(strings.Fields(text.String())); n != tokens {
		t.Errorf("streamed %d words, want %d", n, tokens)
	}

	var finish chunkResponse
	if err := json.Unmarshal([]byte(events[1+tokens]), &finish); err != nil {
		t.Fatalf("decode finish chunk: %v", err)
	}
	if finish.Choices[0].FinishReason == nil || *finish.Choices[0].FinishReason != "stop" {
		t.Errorf("finish chunk = %#v", finish.Choices[0])
	}

	// Acceptance: the stream ends with a usage-bearing chunk, then [DONE].
	var last chunkResponse
	if err := json.Unmarshal([]byte(events[len(events)-2]), &last); err != nil {
		t.Fatalf("decode usage chunk: %v", err)
	}
	if last.Usage == nil {
		t.Fatalf("penultimate event carries no usage: %s", events[len(events)-2])
	}
	if last.Usage.CompletionTokens != tokens || last.Usage.PromptTokens <= 0 {
		t.Errorf("usage = %#v", last.Usage)
	}
	if last.Usage.TotalTokens != last.Usage.PromptTokens+last.Usage.CompletionTokens {
		t.Errorf("usage does not add up: %#v", last.Usage)
	}
	if len(last.Choices) != 0 {
		t.Errorf("usage chunk carries %d choices, want none", len(last.Choices))
	}
}

func TestStreamingIncludeUsageOptOut(t *testing.T) {
	tests := []struct {
		name      string
		opts      string
		wantUsage bool
	}{
		{"default on", ``, true},
		{"explicit true", `,"stream_options":{"include_usage":true}`, true},
		{"explicit false", `,"stream_options":{"include_usage":false}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, Config{Tokens: 2})
			rec := post(t, s, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]`+tt.opts+`}`)

			events := sseEvents(t, rec.Body)
			if events[len(events)-1] != "[DONE]" {
				t.Fatalf("stream does not end with [DONE]: %v", events)
			}

			var last chunkResponse
			if err := json.Unmarshal([]byte(events[len(events)-2]), &last); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if gotUsage := last.Usage != nil; gotUsage != tt.wantUsage {
				t.Errorf("usage chunk present = %v, want %v", gotUsage, tt.wantUsage)
			}
		})
	}
}

func TestFailureInjection(t *testing.T) {
	tests := []struct {
		name       string
		failRate   float64
		draw       float64
		status     int
		wantStatus int
		wantType   string
		wantRetry  bool
	}{
		{"never fails", 0, 0, 0, http.StatusOK, "", false},
		{"draw above rate", 0.3, 0.9, http.StatusInternalServerError, http.StatusOK, "", false},
		{"429", 1, 0, http.StatusTooManyRequests, http.StatusTooManyRequests, "rate_limit_error", true},
		{"500", 1, 0, http.StatusInternalServerError, http.StatusInternalServerError, "api_error", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, Config{Tokens: 2, FailRate: tt.failRate})
			s.randFloat = func() float64 { return tt.draw }
			s.pickErrStatus = func() int { return tt.status }

			rec := post(t, s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantStatus == http.StatusOK {
				return
			}

			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if env.Error.Type != tt.wantType || env.Error.Message == "" {
				t.Errorf("error = %#v", env.Error)
			}
			if got := rec.Header().Get("Retry-After") != ""; got != tt.wantRetry {
				t.Errorf("Retry-After present = %v, want %v", got, tt.wantRetry)
			}
		})
	}
}

func TestFailRateIsProbabilistic(t *testing.T) {
	s := newTestServer(t, Config{Tokens: 1, FailRate: 0.5})
	draws := []float64{0.1, 0.9, 0.4, 0.99, 0.5}
	i := 0
	s.randFloat = func() float64 { d := draws[i]; i++; return d }

	var failures int
	for range draws {
		if post(t, s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`).Code != http.StatusOK {
			failures++
		}
	}
	// Draws strictly below 0.5 fail: 0.1 and 0.4.
	if failures != 2 {
		t.Errorf("failures = %d, want 2", failures)
	}
}

func TestBadRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"malformed json", `{"model":`, http.StatusBadRequest},
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, Config{Tokens: 2})
			rec := post(t, s, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if env.Error.Type != "invalid_request_error" || env.Error.Message == "" {
				t.Errorf("error = %#v", env.Error)
			}
		})
	}
}

func TestRouting(t *testing.T) {
	s := newTestServer(t, Config{Tokens: 1, Models: []string{"mock-small", "mock-large"}})

	t.Run("models", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var out struct {
			Object string `json:"object"`
			Data   []struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Object != "list" || len(out.Data) != 2 {
			t.Fatalf("models = %#v", out)
		}
		if out.Data[0].ID != "mock-small" || out.Data[1].ID != "mock-large" {
			t.Errorf("model ids = %#v", out.Data)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("unknown path", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/nope", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

func TestFirstByteDelay(t *testing.T) {
	tests := []struct {
		name    string
		latency time.Duration
		jitter  time.Duration
		draw    float64
		want    time.Duration
	}{
		{"no jitter", 50 * time.Millisecond, 0, 0.5, 50 * time.Millisecond},
		{"jitter low end", 50 * time.Millisecond, 10 * time.Millisecond, 0, 40 * time.Millisecond},
		{"jitter midpoint", 50 * time.Millisecond, 10 * time.Millisecond, 0.5, 50 * time.Millisecond},
		{"jitter high end", 50 * time.Millisecond, 10 * time.Millisecond, 1, 60 * time.Millisecond},
		{"clamped at zero", 5 * time.Millisecond, 10 * time.Millisecond, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, Config{Latency: tt.latency, Jitter: tt.jitter})
			s.randFloat = func() float64 { return tt.draw }
			if got := s.firstByteDelay(); got != tt.want {
				t.Errorf("firstByteDelay() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestTokenInterval(t *testing.T) {
	tests := []struct {
		tps  float64
		want time.Duration
	}{
		{0, 0},
		{-1, 0},
		{50, 20 * time.Millisecond},
		{1000, time.Millisecond},
	}
	for _, tt := range tests {
		s := newTestServer(t, Config{TPS: tt.tps})
		if got := s.tokenInterval(); got != tt.want {
			t.Errorf("tokenInterval(tps=%g) = %s, want %s", tt.tps, got, tt.want)
		}
	}
}

// TestLatencyAndPacingAreRealTime exercises the actual clock, over an end-to-end
// HTTP connection, to prove the mocker delays the first byte and paces tokens
// rather than just computing the right durations.
func TestLatencyAndPacingAreRealTime(t *testing.T) {
	const (
		latency = 30 * time.Millisecond
		tokens  = 4
		tps     = 200 // 5ms per token
	)
	s := NewServer(Config{Latency: latency, Tokens: tokens, TPS: tps})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	start := time.Now()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read first event: %v", err)
	}
	firstByte := time.Since(start)
	if firstByte < latency {
		t.Errorf("first byte after %s, want at least %s", firstByte, latency)
	}

	if _, err := io.Copy(io.Discard, br); err != nil {
		t.Fatalf("drain: %v", err)
	}
	total := time.Since(start)
	// tokens-1 gaps between token chunks.
	minPaced := latency + time.Duration(tokens-1)*(time.Second/tps)
	if total < minPaced {
		t.Errorf("stream completed in %s, want at least %s for %d tokens at %d tps",
			total, minPaced, tokens, tps)
	}
}

// TestStreamStopsOnClientDisconnect covers the mocker abandoning a paced stream
// when the caller goes away, which is what the M4 disconnect test will rely on.
func TestStreamStopsOnClientDisconnect(t *testing.T) {
	s := NewServer(Config{Tokens: 10_000, TPS: 100}) // ~100s if it ran to completion
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read first event: %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	// ts.Close blocks until outstanding handlers return; if the handler ignored
	// the cancelled context this would hang until the test times out.
	done := make(chan struct{})
	go func() { ts.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not stop streaming after the client disconnected")
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	tests := []struct {
		name string
		msgs []chatMessage
		want int
	}{
		{"none", nil, 0},
		{"string content", []chatMessage{{Role: "user", Content: json.RawMessage(`"12345678901234567890"`)}}, 6},
		{
			"multimodal parts",
			[]chatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"12345678901234567890"}]`)}},
			6,
		},
		{"empty content still counts as one", []chatMessage{{Role: "u", Content: json.RawMessage(`""`)}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimatePromptTokens(tt.msgs); got != tt.want {
				t.Errorf("estimatePromptTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCompletionTextMatchesStreamedTokens(t *testing.T) {
	const n = 30
	var streamed strings.Builder
	for i := range n {
		streamed.WriteString(tokenAt(i))
	}
	if got, want := completionText(n), strings.TrimSuffix(streamed.String(), " "); got != want {
		t.Errorf("completionText(%d) = %q, want %q", n, got, want)
	}
	if got := completionText(0); got != "" {
		t.Errorf("completionText(0) = %q, want empty", got)
	}
}
