package openai

import "encoding/json"

// Responses API wire types. Used only when a route's API flavor is
// "responses" (OpenAI /v1/responses). Chat Completions types stay in
// types.go and remain the default for every OpenAI-compatible upstream.

type ResponsesRequest struct {
	Model             string               `json:"model"`
	Input             []ResponsesInputItem `json:"input"`
	Instructions      string               `json:"instructions,omitempty"`
	MaxOutputTokens   int                  `json:"max_output_tokens,omitempty"`
	Temperature       *float64             `json:"temperature,omitempty"`
	TopP              *float64             `json:"top_p,omitempty"`
	Stream            bool                 `json:"stream,omitempty"`
	Tools             []ResponsesTool      `json:"tools,omitempty"`
	ToolChoice        any                  `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                `json:"parallel_tool_calls,omitempty"`
	Reasoning         *ResponsesReasoning  `json:"reasoning,omitempty"`
	// Store is a pointer so we can send false (omitempty would drop it).
	// Always false: we don't use previous_response_id or encrypted
	// reasoning round-trip, so there's nothing to persist server-side.
	Store *bool `json:"store,omitempty"`
}

type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"` // auto | concise | detailed
}

type ResponsesTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ResponsesInputItem is a tagged union. Type selects the variant:
//   - "message" (or empty with Role set): conversational turn
//   - "function_call": prior assistant tool call
//   - "function_call_output": tool result
type ResponsesInputItem struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"` // string or []ResponsesContentPart
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type ResponsesContentPart struct {
	Type     string `json:"type"` // input_text | output_text | input_image
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type ResponsesResponse struct {
	ID                string                `json:"id"`
	Status            string                `json:"status"` // completed | incomplete | failed
	Output            []ResponsesOutputItem `json:"output"`
	Usage             *ResponsesUsage       `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *ResponsesError `json:"error"`
}

type ResponsesError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}

type ResponsesOutputItem struct {
	Type      string                   `json:"type"` // message | function_call | reasoning
	ID        string                   `json:"id"`
	Status    string                   `json:"status,omitempty"`
	CallID    string                   `json:"call_id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Arguments string                   `json:"arguments,omitempty"`
	Role      string                   `json:"role,omitempty"`
	Content   []ResponsesOutputContent `json:"content,omitempty"`
	Summary   []ResponsesSummaryText   `json:"summary,omitempty"`
}

type ResponsesOutputContent struct {
	Type    string `json:"type"` // output_text | refusal
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

type ResponsesSummaryText struct {
	Type string `json:"type"` // summary_text
	Text string `json:"text,omitempty"`
}

type ResponsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

// ResponsesStreamEvent is the union of /v1/responses SSE payloads.
// Type matches the `event:` line (and the JSON "type" field). Only the
// fields for the given event are populated.
type ResponsesStreamEvent struct {
	Type        string               `json:"type"`
	Delta       string               `json:"delta"`
	ItemID      string               `json:"item_id"`
	OutputIndex int                  `json:"output_index"`
	Item        *ResponsesOutputItem `json:"item"`
	Response    *ResponsesResponse   `json:"response"`
	Message     string               `json:"message"`
	Code        string               `json:"code"`
	Error       *ResponsesError      `json:"error"`
}
