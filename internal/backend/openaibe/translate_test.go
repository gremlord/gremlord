package openaibe

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/openai"
)

func route(maxTokensParam, reasoning string) config.Resolved {
	return config.Resolved{
		Alias:        "gpt",
		ProviderName: "openai",
		Provider:     config.Provider{Type: "openai", BaseURL: "https://api.openai.com/v1", MaxTokensParam: maxTokensParam},
		Model:        config.Model{ID: "gpt-5.2", Reasoning: reasoning},
	}
}

func parseReq(t *testing.T, body string) *anthropic.MessagesRequest {
	t.Helper()
	req, err := anthropic.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRequestTranslation(t *testing.T) {
	body := `{
	  "model": "gpt", "max_tokens": 4096, "stream": true,
	  "temperature": 0.7, "top_k": 40,
	  "stop_sequences": ["a","b","c","d","e"],
	  "system": [{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}],
	  "tools": [
	    {"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
	    {"type":"web_search_20260209","name":"web_search"}
	  ],
	  "tool_choice": {"type":"auto","disable_parallel_tool_use":true},
	  "messages": [
	    {"role":"user","content":"read main.go"},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"I should read it"},
	      {"type":"text","text":"Reading."},
	      {"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"main.go"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"package main"}]},
	      {"type":"text","text":"now explain"}
	    ]}
	  ]
	}`
	out, err := TranslateRequest(parseReq(t, body), route("", "none"))
	if err != nil {
		t.Fatal(err)
	}

	// system → leading system message
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "You are helpful." {
		t.Errorf("system message: %+v", out.Messages[0])
	}
	// tool ordering: assistant tool_calls, then role:"tool" BEFORE trailing user text
	roles := []string{}
	for _, m := range out.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"system", "user", "assistant", "tool", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	asst := out.Messages[2]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "toolu_1" || asst.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("assistant tool_calls: %+v", asst.ToolCalls)
	}
	if asst.Content != "Reading." {
		t.Errorf("thinking should be dropped, text kept: %v", asst.Content)
	}
	if out.Messages[3].ToolCallID != "toolu_1" || out.Messages[3].Content != "package main" {
		t.Errorf("tool message: %+v", out.Messages[3])
	}

	// server tool stripped, user tool kept with schema intact
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "read_file" {
		t.Fatalf("tools: %+v", out.Tools)
	}
	var schema map[string]any
	json.Unmarshal(out.Tools[0].Function.Parameters, &schema)
	if schema["type"] != "object" {
		t.Error("input_schema not passed through")
	}

	if out.ToolChoice != "auto" || out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Errorf("tool_choice: %v parallel: %v", out.ToolChoice, out.ParallelToolCalls)
	}
	if out.MaxTokens != 4096 || out.MaxCompletionTokens != 0 {
		t.Errorf("max_tokens mapping: %d/%d", out.MaxTokens, out.MaxCompletionTokens)
	}
	if out.Temperature == nil || *out.Temperature != 0.7 {
		t.Error("temperature not passed")
	}
	if len(out.Stop) != 4 {
		t.Errorf("stop truncation: %d", len(out.Stop))
	}
	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage missing")
	}
}

func TestMidConversationSystemMessage(t *testing.T) {
	// Claude Code sends {"role":"system"} entries inside messages[].
	body := `{"model":"gpt","max_tokens":10,"messages":[
	  {"role":"user","content":"hi"},
	  {"role":"system","content":"Terse mode enabled."}
	]}`
	out, err := TranslateRequest(parseReq(t, body), route("", ""))
	if err != nil {
		t.Fatal(err)
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Role != "system" || last.Content != "Terse mode enabled." {
		t.Errorf("mid-conversation system: %+v", last)
	}
}

func TestReasoningModes(t *testing.T) {
	body := `{"model":"gpt","max_tokens":100,"temperature":0.5,
	  "thinking":{"type":"enabled","budget_tokens":16000},
	  "messages":[{"role":"user","content":"hi"}]}`

	// effort: budget → reasoning_effort, sampling params dropped
	out, _ := TranslateRequest(parseReq(t, body), route("max_completion_tokens", "effort"))
	if out.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q", out.ReasoningEffort)
	}
	if out.Temperature != nil {
		t.Error("temperature must be dropped for reasoning models")
	}
	if out.MaxCompletionTokens != 100 || out.MaxTokens != 0 {
		t.Errorf("max_completion_tokens mapping: %d/%d", out.MaxTokens, out.MaxCompletionTokens)
	}

	// passive: thinking stripped, sampling kept
	out, _ = TranslateRequest(parseReq(t, body), route("", "passive"))
	if out.ReasoningEffort != "" {
		t.Error("passive must not set reasoning_effort")
	}
	if out.Temperature == nil {
		t.Error("passive keeps sampling params")
	}

	// none: reasoning_effort explicitly "none" (GPT-5-class models reject
	// function tools otherwise), sampling kept
	out, _ = TranslateRequest(parseReq(t, body), route("", "none"))
	if out.ReasoningEffort != "none" {
		t.Errorf("none must explicitly set reasoning_effort=\"none\", got %q", out.ReasoningEffort)
	}
	if out.Temperature == nil {
		t.Error("none keeps sampling params")
	}
}

func TestImageTranslation(t *testing.T) {
	body := `{"model":"gpt","max_tokens":10,"messages":[
	  {"role":"user","content":[
	    {"type":"text","text":"what is this"},
	    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}
	  ]}]}`
	out, _ := TranslateRequest(parseReq(t, body), route("", ""))
	parts, ok := out.Messages[0].Content.([]openai.ContentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("content: %+v", out.Messages[0].Content)
	}
	if parts[1].ImageURL.URL != "data:image/png;base64,iVBOR" {
		t.Errorf("image url: %s", parts[1].ImageURL.URL)
	}
}

func TestResponseTranslation(t *testing.T) {
	resp := &openai.ChatResponse{
		ID: "chatcmpl-abc",
		Choices: []openai.Choice{{
			Message: openai.ResponseMessage{
				Role: "assistant", Content: "done",
				ReasoningContent: "let me think",
				ToolCalls: []openai.ToolCall{{
					ID: "call_1", Type: "function",
					Function: openai.FunctionCall{Name: "edit", Arguments: `{"path":"a.go"`}, // truncated JSON
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: &openai.Usage{PromptTokens: 100, CompletionTokens: 20,
			PromptTokensDetails: &struct {
				CachedTokens int64 `json:"cached_tokens"`
			}{CachedTokens: 60}},
	}
	out, err := TranslateResponse(resp, "gpt")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "gpt" || out.StopReason != "tool_use" {
		t.Errorf("model=%s stop=%s", out.Model, out.StopReason)
	}
	if out.Content[0].Type != "thinking" || out.Content[1].Type != "text" || out.Content[2].Type != "tool_use" {
		t.Fatalf("block order: %+v", out.Content)
	}
	// Translated reasoning is display-only: ContentBlock intentionally has no
	// signature field, so native Anthropic replay can identify and strip it.
	encoded, err := json.Marshal(out.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"signature"`) {
		t.Errorf("translated thinking unexpectedly has a signature: %s", encoded)
	}
	// truncated arguments repaired
	var input map[string]any
	if err := json.Unmarshal(out.Content[2].Input, &input); err != nil || input["path"] != "a.go" {
		t.Errorf("repaired input: %s (%v)", out.Content[2].Input, err)
	}
	// Anthropic invariant: input_tokens excludes cache reads
	if out.Usage.InputTokens != 40 || out.Usage.CacheReadInputTokens != 60 {
		t.Errorf("usage: %+v", out.Usage)
	}
}

func TestRepairJSON(t *testing.T) {
	cases := map[string]bool{
		`{"a":1}`:            true,
		``:                   true, // empty → {}
		`{"a":"unclosed`:     true, // close string + brace
		`{"a":[1,2`:          true,
		`not json at all {{`: false,
	}
	for in, ok := range cases {
		_, err := repairJSON(in)
		if (err == nil) != ok {
			t.Errorf("repairJSON(%q): err=%v, want ok=%v", in, err, ok)
		}
	}
}

// Anthropic's API validates tool_use.id against ^[A-Za-z0-9_-]+$. vLLM and
// some other OpenAI-compatible backends emit ids like "Bash:0" — passed
// through unsanitized, Claude Code persists that into its transcript, and
// every later turn that resends the history to a real Anthropic model 400s
// on invalid_request_error until the transcript is hand-edited.
func TestToolUseIDSanitizesInvalidCharacters(t *testing.T) {
	cases := map[string]string{
		"Bash:0":                      "Bash_0",
		"TaskOutput:12":               "TaskOutput_12",
		"call_a":                      "call_a",
		"toolu_1":                     "toolu_1",
		"":                            "toolu_gremlord_missing",
		"functions.read_file:0":       "functions_read_file_0",
		"mcp__clauder__get_context:0": "mcp__clauder__get_context_0",
	}
	for in, want := range cases {
		if got := toolUseID(in); got != want {
			t.Errorf("toolUseID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResponseTranslationSanitizesToolCallID(t *testing.T) {
	resp := &openai.ChatResponse{
		ID: "chatcmpl-abc",
		Choices: []openai.Choice{{
			Message: openai.ResponseMessage{
				Role: "assistant",
				ToolCalls: []openai.ToolCall{{
					ID: "Bash:0", Type: "function",
					Function: openai.FunctionCall{Name: "Bash", Arguments: `{"command":"ls"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	out, err := TranslateResponse(resp, "gpt")
	if err != nil {
		t.Fatal(err)
	}
	if out.Content[0].ID != "Bash_0" {
		t.Errorf("tool_use.id = %q, want sanitized id without colon", out.Content[0].ID)
	}
}

// OpenAI rejects messages[].tool_calls arrays longer than 128 ("array too
// long"). Claude Code can still emit a single assistant turn with more
// tool_use blocks than that; translateMessage must split it across several
// assistant messages rather than send one oversized array.
func TestAssistantMessageWithManyToolCallsIsSplit(t *testing.T) {
	const n = 152
	blocks := []anthropic.ContentBlock{{Type: "text", Text: "working"}}
	for i := 0; i < n; i++ {
		blocks = append(blocks, anthropic.ContentBlock{
			Type: "tool_use", ID: fmt.Sprintf("toolu_%d", i), Name: "read_file",
			Input: json.RawMessage(`{"path":"a.go"}`),
		})
	}
	msgs, err := translateMessage(anthropic.Message{Role: "assistant", Content: blocks})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("split messages = %d, want 2", len(msgs))
	}
	if len(msgs[0].ToolCalls) != maxToolCallsPerMessage {
		t.Errorf("first chunk tool_calls = %d, want %d", len(msgs[0].ToolCalls), maxToolCallsPerMessage)
	}
	if len(msgs[1].ToolCalls) != n-maxToolCallsPerMessage {
		t.Errorf("second chunk tool_calls = %d, want %d", len(msgs[1].ToolCalls), n-maxToolCallsPerMessage)
	}
	for _, m := range msgs {
		if m.Role != "assistant" {
			t.Errorf("chunk role = %q, want assistant", m.Role)
		}
	}
	if msgs[0].Content != "working" {
		t.Errorf("text should land on first chunk, got %v / %v", msgs[0].Content, msgs[1].Content)
	}
	if msgs[1].Content != nil {
		t.Errorf("second chunk should carry no text, got %v", msgs[1].Content)
	}
	// order and ids preserved across the split
	if msgs[0].ToolCalls[0].ID != "toolu_0" || msgs[1].ToolCalls[len(msgs[1].ToolCalls)-1].ID != fmt.Sprintf("toolu_%d", n-1) {
		t.Errorf("tool_call ids out of order across split: %+v / %+v", msgs[0].ToolCalls[0], msgs[1].ToolCalls[len(msgs[1].ToolCalls)-1])
	}
}

// The real wire shape of a ToolSearch turn under deferred tool loading: the
// client's own tool_result carries nothing but tool_reference blocks. Left
// unrendered these produce a role:"tool" message with empty content, which
// OpenAI-dialect servers (vLLM especially) reject.
func TestToolReferenceResultIsNotEmpty(t *testing.T) {
	body := `{
	  "model": "gpt", "max_tokens": 4096,
	  "tools": [{"name":"ToolSearch","description":"Load deferred tool schemas","input_schema":{"type":"object"}}],
	  "messages": [
	    {"role":"user","content":"fetch example.com"},
	    {"role":"assistant","content":[
	      {"type":"tool_use","id":"toolu_probe1","name":"ToolSearch","input":{"query":"select:WebFetch"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_probe1","content":[{"type":"tool_reference","tool_name":"WebFetch"}]}
	    ]}
	  ]
	}`
	out, err := TranslateRequest(parseReq(t, body), route("", "none"))
	if err != nil {
		t.Fatal(err)
	}
	var tool *openai.ChatMessage
	for i := range out.Messages {
		if out.Messages[i].Role == "tool" {
			tool = &out.Messages[i]
		}
	}
	if tool == nil {
		t.Fatal("no tool message produced for the ToolSearch result")
	}
	content, ok := tool.Content.(string)
	if !ok || strings.TrimSpace(content) == "" {
		t.Fatalf("tool message content = %#v, want non-empty text naming the loaded tool", tool.Content)
	}
	if !strings.Contains(content, "WebFetch") {
		t.Errorf("tool message content = %q, want the referenced tool named", content)
	}
}

// A tool can legitimately produce nothing — a Bash command with no stdout
// arrives as content:"" — and ChatMessage.Content is `any` with omitempty,
// so passing that through drops the key and sends a tool message with no
// content at all. OpenAI-dialect servers reject that.
func TestEmptyToolResultStillHasContent(t *testing.T) {
	cases := map[string]string{
		"empty string":  `""`,
		"empty array":   `[]`,
		"unknown block": `[{"type":"future_block_type"}]`,
	}
	for name, content := range cases {
		body := fmt.Sprintf(`{
		  "model": "gpt", "max_tokens": 4096,
		  "messages": [
		    {"role":"user","content":"run it"},
		    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]},
		    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":%s}]}
		  ]
		}`, content)
		out, err := TranslateRequest(parseReq(t, body), route("", "none"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var tool *openai.ChatMessage
		for i := range out.Messages {
			if out.Messages[i].Role == "tool" {
				tool = &out.Messages[i]
			}
		}
		if tool == nil {
			t.Fatalf("%s: no tool message produced", name)
		}
		// Marshal the way the request goes out: omitempty is the whole point.
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"content"`) {
			t.Errorf("%s: tool message marshalled without a content key: %s", name, raw)
		}
	}
}
