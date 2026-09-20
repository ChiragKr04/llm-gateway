package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newLoadTarget starts a server and returns its chat-completions URL along
// with a counter of the requests it saw.
func newLoadTarget(t *testing.T, h http.HandlerFunc) (string, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + chatCompletionsPath, &hits
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
}

func testOptions(o Options) Options {
	if o.Concurrency == 0 {
		o.Concurrency = 8
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	if len(o.Body) == 0 {
		o.Body = []byte(`{"model":"mock-small","messages":[{"role":"user","content":"hi"}]}`)
	}
	return o
}

func TestRunHoldsTheOfferedRate(t *testing.T) {
	url, hits := newLoadTarget(t, okHandler)

	// 200 RPS for 500ms is 100 requests, scheduled 5ms apart.
	res, err := Run(context.Background(), legDirect, url, testOptions(Options{
		RPS: 200, Duration: 500 * time.Millisecond,
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Measured < 90 || res.Measured > 100 {
		t.Errorf("measured = %d, want ~100", res.Measured)
	}
	if got := hits.Load(); got != res.Attempted {
		t.Errorf("server saw %d requests, driver issued %d", got, res.Attempted)
	}
	if res.Succeeded != res.Measured {
		t.Errorf("succeeded = %d, measured = %d, failures = %v", res.Succeeded, res.Measured, res.Failures)
	}
	if rps := res.AchievedRPS(); rps < 160 || rps > 240 {
		t.Errorf("achieved RPS = %.1f, want ~200", rps)
	}
	if res.Latency.TotalCount() != res.Succeeded {
		t.Errorf("latency samples = %d, succeeded = %d", res.Latency.TotalCount(), res.Succeeded)
	}
	// An instant local handler must not look like a saturated driver.
	if res.Saturated() {
		t.Errorf("reported saturated against an instant handler: sched p99 = %s",
			dur(res.Schedule.ValueAtQuantile(99)))
	}
}

func TestWarmupRequestsAreIssuedButNotRecorded(t *testing.T) {
	url, hits := newLoadTarget(t, okHandler)

	// 100 RPS: 20 warmup requests over 200ms, then 30 measured over 300ms.
	res, err := Run(context.Background(), legDirect, url, testOptions(Options{
		RPS: 100, Warmup: 200 * time.Millisecond, Duration: 300 * time.Millisecond,
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Measured < 26 || res.Measured > 30 {
		t.Errorf("measured = %d, want ~30 (warmup excluded)", res.Measured)
	}
	if res.Attempted <= res.Measured {
		t.Errorf("attempted = %d, measured = %d: warmup load was never issued", res.Attempted, res.Measured)
	}
	if hits.Load() != res.Attempted {
		t.Errorf("server saw %d requests, driver issued %d", hits.Load(), res.Attempted)
	}
	if res.Latency.TotalCount() != res.Measured {
		t.Errorf("latency samples = %d, want %d: warmup leaked into the histogram",
			res.Latency.TotalCount(), res.Measured)
	}
}

func TestFailuresAreCategorisedAndKeptOutOfTheHistogram(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, "status_5xx"},
		{"rate limited", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
		}, "status_429"},
		{"bad request", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusBadRequest)
		}, "status_4xx"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, _ := newLoadTarget(t, tc.handler)
			res, err := Run(context.Background(), legDirect, url, testOptions(Options{
				RPS: 200, Duration: 150 * time.Millisecond,
			}))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Succeeded != 0 {
				t.Errorf("succeeded = %d, want 0", res.Succeeded)
			}
			if res.Failures[tc.want] != res.Measured {
				t.Errorf("failures = %v, want all %d under %q", res.Failures, res.Measured, tc.want)
			}
			// A 20µs error must never flatter the latency percentiles.
			if res.Latency.TotalCount() != 0 {
				t.Errorf("latency samples = %d, want 0: a failed request was recorded",
					res.Latency.TotalCount())
			}
			if res.SuccessRate() != 0 {
				t.Errorf("success rate = %v, want 0", res.SuccessRate())
			}
		})
	}
}

// sseHandler writes chunks tokens and, when done is true, terminates the
// stream with the [DONE] marker.
func sseHandler(chunks int, done bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := range chunks {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
			flusher.Flush()
		}
		if done {
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
	}
}

func TestStreamIsReadToDone(t *testing.T) {
	url, _ := newLoadTarget(t, sseHandler(20, true))

	res, err := Run(context.Background(), legDirect, url, testOptions(Options{
		RPS: 100, Duration: 200 * time.Millisecond, Stream: true,
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Succeeded == 0 || res.Succeeded != res.Measured {
		t.Fatalf("succeeded = %d of %d measured, failures = %v", res.Succeeded, res.Measured, res.Failures)
	}
	if res.TTFB.TotalCount() != res.Succeeded {
		t.Errorf("ttfb samples = %d, want %d", res.TTFB.TotalCount(), res.Succeeded)
	}
	if res.TTFB.ValueAtQuantile(50) > res.Latency.ValueAtQuantile(50) {
		t.Errorf("ttfb p50 %s exceeds latency p50 %s",
			dur(res.TTFB.ValueAtQuantile(50)), dur(res.Latency.ValueAtQuantile(50)))
	}
	if res.Bytes == 0 {
		t.Error("no stream bytes counted")
	}
}

func TestStreamWithoutDoneIsAFailure(t *testing.T) {
	url, _ := newLoadTarget(t, sseHandler(5, false))

	res, err := Run(context.Background(), legDirect, url, testOptions(Options{
		RPS: 100, Duration: 150 * time.Millisecond, Stream: true,
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Succeeded != 0 {
		t.Errorf("succeeded = %d, want 0: a truncated stream is not a success", res.Succeeded)
	}
	if res.Failures["stream_truncated"] != res.Measured {
		t.Errorf("failures = %v, want all %d truncated", res.Failures, res.Measured)
	}
}

func TestRunRejectsInvalidOptions(t *testing.T) {
	url, _ := newLoadTarget(t, okHandler)
	for _, o := range []Options{
		{RPS: 0, Duration: time.Second, Concurrency: 1},
		{RPS: 10, Duration: 0, Concurrency: 1},
		{RPS: 10, Duration: time.Second, Concurrency: 0},
	} {
		if _, err := Run(context.Background(), legDirect, url, o); err == nil {
			t.Errorf("Run(%+v) = nil error, want a rejection", o)
		}
	}
}

func TestCancellationStopsTheRun(t *testing.T) {
	url, _ := newLoadTarget(t, okHandler)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := Run(ctx, legDirect, url, testOptions(Options{
		RPS: 50, Duration: 10 * time.Second,
	}))
	if err == nil {
		t.Error("Run returned a nil error after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Run took %s to notice cancellation", elapsed)
	}
	if res == nil {
		t.Fatal("Run returned no result; partial measurements are still worth reporting")
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "http://127.0.0.1:8081", want: "http://127.0.0.1:8081/v1/chat/completions"},
		{in: "http://127.0.0.1:8081/", want: "http://127.0.0.1:8081/v1/chat/completions"},
		{in: "127.0.0.1:8081", want: "http://127.0.0.1:8081/v1/chat/completions"},
		{in: "http://gw.local/anthropic/v1/messages", want: "http://gw.local/anthropic/v1/messages"},
		{in: "http:// bad host", wantErr: true},
		{in: "http:///nohost", wantErr: true},
	}
	for _, tc := range tests {
		got, err := resolveURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("resolveURL(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("resolveURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildBody(t *testing.T) {
	body, err := buildBody("mock-small", 12, 0, false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["model"] != "mock-small" {
		t.Errorf("model = %v", got["model"])
	}
	if _, ok := got["max_tokens"]; ok {
		t.Error("max_tokens present although the flag was 0")
	}
	if _, ok := got["stream_options"]; ok {
		t.Error("stream_options present on a non-streaming request")
	}

	body, err = buildBody("mock-large", 4, 64, true)
	if err != nil {
		t.Fatalf("buildBody streaming: %v", err)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["stream"] != true {
		t.Errorf("stream = %v, want true", got["stream"])
	}
	if got["max_tokens"] != float64(64) {
		t.Errorf("max_tokens = %v, want 64", got["max_tokens"])
	}
	// M4's usage accumulation depends on the upstream being asked for usage.
	opts, ok := got["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage true", got["stream_options"])
	}
}

func TestPromptLength(t *testing.T) {
	for _, n := range []int{1, 8, 64, 200} {
		if got := len(splitFields(prompt(n))); got != n {
			t.Errorf("prompt(%d) has %d words", n, got)
		}
	}
}

func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
