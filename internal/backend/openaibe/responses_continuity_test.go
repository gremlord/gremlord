package openaibe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/openai"
	"github.com/gremlord/gremlord/internal/wire"
)

const continuityOutput = `[
 {"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque-ciphertext"},
 {"type":"message","id":"msg_1","role":"assistant","phase":"commentary","status":"completed","content":[{"type":"output_text","text":"Checking both files.","annotations":[]}]},
 {"type":"function_call","id":"fc_1","call_id":"call:a","name":"read_file","arguments":"{\"path\":\"a.go\",\"line\":9007199254740993}"},
 {"type":"function_call","id":"fc_2","call_id":"call_b","name":"read_file","arguments":"{\"path\":\"b.go\"}"}
]`

func continuityResponse(t *testing.T) *openai.ResponsesResponse {
	t.Helper()
	var resp openai.ResponsesResponse
	if err := json.Unmarshal([]byte(`{"id":"resp_1","status":"completed","output":`+continuityOutput+`,"usage":{"input_tokens":2000,"output_tokens":1000}}`), &resp); err != nil {
		t.Fatal(err)
	}
	return &resp
}

func continuityCall() *backend.Call {
	return &backend.Call{Route: responsesRoute("effort"),
		Header: http.Header{wire.HeaderSession: []string{"session-a"}}}
}

func continuityRequest(t *testing.T, call *backend.Call, messages []anthropic.Message) *openai.ResponsesRequest {
	t.Helper()
	r, err := TranslateResponsesRequest(&anthropic.MessagesRequest{MaxTokens: 4096, Messages: messages}, call.Route)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func continuityHistory(t *testing.T) []anthropic.Message {
	t.Helper()
	resp, err := TranslateResponsesResponse(continuityResponse(t), "gpt")
	if err != nil {
		t.Fatal(err)
	}
	messages := []anthropic.Message{{Role: "user", Content: anthropic.MessageBody{{Type: "text", Text: "read both files"}}}}
	// Claude Code can split a response into separate assistant messages.
	for _, block := range resp.Content {
		if block.Type == "tool_use" && block.ID == "call_a" {
			block.Input = json.RawMessage(`{ "line": 9007199254740993, "path": "a.go" }`)
		}
		messages = append(messages, anthropic.Message{Role: "assistant", Content: anthropic.MessageBody{block}})
	}
	messages = append(messages, anthropic.Message{Role: "user", Content: anthropic.MessageBody{
		{Type: "tool_result", ToolUseID: "call_a", Content: anthropic.MessageBody{{Type: "text", Text: "package a"}}},
		{Type: "tool_result", ToolUseID: "call_b", Content: anthropic.MessageBody{{Type: "text", Text: "package b"}}},
	}})
	return messages
}

func replayCount(req *openai.ResponsesRequest) int {
	n := 0
	for _, item := range req.Input {
		if len(item.Replay) != 0 {
			n++
		}
	}
	return n
}

func TestResponsesContinuityReplayAndIsolation(t *testing.T) {
	for _, scenario := range []string{"same", "session", "provider", "endpoint", "key", "model", "instructions", "edited_history", "compacted", "tool_arguments", "no_session", "legacy_header"} {
		t.Run(scenario, func(t *testing.T) {
			cache := newResponsesContinuity()
			call := continuityCall()
			history := continuityHistory(t)
			first := continuityRequest(t, call, history[:1])
			cache.prepare(call, first, 20).remember(continuityResponse(t))
			next := continuityRequest(t, call, history)
			switch scenario {
			case "session":
				call.Header.Set(wire.HeaderSession, "session-b")
			case "provider":
				call.Route.ProviderName = "other"
			case "endpoint":
				call.Route.Provider.BaseURL = "https://another.invalid/v1"
			case "key":
				call.Route.Provider.APIKey = "other-credential"
			case "model":
				next.Model = "another-model"
			case "instructions":
				next.Instructions = "changed instructions"
			case "edited_history":
				next.Input[0].Content = []openai.ResponsesContentPart{{Type: "input_text", Text: "different task"}}
			case "compacted":
				next.Input = next.Input[1:]
			case "tool_arguments":
				next.Input[2].Arguments = `{"path":"different.go"}`
			case "no_session":
				call.Header = nil
			case "legacy_header":
				call.Header = http.Header{wire.LegacyHeaderSession: []string{"session-a"}}
			}
			turn := cache.prepare(call, next, 100)
			want := 0
			if scenario == "same" || scenario == "legacy_header" {
				want = 4
			}
			if got := replayCount(next); got != want {
				t.Fatalf("replayed %d items, want %d", got, want)
			}
			if want == 0 {
				return
			}
			if len(next.Input) != 7 {
				t.Fatalf("duplicated/lost input items: %+v", next.Input)
			}
			if next.Input[5].CallID != "call:a" || next.Input[6].CallID != "call_b" {
				t.Fatal("tool results were not paired with original call ids")
			}
			if turn.inputEstimate < 3000 {
				t.Fatalf("hidden context not accounted for: %d", turn.inputEstimate)
			}
			var raw map[string]any
			json.Unmarshal(next.Input[1].Replay, &raw)
			if raw["encrypted_content"] != "opaque-ciphertext" || len(raw["summary"].([]any)) != 0 {
				t.Fatal("reasoning changed")
			}
			json.Unmarshal(next.Input[2].Replay, &raw)
			if raw["phase"] != "commentary" {
				t.Fatal("phase lost")
			}
		})
	}
}

func TestResponsesContinuityEvictionAndFailures(t *testing.T) {
	for _, scenario := range []string{"ttl", "bytes", "entries", "failed", "incomplete", "reasoning_only"} {
		t.Run(scenario, func(t *testing.T) {
			cache := newResponsesContinuity()
			now := time.Now()
			cache.now = func() time.Time { return now }
			call := continuityCall()
			history := continuityHistory(t)
			resp := continuityResponse(t)
			switch scenario {
			case "bytes":
				cache.maxBytes = 10
			case "entries":
				cache.maxEntries = 0
			case "failed", "incomplete":
				resp.Status = scenario
			case "reasoning_only":
				resp.Output = resp.Output[:1]
			}
			cache.prepare(call, continuityRequest(t, call, history[:1]), 20).remember(resp)
			if scenario == "ttl" {
				now = now.Add(cache.ttl)
			}
			next := continuityRequest(t, call, history)
			cache.prepare(call, next, 100)
			if replayCount(next) != 0 || cache.lru.Len() != 0 || cache.bytes != 0 {
				t.Fatal("invalid or expired continuation retained")
			}
		})
	}
}

type continuityTransport func(*http.Request) (*http.Response, error)

func (f continuityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResponsesBackendContinuity(t *testing.T) {
	for _, mode := range []string{"json", "stream", "done_only", "terminal_only", "no_terminal_output"} {
		t.Run(mode, func(t *testing.T) {
			b := New()
			count := 0
			b.client.Transport = continuityTransport(func(r *http.Request) (*http.Response, error) {
				var request struct {
					Include []string          `json:"include"`
					Store   bool              `json:"store"`
					Input   []json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if r.URL.Path != "/v1/responses" || request.Store || len(request.Include) != 1 || request.Include[0] != "reasoning.encrypted_content" {
					t.Fatal("wrong request settings")
				}
				count++
				if count == 2 {
					if len(request.Input) != 7 || !strings.Contains(string(request.Input[1]), "opaque-ciphertext") || !strings.Contains(string(request.Input[2]), `"phase":"commentary"`) {
						t.Fatalf("missing original output: %s", request.Input)
					}
					if !strings.Contains(string(request.Input[5]), `"call_id":"call:a"`) {
						t.Fatal("call id not restored")
					}
				}
				resp := continuityResponse(t)
				raw, _ := json.Marshal(resp)
				body := string(raw)
				if mode != "json" {
					var events []string
					if mode != "terminal_only" {
						for i, item := range resp.Output {
							if mode == "stream" || mode == "no_terminal_output" {
								added := item
								added.Content, added.Summary, added.Arguments, added.EncryptedContent = nil, nil, "", ""
								data, _ := json.Marshal(openai.ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: i, Item: &added})
								events = append(events, string(data))
								if item.Type == "message" {
									data, _ = json.Marshal(openai.ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: i, ItemID: item.ID, Delta: item.Content[0].Text})
									events = append(events, string(data))
								}
							}
							data, _ := json.Marshal(openai.ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: i, Item: &item})
							events = append(events, string(data))
						}
					}
					if mode == "no_terminal_output" {
						resp.Output = nil
					}
					data, _ := json.Marshal(openai.ResponsesStreamEvent{Type: "response.completed", Response: resp})
					events = append(events, string(data))
					body = "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			call := continuityCall()
			call.Envelope.Model = "gpt"
			history := continuityHistory(t)
			for i, messages := range [][]anthropic.Message{history[:1], history} {
				call.Raw, _ = json.Marshal(anthropic.MessagesRequest{Model: "gpt", MaxTokens: 4096, Stream: mode != "json", Messages: messages})
				rec := httptest.NewRecorder()
				res := b.Messages(context.Background(), call, rec)
				if res.Status != 200 {
					t.Fatalf("status %d: %s", res.Status, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), "opaque-ciphertext") {
					t.Fatal("ciphertext exposed to Anthropic client")
				}
				if !strings.Contains(rec.Body.String(), "Checking both files.") {
					t.Fatal("message output lost")
				}
				if i == 1 && mode != "json" {
					events := parseEvents(t, rec.Body.String())
					start := events[0].data["message"].(map[string]any)
					if start["usage"].(map[string]any)["input_tokens"].(float64) < 3000 {
						t.Fatal("message_start undercounted hidden context")
					}
				}
			}
			// count_tokens must see hidden context too, without contacting upstream.
			rec := httptest.NewRecorder()
			b.CountTokens(context.Background(), call, rec)
			var usage anthropic.CountTokensResponse
			json.Unmarshal(rec.Body.Bytes(), &usage)
			if usage.InputTokens < 3000 || count != 2 {
				t.Fatalf("count_tokens = %d; upstream calls = %d", usage.InputTokens, count)
			}
		})
	}
}

func TestResponsesContinuityConcurrentBranches(t *testing.T) {
	cache := newResponsesContinuity()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			call := continuityCall()
			history := continuityHistory(t)
			history[0].Content[0].Text = fmt.Sprintf("branch %d", i)
			cache.prepare(call, continuityRequest(t, call, history[:1]), 20).remember(continuityResponse(t))
			next := continuityRequest(t, call, history)
			cache.prepare(call, next, 100)
			if replayCount(next) != 4 {
				t.Error("concurrent branch lost continuity")
			}
		}(i)
	}
	wg.Wait()
}

func TestResponsesContinuityMultipleTurns(t *testing.T) {
	cache := newResponsesContinuity()
	call := continuityCall()
	history := continuityHistory(t)
	cache.prepare(call, continuityRequest(t, call, history[:1]), 20).remember(continuityResponse(t))
	second := continuityRequest(t, call, history)
	turn := cache.prepare(call, second, 100)
	var final openai.ResponsesResponse
	json.Unmarshal([]byte(`{"status":"completed","output":[{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"second-ciphertext"},{"type":"message","id":"msg_final","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Both are valid."}]}],"usage":{"input_tokens":3200,"output_tokens":800}}`), &final)
	turn.remember(&final)
	history = append(history,
		anthropic.Message{Role: "assistant", Content: anthropic.MessageBody{{Type: "text", Text: "Both are valid."}}},
		anthropic.Message{Role: "user", Content: anthropic.MessageBody{{Type: "text", Text: "Now explain them."}}})
	third := continuityRequest(t, call, history)
	thirdTurn := cache.prepare(call, third, 140)
	if replayCount(third) != 6 || thirdTurn.inputEstimate < 4000 {
		t.Fatalf("three-turn continuity failed: count=%d estimate=%d", replayCount(third), thirdTurn.inputEstimate)
	}
	raw, _ := json.Marshal(third)
	if strings.Count(string(raw), "second-ciphertext") != 1 || strings.Count(string(raw), "opaque-ciphertext") != 1 || !strings.Contains(string(raw), `"phase":"final_answer"`) {
		t.Fatalf("duplicate or missing state: %s", raw)
	}
	// Compaction starts a new visible history. Old hidden context must not
	// inflate that fresh window or be resurrected behind its summary.
	compacted := continuityRequest(t, call, []anthropic.Message{{Role: "user", Content: anthropic.MessageBody{{Type: "text", Text: "Summary: both files were checked. Continue."}}}})
	newTurn := cache.prepare(call, compacted, 30)
	if replayCount(compacted) != 0 || newTurn.inputEstimate != 30 {
		t.Fatal("compaction resurrected old context")
	}
}

func TestResponsesContinuityDoesNotCachePartialNativeOutput(t *testing.T) {
	cache := newResponsesContinuity()
	call := continuityCall()
	history := continuityHistory(t)
	turn := cache.prepare(call, continuityRequest(t, call, history[:1]), 20)
	resp := continuityResponse(t)
	translated, _ := TranslateResponsesResponse(resp, "gpt")
	resp.Output = resp.Output[:2] // terminal payload lost both tool calls
	turn.remember(resp, anthropic.MessageBody(translated.Content))
	if cache.lru.Len() != 0 {
		t.Fatal("partial output could erase tool calls on replay")
	}
}

func TestResponsesContinuityLRUEviction(t *testing.T) {
	cache := newResponsesContinuity()
	cache.maxEntries = 2
	history := continuityHistory(t)
	for _, session := range []string{"a", "b", "c"} {
		call := continuityCall()
		call.Header.Set(wire.HeaderSession, session)
		cache.prepare(call, continuityRequest(t, call, history[:1]), 20).remember(continuityResponse(t))
	}
	for _, session := range []string{"a", "b", "c"} {
		call := continuityCall()
		call.Header.Set(wire.HeaderSession, session)
		req := continuityRequest(t, call, history)
		cache.prepare(call, req, 100)
		want := 4
		if session == "a" {
			want = 0
		}
		if replayCount(req) != want {
			t.Fatalf("session %s: replayed %d", session, replayCount(req))
		}
	}
	if cache.lru.Len() != 2 || len(cache.entries) != 2 {
		t.Fatal("entry bound exceeded")
	}
}
