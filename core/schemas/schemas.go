// Package schemas defines the canonical, provider-neutral types that flow
// through the gateway. Every provider adapter translates its native wire
// format into these types on the way in and back out on the way out; no raw
// provider maps are allowed past the adapter boundary.
//
// This package is transport-free by construction: it must never import
// net/http, fasthttp, or any other transport package.
package schemas

// Role identifies the author of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType discriminates the payload carried by a Part.
type PartType string

const (
	PartTypeText     PartType = "text"
	PartTypeImage    PartType = "image"
	PartTypeAudio    PartType = "audio"
	PartTypeThinking PartType = "thinking"
)

// Part is one element of a Message's content. Content is modelled as a slice
// of Parts rather than a bare string so multimodal messages (and interleaved
// reasoning) round-trip without a lossy conversion.
type Part struct {
	Type PartType `json:"type"`

	// Text is set for PartTypeText and PartTypeThinking.
	Text string `json:"text,omitempty"`

	// Media is set for PartTypeImage and PartTypeAudio.
	Media *Media `json:"media,omitempty"`

	// Signature carries a provider-issued attestation over a thinking part
	// (Anthropic returns one and requires it echoed back on the next turn).
	Signature string `json:"signature,omitempty"`
}

// Media is a binary payload referenced either by URL or inline as base64.
// Exactly one of URL and Data is expected to be set.
type Media struct {
	MIMEType string `json:"mime_type,omitempty"`
	URL      string `json:"url,omitempty"`
	Data     string `json:"data,omitempty"` // base64, no data: prefix
}

// TextPart is a convenience constructor for the common case.
func TextPart(text string) Part {
	return Part{Type: PartTypeText, Text: text}
}

// Message is one turn of a conversation.
type Message struct {
	Role    Role   `json:"role"`
	Content []Part `json:"content,omitempty"`

	// Name optionally disambiguates multiple participants sharing a Role.
	Name string `json:"name,omitempty"`

	// ToolCalls is set on assistant messages that invoke tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID is set on RoleTool messages and refers to the ToolCall.ID
	// this message is the result of.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// Text flattens the message's text parts into a single string. Non-text parts
// are skipped.
func (m Message) Text() string {
	// Fast path: the overwhelmingly common single-text-part message.
	if len(m.Content) == 1 && m.Content[0].Type == PartTypeText {
		return m.Content[0].Text
	}
	var out []byte
	for _, p := range m.Content {
		if p.Type == PartTypeText {
			out = append(out, p.Text...)
		}
	}
	return string(out)
}

// ToolCall is a model-requested invocation of a tool.
type ToolCall struct {
	// Index is the position of this call within the assistant turn. Streaming
	// upstreams identify tool-call deltas by index rather than by ID, so it is
	// a pointer to distinguish "index 0" from "not supplied".
	Index *int `json:"index,omitempty"`

	ID       string       `json:"id,omitempty"`
	Type     ToolType     `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names a function and carries its arguments as the raw JSON
// string the model emitted. It is kept as a string because streaming providers
// deliver it in fragments that are only valid JSON once concatenated.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolType discriminates kinds of callable tools.
type ToolType string

const ToolTypeFunction ToolType = "function"

// Tool is a tool definition offered to the model.
type Tool struct {
	Type     ToolType        `json:"type"`
	Function *FunctionSchema `json:"function,omitempty"`
}

// FunctionSchema describes a callable function. Parameters holds a JSON Schema
// document; it stays untyped because providers pass it through verbatim.
type FunctionSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ToolChoiceMode constrains whether and how the model may call tools.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceFunction forces the specific tool named by ToolChoice.Name.
	ToolChoiceFunction ToolChoiceMode = "function"
)

// ToolChoice expresses the caller's tool-use constraint.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"`
}

// Request is a canonical completion request. Optional generation parameters are
// pointers so an adapter can tell "unset" from "explicitly zero" — the two mean
// different things to most providers.
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	MaxTokens        *int     `json:"max_tokens,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	Stop             []string `json:"stop,omitempty"`

	Stream bool `json:"stream,omitempty"`

	Tools      []Tool      `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	// User is an opaque end-user identifier forwarded to providers that accept
	// one for abuse tracking.
	User string `json:"user,omitempty"`

	// Extra is the escape hatch for provider-specific parameters that have no
	// canonical equivalent. Adapters merge recognised keys into their native
	// payload and ignore the rest.
	Extra map[string]any `json:"extra,omitempty"`
}

// FinishReason is the canonical vocabulary every provider's completion reason
// is mapped onto.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishError         FinishReason = "error"
	FinishUnknown       FinishReason = "unknown"
)

// Usage is the token accounting for one request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// CachedPromptTokens is the subset of PromptTokens served from the
	// provider's prompt cache; it is billed at a different rate.
	CachedPromptTokens int `json:"cached_prompt_tokens,omitempty"`
	// CacheWriteTokens is the subset of PromptTokens written into the cache.
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	// ReasoningTokens is the subset of CompletionTokens spent on hidden
	// reasoning.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// Add accumulates other into u. Streaming upstreams may report usage
// incrementally or only in a final chunk; both cases fold through here.
func (u *Usage) Add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.CachedPromptTokens += other.CachedPromptTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.ReasoningTokens += other.ReasoningTokens
}

// Choice is one completion alternative.
type Choice struct {
	Index        int          `json:"index"`
	Message      Message      `json:"message"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}

// Response is a canonical non-streaming completion response.
type Response struct {
	ID      string   `json:"id,omitempty"`
	Created int64    `json:"created,omitempty"` // unix seconds
	Model   string   `json:"model,omitempty"`   // model that actually served
	Choices []Choice `json:"choices,omitempty"`
	Usage   Usage    `json:"usage"`

	// Provider names the adapter that produced this response. It is set by the
	// gateway, not by the caller.
	Provider string `json:"provider,omitempty"`
}

// Delta is the incremental content carried by a streaming Chunk.
type Delta struct {
	Role      Role       `json:"role,omitempty"`
	Content   []Part     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ChunkChoice is one completion alternative's slice of a streaming update.
type ChunkChoice struct {
	Index        int          `json:"index"`
	Delta        Delta        `json:"delta"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}

// Chunk is one canonical streaming update.
type Chunk struct {
	ID      string        `json:"id,omitempty"`
	Created int64         `json:"created,omitempty"`
	Model   string        `json:"model,omitempty"`
	Choices []ChunkChoice `json:"choices,omitempty"`

	// Usage is set only on chunks that carry token accounting, typically the
	// final one before the stream terminates.
	Usage *Usage `json:"usage,omitempty"`

	Provider string `json:"provider,omitempty"`

	// Raw is the untouched upstream SSE payload for this chunk. When the
	// inbound and outbound wire formats match, the transport may forward Raw
	// verbatim instead of re-encoding the parsed fields. It is excluded from
	// JSON so it never doubles up in an encoded chunk.
	Raw []byte `json:"-"`
}
