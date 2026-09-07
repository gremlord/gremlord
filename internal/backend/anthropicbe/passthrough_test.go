package anthropicbe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
)

func mkCall(t *testing.T, body, baseURL, upstreamModel string) *backend.Call {
	t.Helper()
	env, err := anthropic.ParseEnvelope([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return &backend.Call{
		Raw:      []byte(body),
		Envelope: env,
		Route: config.Resolved{
			Alias:        env.Model,
			ProviderName: "anthropic",
			Provider:     config.Provider{Type: "anthropic", BaseURL: baseURL, APIKey: "sk-test"},
			Model:        config.Model{ID: upstreamModel},
		},
		Header: http.Header{"Anthropic-Version": []string{"2023-06-01"}, "Anthropic-Beta": []string{"claude-code-20250219"}},
		Query:  url.Values{},
	}
}

func TestByteFaithfulPassthrough(t *testing.T) {
	body := `{"model":"claude-sonnet-5","max_tokens":100,"temperature":0.5,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
	var gotBody []byte
	var gotHeaders http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`))
	}))
	defer up.Close()

	call := mkCall(t, body, up.URL, "claude-sonnet-5") // alias == upstream
	rec := httptest.NewRecorder()
	res := New().Messages(context.Background(), call, rec)

	if string(gotBody) != body {
		t.Errorf("body was modified:\n got %s\nwant %s", gotBody, body)
	}
	if gotHeaders.Get("x-api-key") != "sk-test" {
		t.Error("provider key not injected")
	}
	if gotHeaders.Get("anthropic-beta") != "claude-code-20250219" {
		t.Error("anthropic-beta not forwarded")
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 ||
		res.Usage.CacheReadInputTokens != 3 || res.Usage.CacheCreationInputTokens != 2 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestPoisonedToolIDsAreRepairedForAnthropic(t *testing.T) {
	body := `{"model":"claude-sonnet-5","max_tokens":100,"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"Bash:0","name":"Bash","input":{"command":"ls"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"Bash:0","content":"ok"}]}` +
		`]}`
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(`{"usage":{}}`))
	}))
	defer up.Close()

	// alias == upstream model: this normally takes the byte-faithful path,
	// but a poisoned history must be rewritten so an existing session can
	// recover without editing its transcript on disk.
	call := mkCall(t, body, up.URL, "claude-sonnet-5")
	New().Messages(context.Background(), call, httptest.NewRecorder())

	msgs := got["messages"].([]any)
	toolUse := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	toolResult := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolUse["id"] != "Bash_0" || toolResult["tool_use_id"] != "Bash_0" {
		t.Fatalf("matching ids not repaired: tool_use=%v tool_result=%v", toolUse["id"], toolResult["tool_use_id"])
	}
}

func TestUnsignedThinkingIsRepairedForAnthropic(t *testing.T) {
	cases := []struct {
		name     string
		thinking string
	}{
		{"absent signature", `{"type":"thinking","thinking":"translated reasoning"}`},
		{"empty signature", `{"type":"thinking","thinking":"translated reasoning","signature":""}`},
		{"null signature", `{"type":"thinking","thinking":"translated reasoning","signature":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"claude-sonnet-5","max_tokens":100,"messages":[` +
				`{"role":"user","content":"hi"},` +
				`{"role":"assistant","content":[` + tc.thinking + `,{"type":"text","text":"answer"}]},` +
				`{"role":"user","content":"continue"}]}`
			var got map[string]any
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				w.Write([]byte(`{"usage":{}}`))
			}))
			defer up.Close()

			// alias == upstream normally stays byte-faithful; unsigned thinking
			// must still force normalization so existing transcripts self-heal.
			call := mkCall(t, body, up.URL, "claude-sonnet-5")
			New().Messages(context.Background(), call, httptest.NewRecorder())

			blocks := got["messages"].([]any)[1].(map[string]any)["content"].([]any)
			if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "text" {
				t.Fatalf("assistant content = %#v, want only text", blocks)
			}
		})
	}
}

func TestThinkingRepairPreservesAnthropicBlocks(t *testing.T) {
	body := `{"model":"alias","max_tokens":100,"messages":[` +
		`{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"signed","signature":"sig_abc"},` +
		`{"type":"redacted_thinking","data":"encrypted"},` +
		`{"type":"text","text":"answer"}]},` +
		`{"role":"user","content":[{"type":"thinking","thinking":"user content"}]}]}`
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(`{"usage":{}}`))
	}))
	defer up.Close()

	// Alias rewrite exercises normalizeForModel without requiring poison.
	call := mkCall(t, body, up.URL, "claude-sonnet-5")
	New().Messages(context.Background(), call, httptest.NewRecorder())

	msgs := got["messages"].([]any)
	assistant := msgs[0].(map[string]any)["content"].([]any)
	if len(assistant) != 3 || assistant[0].(map[string]any)["signature"] != "sig_abc" ||
		assistant[1].(map[string]any)["type"] != "redacted_thinking" {
		t.Fatalf("signed/redacted thinking changed: %#v", assistant)
	}
	user := msgs[1].(map[string]any)["content"].([]any)
	if len(user) != 1 || user[0].(map[string]any)["type"] != "thinking" {
		t.Fatalf("user content changed: %#v", user)
	}
}

func TestReasoningOnlyAssistantBecomesText(t *testing.T) {
	body := `{"model":"claude-sonnet-5","max_tokens":100,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"translated reasoning","signature":""}]},` +
		`{"role":"user","content":"continue"}]}`
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(`{"usage":{}}`))
	}))
	defer up.Close()

	call := mkCall(t, body, up.URL, "claude-sonnet-5")
	New().Messages(context.Background(), call, httptest.NewRecorder())

	blocks := got["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "text" ||
		blocks[0].(map[string]any)["text"] != "translated reasoning" {
		t.Fatalf("assistant content = %#v", blocks)
	}
}

func TestModelRewriteOnlyTouchesModel(t *testing.T) {
	body := `{"model":"cheap","max_tokens":100,"temperature":0.5,"messages":[]}`
	var gotBody map[string]json.RawMessage
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	call := mkCall(t, body, up.URL, "claude-haiku-4-5")
	New().Messages(context.Background(), call, httptest.NewRecorder())

	if string(gotBody["model"]) != `"claude-haiku-4-5"` {
		t.Errorf("model = %s", gotBody["model"])
	}
	if string(gotBody["temperature"]) != "0.5" {
		t.Errorf("temperature mangled: %s (json.Number must be preserved)", gotBody["temperature"])
	}
}

func TestSSEUsageTee(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":50,"cache_creation_input_tokens":7}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n") + "\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sse))
	}))
	defer up.Close()

	call := mkCall(t, `{"model":"claude-sonnet-5","stream":true,"messages":[]}`, up.URL, "claude-sonnet-5")
	rec := httptest.NewRecorder()
	res := New().Messages(context.Background(), call, rec)

	if rec.Body.String() != sse {
		t.Errorf("stream body altered:\n got %q\nwant %q", rec.Body.String(), sse)
	}
	if res.Usage.InputTokens != 100 || res.Usage.OutputTokens != 42 ||
		res.Usage.CacheReadInputTokens != 50 || res.Usage.CacheCreationInputTokens != 7 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestUpstreamErrorPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer up.Close()

	call := mkCall(t, `{"model":"claude-sonnet-5","messages":[]}`, up.URL, "claude-sonnet-5")
	rec := httptest.NewRecorder()
	res := New().Messages(context.Background(), call, rec)

	if rec.Code != 429 || res.ErrType != "rate_limit_error" {
		t.Errorf("status=%d errType=%q", rec.Code, res.ErrType)
	}
	if !strings.Contains(rec.Body.String(), "slow down") {
		t.Errorf("error body not passed through: %s", rec.Body.String())
	}
}

func TestNormalizeForModel(t *testing.T) {
	legacy := `{"model":"auto","max_tokens":100,"temperature":1,"top_k":40,` +
		`"thinking":{"type":"enabled","budget_tokens":8000},"messages":[]}`

	// Opus 4.8: enabled -> adaptive, sampling stripped.
	out, err := rewriteForModel([]byte(legacy), "claude-opus-4-8")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	th := m["thinking"].(map[string]any)
	if th["type"] != "adaptive" || th["budget_tokens"] != nil {
		t.Errorf("opus-4-8 thinking: %v", th)
	}
	if _, has := m["temperature"]; has {
		t.Error("opus-4-8 must not receive temperature")
	}

	// Fable: thinking stripped entirely.
	out, _ = rewriteForModel([]byte(legacy), "claude-fable-5")
	m = map[string]any{}
	json.Unmarshal(out, &m)
	if _, has := m["thinking"]; has {
		t.Error("fable must not receive a thinking field")
	}

	// Haiku 4.5: legacy config passes through untouched.
	out, _ = rewriteForModel([]byte(legacy), "claude-haiku-4-5")
	m = map[string]any{}
	json.Unmarshal(out, &m)
	th = m["thinking"].(map[string]any)
	if th["type"] != "enabled" {
		t.Errorf("haiku thinking: %v", th)
	}
	if _, has := m["temperature"]; !has {
		t.Error("haiku keeps sampling params")
	}
}

func TestNormalizeEffortAndSystemMessages(t *testing.T) {
	body := `{"model":"auto","max_tokens":100,
	  "output_config":{"effort":"high"},
	  "messages":[
	    {"role":"user","content":"do the thing"},
	    {"role":"system","content":"Terse mode enabled."},
	    {"role":"assistant","content":"ok"}
	  ]}`

	// Haiku 4.5: effort stripped, system message folded into the user turn.
	out, err := rewriteForModel([]byte(body), "claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if _, has := m["output_config"]; has {
		t.Error("haiku must not receive output_config.effort")
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("system message not folded: %d messages", len(msgs))
	}
	user := msgs[0].(map[string]any)
	blocks := user["content"].([]any)
	last := blocks[len(blocks)-1].(map[string]any)
	if !strings.Contains(last["text"].(string), "<system-reminder>") ||
		!strings.Contains(last["text"].(string), "Terse mode enabled.") {
		t.Errorf("folded block: %v", last)
	}
	if msgs[1].(map[string]any)["role"] != "assistant" {
		t.Error("alternation broken")
	}

	// Opus 4.8: both preserved (supports effort and mid-conv system).
	out, _ = rewriteForModel([]byte(body), "claude-opus-4-8")
	m = map[string]any{}
	json.Unmarshal(out, &m)
	if _, has := m["output_config"]; !has {
		t.Error("opus-4-8 keeps output_config")
	}
	if len(m["messages"].([]any)) != 3 {
		t.Error("opus-4-8 keeps the system message")
	}
}

// Claude Code sends its betas comma-joined on one line, but ANTHROPIC_CUSTOM_HEADERS
// (or any intermediary) can produce repeated header lines, and Header.Get would
// keep only the first. Losing advanced-tool-use-2025-11-20 while the conversation
// carries tool_reference blocks earns a 400 that persists for the rest of the
// session, since those blocks stay in history.
func TestRepeatedAnthropicBetaHeadersAreAllForwarded(t *testing.T) {
	body := `{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	var gotHeaders http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()

	call := mkCall(t, body, up.URL, "claude-sonnet-5")
	call.Header["Anthropic-Beta"] = []string{"claude-code-20250219", "advanced-tool-use-2025-11-20"}
	New().Messages(context.Background(), call, httptest.NewRecorder())

	got := gotHeaders.Get("anthropic-beta")
	for _, want := range []string{"claude-code-20250219", "advanced-tool-use-2025-11-20"} {
		if !strings.Contains(got, want) {
			t.Errorf("anthropic-beta = %q, missing %s", got, want)
		}
	}
}
