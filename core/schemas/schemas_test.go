package schemas

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func intPtr(i int) *int           { return &i }
func floatPtr(f float64) *float64 { return &f }
func boolPtr(b bool) *bool        { return &b }

// conversation builds a multi-turn exchange that exercises every branch of the
// schema: a system turn, a multimodal user turn, an assistant tool call, the
// tool result, and a final assistant answer.
func conversation() *Request {
	return &Request{
		Model: "gpt-oss-120b",
		Messages: []Message{
			{
				Role:    RoleSystem,
				Content: []Part{TextPart("You are a terse weather assistant.")},
			},
			{
				Role: RoleUser,
				Content: []Part{
					TextPart("What is the weather here?"),
					{
						Type: PartTypeImage,
						Media: &Media{
							MIMEType: "image/png",
							Data:     "aGVsbG8=",
						},
					},
				},
				Name: "chirag",
			},
			{
				Role: RoleAssistant,
				Content: []Part{
					{Type: PartTypeThinking, Text: "The image shows a street sign in Pune.", Signature: "sig-abc"},
				},
				ToolCalls: []ToolCall{
					{
						Index: intPtr(0),
						ID:    "call_1",
						Type:  ToolTypeFunction,
						Function: FunctionCall{
							Name:      "get_weather",
							Arguments: `{"city":"Pune","unit":"c"}`,
						},
					},
				},
			},
			{
				Role:       RoleTool,
				ToolCallID: "call_1",
				Content:    []Part{TextPart(`{"temp_c":31,"sky":"clear"}`)},
			},
			{
				Role:    RoleAssistant,
				Content: []Part{TextPart("31°C and clear.")},
			},
		},
		MaxTokens:   intPtr(512),
		Temperature: floatPtr(0),
		Stop:        []string{"\n\n"},
		Tools: []Tool{
			{
				Type: ToolTypeFunction,
				Function: &FunctionSchema{
					Name:        "get_weather",
					Description: "Look up current weather for a city.",
					Parameters: map[string]json.RawMessage{
						"type":       json.RawMessage(`"object"`),
						"properties": json.RawMessage(`{"city":{"type":"string"},"unit":{"type":"string","enum":["c","f"]}}`),
						"required":   json.RawMessage(`["city"]`),
					},
				},
			},
		},
		ToolChoice: &ToolChoice{Mode: ToolChoiceAuto},
		ResponseFormat: &ResponseFormat{
			Type: ResponseFormatJSONSchema,
			JSONSchema: &JSONSchema{
				Name:        "weather_answer",
				Description: "Structured weather reading.",
				Schema: map[string]json.RawMessage{
					"type":       json.RawMessage(`"object"`),
					"properties": json.RawMessage(`{"sky":{"type":"string"}}`),
					// A numeric keyword: decoded into any this would become a
					// float64 and re-encode as 0.1 rather than the bytes sent.
					"multipleOf": json.RawMessage(`0.10`),
					"required":   json.RawMessage(`["sky"]`),
				},
				Strict: boolPtr(true),
			},
		},
		// Values span the kinds adapters actually put here: a string, a small
		// number, and an integer too large to survive a float64.
		Extra: map[string]json.RawMessage{
			"reasoning_effort": json.RawMessage(`"low"`),
			"top_k":            json.RawMessage(`40`),
			"thinking_budget":  json.RawMessage(`9007199254740993`),
		},
	}
}

func TestRequestRoundTrip(t *testing.T) {
	want := conversation()

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Request
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !reflect.DeepEqual(want, &got) {
		t.Fatalf("round-trip mismatch:\n want %#v\n  got %#v", want, &got)
	}

	// Re-encoding the decoded value must be byte-identical, which catches
	// fields that decode into an equal-but-differently-encoded shape.
	reencoded, err := json.Marshal(&got)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encode mismatch:\n first %s\nsecond %s", encoded, reencoded)
	}
}

func TestRequestRoundTripPreservesToolCallDetail(t *testing.T) {
	var got Request
	encoded, err := json.Marshal(conversation())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if n := len(got.Messages); n != 5 {
		t.Fatalf("messages = %d, want 5", n)
	}

	assistant := got.Messages[2]
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(assistant.ToolCalls))
	}
	call := assistant.ToolCalls[0]
	if call.Index == nil || *call.Index != 0 {
		t.Errorf("tool call index = %v, want 0", call.Index)
	}
	if call.ID != "call_1" || call.Function.Name != "get_weather" {
		t.Errorf("tool call identity = %q/%q", call.ID, call.Function.Name)
	}
	// Arguments must survive as the exact JSON string the model emitted, not
	// as a re-serialised map.
	if call.Function.Arguments != `{"city":"Pune","unit":"c"}` {
		t.Errorf("tool call arguments = %q", call.Function.Arguments)
	}
	if assistant.Content[0].Type != PartTypeThinking || assistant.Content[0].Signature != "sig-abc" {
		t.Errorf("thinking part not preserved: %#v", assistant.Content[0])
	}

	result := got.Messages[3]
	if result.Role != RoleTool || result.ToolCallID != "call_1" {
		t.Errorf("tool result = %q/%q", result.Role, result.ToolCallID)
	}

	user := got.Messages[1]
	if len(user.Content) != 2 || user.Content[1].Media == nil || user.Content[1].Media.Data != "aGVsbG8=" {
		t.Errorf("multimodal user turn not preserved: %#v", user.Content)
	}

	// A zero Temperature must stay set rather than collapsing to unset.
	if got.Temperature == nil || *got.Temperature != 0 {
		t.Errorf("temperature = %v, want explicit 0", got.Temperature)
	}

	// response_format is a hard routing gate, so it must arrive as a typed
	// field rather than something the router has to dig out of Extra.
	rf := got.ResponseFormat
	if rf == nil || rf.Type != ResponseFormatJSONSchema {
		t.Fatalf("response format = %#v, want json_schema", rf)
	}
	if rf.JSONSchema == nil || rf.JSONSchema.Name != "weather_answer" {
		t.Fatalf("json schema = %#v", rf.JSONSchema)
	}
	if rf.JSONSchema.Strict == nil || !*rf.JSONSchema.Strict {
		t.Errorf("strict = %v, want explicit true", rf.JSONSchema.Strict)
	}
	// The schema document is pass-through: every member must come back as the
	// exact bytes it went in as, numeric keywords included.
	if got, want := string(rf.JSONSchema.Schema["multipleOf"]), "0.10"; got != want {
		t.Errorf("schema multipleOf = %s, want %s", got, want)
	}
	if got, want := string(rf.JSONSchema.Schema["properties"]), `{"sky":{"type":"string"}}`; got != want {
		t.Errorf("schema properties = %s, want %s", got, want)
	}

	tool := got.Tools[0]
	if tool.Function == nil {
		t.Fatalf("tool function missing")
	}
	if got, want := string(tool.Function.Parameters["required"]), `["city"]`; got != want {
		t.Errorf("tool parameters required = %s, want %s", got, want)
	}
}

// TestRequestExtraPreservesNumericFidelity pins the policy behind Extra's
// json.RawMessage value type. Decoding into any would widen every number to a
// float64: type switches in adapters expecting an int would miss, and an
// integer above 2^53 would come back changed.
func TestRequestExtraPreservesNumericFidelity(t *testing.T) {
	want := &Request{
		Model: "gpt-oss-120b",
		Extra: map[string]json.RawMessage{
			"top_k":            json.RawMessage(`40`),
			"thinking_budget":  json.RawMessage(`9007199254740993`),
			"temperature_hint": json.RawMessage(`0.10`),
		},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Request
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for k, w := range want.Extra {
		if g := string(got.Extra[k]); g != string(w) {
			t.Errorf("Extra[%q] = %s, want %s", k, g, w)
		}
	}

	// The same values through map[string]any is what this type choice avoids;
	// assert the failure mode explicitly so the rationale cannot silently rot.
	var loose struct {
		Extra map[string]any `json:"extra"`
	}
	if err := json.Unmarshal(encoded, &loose); err != nil {
		t.Fatalf("unmarshal loose: %v", err)
	}
	if v, ok := loose.Extra["thinking_budget"].(float64); !ok || int64(v) == 9007199254740993 {
		t.Errorf("expected map[string]any to widen and lose precision, got %#v", loose.Extra["thinking_budget"])
	}
}

func TestResponseRoundTrip(t *testing.T) {
	want := &Response{
		ID:      "resp_1",
		Created: 1757116800,
		Model:   "gpt-oss-120b",
		Choices: []Choice{
			{
				Index: 0,
				Message: Message{
					Role: RoleAssistant,
					ToolCalls: []ToolCall{
						{ID: "call_1", Type: ToolTypeFunction, Function: FunctionCall{
							Name:      "get_weather",
							Arguments: `{"city":"Pune"}`,
						}},
					},
				},
				FinishReason: FinishToolCalls,
			},
		},
		Usage: Usage{
			PromptTokens:       120,
			CompletionTokens:   30,
			TotalTokens:        150,
			CachedPromptTokens: 64,
			ReasoningTokens:    12,
		},
		Provider: "mocker",
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Response
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(want, &got) {
		t.Fatalf("round-trip mismatch:\n want %#v\n  got %#v", want, &got)
	}
}

func TestChunkRawIsNotEncoded(t *testing.T) {
	c := &Chunk{
		ID:    "chunk_1",
		Model: "gpt-oss-120b",
		Choices: []ChunkChoice{
			{Index: 0, Delta: Delta{Role: RoleAssistant, Content: []Part{TextPart("31")}}},
		},
		Usage: &Usage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150},
		Raw:   []byte(`{"upstream":"passthrough"}`),
	}

	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(encoded, []byte("passthrough")) {
		t.Fatalf("Raw leaked into encoded chunk: %s", encoded)
	}

	var got Chunk
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Raw != nil {
		t.Errorf("Raw = %q, want nil", got.Raw)
	}
	got.Raw = c.Raw
	if !reflect.DeepEqual(c, &got) {
		t.Fatalf("round-trip mismatch:\n want %#v\n  got %#v", c, &got)
	}
}

func TestMessageText(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{"empty", Message{Role: RoleUser}, ""},
		{"single text part", Message{Content: []Part{TextPart("hello")}}, "hello"},
		{
			"skips non-text parts",
			Message{Content: []Part{
				TextPart("look: "),
				{Type: PartTypeImage, Media: &Media{URL: "https://example.test/a.png"}},
				TextPart("a sign"),
			}},
			"look: a sign",
		},
		{
			"includes thinking parts only as text when typed text",
			Message{Content: []Part{{Type: PartTypeThinking, Text: "hmm"}, TextPart("answer")}},
			"answer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.msg.Text(); got != tt.want {
				t.Errorf("Text() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUsageAdd(t *testing.T) {
	u := Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11, CachedPromptTokens: 4}
	u.Add(Usage{CompletionTokens: 5, TotalTokens: 5, ReasoningTokens: 3, CacheWriteTokens: 2})

	want := Usage{
		PromptTokens:       10,
		CompletionTokens:   6,
		TotalTokens:        16,
		CachedPromptTokens: 4,
		CacheWriteTokens:   2,
		ReasoningTokens:    3,
	}
	if u != want {
		t.Errorf("Add() = %#v, want %#v", u, want)
	}
}
