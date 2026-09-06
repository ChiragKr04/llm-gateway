package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Config is the mocker's tunable behaviour. Zero values are valid: no latency,
// no jitter, no failures, unpaced streaming.
type Config struct {
	// Latency is the delay before the first byte of a response.
	Latency time.Duration
	// Jitter is added to Latency as a uniform value in [-Jitter, +Jitter].
	Jitter time.Duration
	// Tokens is the number of completion tokens generated per request.
	Tokens int
	// TPS paces streaming at this many tokens per second. Zero streams as
	// fast as the connection allows.
	TPS float64
	// FailRate is the probability in [0,1] that a request is rejected with a
	// 429 or a 500 instead of being served.
	FailRate float64
	// Models is the set of model IDs served by GET /v1/models.
	Models []string
}

// Server impersonates an OpenAI-compatible provider.
type Server struct {
	cfg Config

	// now, randFloat and pickErrStatus are injection points so tests can drive
	// the server deterministically.
	now           func() time.Time
	randFloat     func() float64
	pickErrStatus func() int
	sleep         func(d time.Duration)
}

// NewServer builds a Server from cfg.
func NewServer(cfg Config) *Server {
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"mock-small", "mock-large"}
	}
	return &Server{
		cfg:       cfg,
		now:       time.Now,
		randFloat: rand.Float64,
		pickErrStatus: func() int {
			if rand.IntN(2) == 0 {
				return http.StatusTooManyRequests
			}
			return http.StatusInternalServerError
		},
		sleep: func(d time.Duration) {
			if d > 0 {
				time.Sleep(d)
			}
		},
	}
}

// Handler returns the mocker's HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// ---------------------------------------------------------------------------
// Wire types
//
// The mocker impersonates an external provider, so it speaks OpenAI's wire
// format directly and deliberately does not import core/schemas. Nothing here
// is canonical.
// ---------------------------------------------------------------------------

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	MaxTokens     *int           `json:"max_tokens"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options"`
}

type streamOptions struct {
	IncludeUsage *bool `json:"include_usage"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *usage       `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int        `json:"index"`
	Message      chatOutMsg `json:"message"`
	FinishReason string     `json:"finish_reason"`
	Logprobs     any        `json:"logprobs"`
}

type chatOutMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chunkResponse struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list"}

	created := s.now().Unix()
	for _, id := range s.cfg.Models {
		out.Data = append(out.Data, model{ID: id, Object: "model", Created: created, OwnedBy: "mocker"})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("could not parse request body: %v", err))
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing required parameter: 'model'")
		return
	}

	// Failure injection happens before the latency sleep: real providers reject
	// over-quota requests immediately.
	if s.shouldFail() {
		s.writeInjectedFailure(w)
		return
	}

	s.sleep(s.firstByteDelay())

	n := s.completionTokens(req.MaxTokens)
	finish := "stop"
	if req.MaxTokens != nil && *req.MaxTokens < s.cfg.Tokens {
		finish = "length"
	}

	u := usage{
		PromptTokens:     estimatePromptTokens(req.Messages),
		CompletionTokens: n,
	}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens

	if req.Stream {
		s.streamCompletion(w, r, req, n, finish, u)
		return
	}

	writeJSON(w, http.StatusOK, chatResponse{
		ID:      s.newID(),
		Object:  "chat.completion",
		Created: s.now().Unix(),
		Model:   req.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatOutMsg{Role: "assistant", Content: completionText(n)},
			FinishReason: finish,
		}},
		Usage: &u,
	})
}

// streamCompletion emits the SSE sequence: a role-only opening chunk, one chunk
// per token paced at TPS, a finish_reason chunk, an optional usage-bearing
// chunk, and finally "data: [DONE]".
func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, req chatRequest, n int, finish string, u usage) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "api_error", "streaming unsupported by this server")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Streaming through a buffering proxy defeats the point of this server.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sse := &sseWriter{w: w, flusher: flusher}
	id := s.newID()
	created := s.now().Unix()

	base := func() chunkResponse {
		return chunkResponse{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model}
	}

	openWith := base()
	openWith.Choices = []chunkChoice{{Index: 0, Delta: delta{Role: "assistant"}}}
	if err := sse.event(openWith); err != nil {
		return
	}

	ctx := r.Context()
	interval := s.tokenInterval()
	for i := range n {
		if i > 0 && interval > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		} else if ctx.Err() != nil {
			return
		}

		c := base()
		c.Choices = []chunkChoice{{Index: 0, Delta: delta{Content: tokenAt(i)}}}
		if err := sse.event(c); err != nil {
			return
		}
	}

	final := base()
	final.Choices = []chunkChoice{{Index: 0, Delta: delta{}, FinishReason: &finish}}
	if err := sse.event(final); err != nil {
		return
	}

	// OpenAI gates the usage chunk on stream_options.include_usage. The mocker
	// defaults it on so a bare `curl -N` sees the terminal usage chunk; an
	// explicit false still opts out, which is the path M4 exercises when the
	// gateway injects the option itself.
	if includeUsage(req.StreamOptions) {
		usageChunk := base()
		usageChunk.Choices = []chunkChoice{}
		usageChunk.Usage = &u
		if err := sse.event(usageChunk); err != nil {
			return
		}
	}

	_ = sse.done()
}

// ---------------------------------------------------------------------------
// Behaviour helpers
// ---------------------------------------------------------------------------

func (s *Server) shouldFail() bool {
	if s.cfg.FailRate <= 0 {
		return false
	}
	return s.randFloat() < s.cfg.FailRate
}

func (s *Server) writeInjectedFailure(w http.ResponseWriter) {
	status := s.pickErrStatus()
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
		writeError(w, status, "rate_limit_error", "injected failure: rate limit exceeded")
		return
	}
	writeError(w, status, "api_error", "injected failure: upstream error")
}

// firstByteDelay is Latency perturbed by a uniform draw from [-Jitter, +Jitter],
// clamped at zero.
func (s *Server) firstByteDelay() time.Duration {
	d := s.cfg.Latency
	if s.cfg.Jitter > 0 {
		offset := time.Duration((s.randFloat()*2 - 1) * float64(s.cfg.Jitter))
		d += offset
	}
	if d < 0 {
		return 0
	}
	return d
}

func (s *Server) tokenInterval() time.Duration {
	if s.cfg.TPS <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / s.cfg.TPS)
}

// completionTokens is the configured token count, capped by the caller's
// max_tokens when it asks for fewer.
func (s *Server) completionTokens(maxTokens *int) int {
	n := s.cfg.Tokens
	if n < 0 {
		n = 0
	}
	if maxTokens != nil && *maxTokens < n {
		n = *maxTokens
	}
	if n < 0 {
		n = 0
	}
	return n
}

func (s *Server) newID() string {
	return "chatcmpl-mock" + strconv.FormatUint(rand.Uint64(), 36)
}

func includeUsage(o *streamOptions) bool {
	if o == nil || o.IncludeUsage == nil {
		return true
	}
	return *o.IncludeUsage
}

// vocabulary is cycled to build completion text. Real words (rather than
// "token1 token2") make `curl -N` output legible as it streams.
var vocabulary = []string{
	"the", "quick", "brown", "fox", "jumps", "over", "the", "lazy", "dog",
	"while", "the", "gateway", "measures", "every", "microsecond", "of",
	"overhead", "between", "client", "and", "provider", "under", "load",
}

// tokenAt is the i-th generated token, with a trailing space so concatenated
// deltas read as prose.
func tokenAt(i int) string {
	return vocabulary[i%len(vocabulary)] + " "
}

func completionText(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(tokenAt(i))
	}
	return strings.TrimSuffix(b.String(), " ")
}

// estimatePromptTokens is a deliberately crude chars/4 approximation. The
// mocker only needs plausible accounting, not a real tokenizer.
func estimatePromptTokens(msgs []chatMessage) int {
	chars := 0
	for _, m := range msgs {
		chars += len(m.Role) + len(messageText(m.Content))
	}
	n := chars / 4
	if n < 1 && len(msgs) > 0 {
		n = 1
	}
	return n
}

// messageText extracts text from OpenAI content, which is either a bare string
// or an array of typed parts.
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// sseWriter serialises one JSON payload per SSE event and flushes it, so each
// token leaves the process immediately rather than sitting in a buffer.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	buf     bytes.Buffer
}

func (s *sseWriter) event(payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.buf.Reset()
	s.buf.WriteString("data: ")
	s.buf.Write(body)
	s.buf.WriteString("\n\n")
	return s.flush()
}

func (s *sseWriter) done() error {
	s.buf.Reset()
	s.buf.WriteString("data: [DONE]\n\n")
	return s.flush()
}

func (s *sseWriter) flush() error {
	if _, err := s.w.Write(s.buf.Bytes()); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":{"message":"failed to encode response","type":"api_error"}}`,
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Message: message, Type: kind}})
}
