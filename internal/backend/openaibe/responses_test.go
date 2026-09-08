package openaibe

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/openai"
)

func responsesRoute(reasoning string) config.Resolved {
	return config.Resolved{
		Alias:        "gpt-6-astra",
		ProviderName: "openai",
		Provider:     config.Provider{Type: "openai", BaseURL: "https://api.openai.com/v1", API: config.APIResponses},
		Model:        config.Model{ID: "gpt-6-astra", Reasoning: reasoning, API: config.APIResponses, MaxOutput: 128000},
	}
}

func TestResponsesRequestTranslation(t *testing.T) {
	body := `{
	  "model": "gpt-6-astra", "max_tokens": 4096, "stream": true,
	  "temperature": 0.7,
	  "thinking": {"type":"enabled","budget_tokens":16000},
	  "system": [{"type":"text","text":"You are helpful."}],
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
	out, err := TranslateResponsesRequest(parseReq(t, body), responsesRoute("effort"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Instructions != "You are helpful." {
		t.Errorf("instructions = %q", out.Instructions)
	}
	if out.Store == nil || *out.Store {
		t.Error("store must be false")
	}
	if out.MaxOutputTokens != 4096 {
		t.Errorf("max_output_tokens = %d", out.MaxOutputTokens)
	}
	if out.Reasoning == nil || out.Reasoning.Effort != "high" || out.Reasoning.Summary != "auto" {
		t.Errorf("reasoning = %+v", out.Reasoning)
	}
	if out.Temperature != nil {
		t.Error("temperature must be dropped for effort reasoning")
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "read_file" || out.Tools[0].Type != "function" {
		t.Fatalf("tools: %+v", out.Tools)
	}
	if out.ToolChoice != "auto" || out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Errorf("tool_choice=%v parallel=%v, want auto/false", out.ToolChoice, out.ParallelToolCalls)
	}

	types := []string{}
	for _, item := range out.Input {
		types = append(types, item.Type)
	}
	want := []string{"message", "message", "function_call", "function_call_output", "message"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("input types = %v, want %v", types, want)
	}
	if out.Input[2].CallID != "toolu_1" || out.Input[2].Name != "read_file" {
		t.Errorf("function_call: %+v", out.Input[2])
	}
	if out.Input[3].CallID != "toolu_1" || out.Input[3].Output != "package main" {
		t.Errorf("function_call_output: %+v", out.Input[3])
	}
}

func TestResponsesEmptyToolResultIsNotEmpty(t *testing.T) {
	body := `{"model":"gpt-6-astra","max_tokens":16,"messages":[
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[]}]}
	]}`
	out, err := TranslateResponsesRequest(parseReq(t, body), responsesRoute("effort"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Input) != 1 || out.Input[0].Output != "(no output)" {
		t.Errorf("empty tool result: %+v", out.Input)
	}
}

func TestResponsesImageTranslation(t *testing.T) {
	body := `{"model":"gpt-6-astra","max_tokens":10,"messages":[
	  {"role":"user","content":[
	    {"type":"text","text":"what is this"},
	    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}
	  ]}]}`
	out, err := TranslateResponsesRequest(parseReq(t, body), responsesRoute(""))
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := out.Input[0].Content.([]openai.ResponsesContentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("content: %+v", out.Input[0].Content)
	}
	if parts[1].Type != "input_image" || parts[1].ImageURL != "data:image/png;base64,iVBOR" {
		t.Errorf("image: %+v", parts[1])
	}
}

func TestResponsesResponseTranslation(t *testing.T) {
	resp := &openai.ResponsesResponse{
		ID:     "resp_abc",
		Status: "completed",
		Output: []openai.ResponsesOutputItem{
			{Type: "reasoning", Summary: []openai.ResponsesSummaryText{{Type: "summary_text", Text: "let me think"}}},
			{Type: "message", Content: []openai.ResponsesOutputContent{{Type: "output_text", Text: "done"}}},
			{Type: "function_call", CallID: "Bash:0", Name: "Bash", Arguments: `{"command":"ls"`},
		},
		Usage: &openai.ResponsesUsage{InputTokens: 100, OutputTokens: 20,
			InputTokensDetails: &struct {
				CachedTokens int64 `json:"cached_tokens"`
			}{CachedTokens: 60}},
	}
	out, err := TranslateResponsesResponse(resp, "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "gpt-6-astra" || out.StopReason != "tool_use" {
		t.Errorf("model=%s stop=%s", out.Model, out.StopReason)
	}
	if out.Content[0].Type != "thinking" || out.Content[1].Type != "text" || out.Content[2].Type != "tool_use" {
		t.Fatalf("block order: %+v", out.Content)
	}
	if out.Content[2].ID != "Bash_0" {
		t.Errorf("tool_use.id = %q, want sanitized", out.Content[2].ID)
	}
	var input map[string]any
	if err := json.Unmarshal(out.Content[2].Input, &input); err != nil || input["command"] != "ls" {
		t.Errorf("repaired input: %s (%v)", out.Content[2].Input, err)
	}
	if out.Usage.InputTokens != 40 || out.Usage.CacheReadInputTokens != 60 {
		t.Errorf("usage: %+v", out.Usage)
	}
}

func TestResponsesIncompleteIsMaxTokens(t *testing.T) {
	resp := &openai.ResponsesResponse{
		ID:     "resp_x",
		Status: "incomplete",
		IncompleteDetails: &struct {
			Reason string `json:"reason"`
		}{Reason: "max_output_tokens"},
		Output: []openai.ResponsesOutputItem{
			{Type: "message", Content: []openai.ResponsesOutputContent{{Type: "output_text", Text: "partial"}}},
		},
	}
	out, err := TranslateResponsesResponse(resp, "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != "max_tokens" {
		t.Errorf("stop = %s, want max_tokens", out.StopReason)
	}
}

func runResponsesStream(t *testing.T, events []string) []event {
	t.Helper()
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt-6-astra")
	body := ""
	for _, e := range events {
		body += "data: " + e + "\n\n"
	}
	usage, errType := state.RunResponses(context.Background(), strings.NewReader(body))
	_ = usage
	if errType != "" {
		t.Fatalf("stream errType=%q\n%s", errType, rec.Body.String())
	}
	return parseEvents(t, rec.Body.String())
}

func TestResponsesStreamTextAndTools(t *testing.T) {
	evs := runResponsesStream(t, []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking hard"}`,
		`{"type":"response.output_item.done"}`,
		`{"type":"response.output_item.added","item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","delta":"Let me check."}`,
		`{"type":"response.output_item.done"}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_a","name":"read_file"}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"path\":\"a.go\"}"}`,
		`{"type":"response.output_item.done"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call"}],"usage":{"input_tokens":50,"output_tokens":30,"input_tokens_details":{"cached_tokens":20}}}}`,
	})
	seen := []string{}
	for _, e := range evs {
		switch e.name {
		case "content_block_start":
			seen = append(seen, e.data["content_block"].(map[string]any)["type"].(string))
		case "content_block_delta":
			seen = append(seen, e.data["delta"].(map[string]any)["type"].(string))
		}
	}
	want := "thinking thinking_delta signature_delta text text_delta tool_use input_json_delta"
	if strings.Join(seen, " ") != want {
		t.Errorf("block sequence:\n got %s\nwant %s", strings.Join(seen, " "), want)
	}
	last := evs[len(evs)-2]
	if last.data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", last.data["delta"])
	}
	usage := last.data["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 30 || usage["cache_read_input_tokens"].(float64) != 20 {
		t.Errorf("usage: %v", usage)
	}
}

func TestResponsesStreamFailedIsError(t *testing.T) {
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt-6-astra")
	body := "data: " + `{"type":"response.failed","response":{"error":{"message":"boom"}}}` + "\n\n"
	_, errType := state.RunResponses(context.Background(), strings.NewReader(body))
	if errType != "api_error" {
		t.Errorf("errType = %q, want api_error", errType)
	}
	if !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("expected failed error event, got:\n%s", rec.Body.String())
	}
}

func TestResponsesStreamErrorEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt-6-astra")
	body := "data: " + `{"type":"error","message":"rate limited"}` + "\n\n"
	_, errType := state.RunResponses(context.Background(), strings.NewReader(body))
	if errType != "api_error" {
		t.Errorf("errType = %q, want api_error", errType)
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("expected error event, got:\n%s", rec.Body.String())
	}
}

func TestResponsesStreamUnknownEventsIgnored(t *testing.T) {
	evs := runResponsesStream(t, []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.future_event","whatever":true}`,
		`{"type":"response.output_item.added","item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":4,"output_tokens":1}}}`,
	})
	if !strings.Contains(names(evs), "content_block_delta") {
		t.Errorf("expected text after unknown event, got %s", names(evs))
	}
}

func TestResponsesStreamToolReconcilesOnDone(t *testing.T) {
	evs := runResponsesStream(t, []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"path\":\"a.go\"}"}`,
		`{"type":"response.output_item.added","item":{"type":"function_call"}}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_a","name":"read_file"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call"}],"usage":{"input_tokens":8,"output_tokens":4}}}`,
	})
	var tools []map[string]any
	var args string
	for _, e := range evs {
		if e.name == "content_block_start" {
			cb := e.data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				tools = append(tools, cb)
			}
		}
		if e.name == "content_block_delta" {
			d := e.data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				args += d["partial_json"].(string)
			}
		}
	}
	if len(tools) != 1 {
		t.Fatalf("tool blocks = %d, want 1: %s", len(tools), names(evs))
	}
	if tools[0]["id"] != "call_a" || tools[0]["name"] != "read_file" {
		t.Errorf("tool = %v", tools[0])
	}
	if args != `{"path":"a.go"}` {
		t.Errorf("args = %q", args)
	}
}

func TestResponsesStreamCompletedOnlyHasMessageStart(t *testing.T) {
	evs := runResponsesStream(t, []string{
		`{"type":"response.completed","response":{"id":"resp_empty","status":"completed","usage":{"input_tokens":3,"output_tokens":0}}}`,
	})
	if evs[0].name != "message_start" {
		t.Fatalf("first event = %s, want message_start: %s", evs[0].name, names(evs))
	}
}

func TestResponsesRequestDisablesParallelTools(t *testing.T) {
	body := `{"model":"gpt-6-astra","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	out, err := TranslateResponsesRequest(parseReq(t, body), responsesRoute("effort"))
	if err != nil {
		t.Fatal(err)
	}
	if out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Errorf("parallel_tool_calls = %v, want false", out.ParallelToolCalls)
	}
}

func TestResponsesRefusalContent(t *testing.T) {
	resp := &openai.ResponsesResponse{
		ID:     "resp_r",
		Status: "completed",
		Output: []openai.ResponsesOutputItem{{
			Type:    "message",
			Content: []openai.ResponsesOutputContent{{Type: "refusal", Refusal: "I can't help with that."}},
		}},
	}
	out, err := TranslateResponsesResponse(resp, "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != "refusal" {
		t.Errorf("stop = %s, want refusal", out.StopReason)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "I can't help with that." {
		t.Errorf("content = %+v", out.Content)
	}
}

func TestResponsesFailedWithoutErrorMessage(t *testing.T) {
	_, err := TranslateResponsesResponse(&openai.ResponsesResponse{ID: "resp_x", Status: "failed"}, "gpt-6-astra")
	if err == nil || !strings.Contains(err.Error(), "response failed") {
		t.Errorf("err = %v, want failed response error", err)
	}
}

func TestResponsesMinimumOutputTokens(t *testing.T) {
	body := `{"model":"gpt-6-astra","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	out, err := TranslateResponsesRequest(parseReq(t, body), responsesRoute("effort"))
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxOutputTokens != 16 {
		t.Errorf("max_output_tokens = %d, want 16", out.MaxOutputTokens)
	}
}

func TestResponsesDisabledThinkingOmitsReasoning(t *testing.T) {
	body := `{"model":"gpt-6-astra","max_tokens":16,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`
	out, err := TranslateResponsesRequest(parseReq(t, body), responsesRoute("effort"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Reasoning != nil {
		t.Errorf("reasoning = %+v, want omitted", out.Reasoning)
	}
}

func TestResponsesStreamFlushesPendingToolOnCompleted(t *testing.T) {
	evs := runResponsesStream(t, []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_a","name":"read_file"}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"path\":\"a.go\"}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call"}],"usage":{"input_tokens":8,"output_tokens":4}}}`,
	})
	if !strings.Contains(names(evs), "content_block_start") {
		t.Fatalf("pending tool was dropped: %s", names(evs))
	}
	last := evs[len(evs)-2]
	if last.data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", last.data["delta"])
	}
}

func TestStreamAcceptsDataWithoutSpace(t *testing.T) {
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt")
	body := "data:{\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata:[DONE]\n\n"
	_, errType := state.Run(context.Background(), strings.NewReader(body))
	if errType != "" {
		t.Fatalf("errType = %q", errType)
	}
	if !strings.Contains(rec.Body.String(), `"text":"hi"`) {
		t.Errorf("missing text delta: %s", rec.Body.String())
	}
}
