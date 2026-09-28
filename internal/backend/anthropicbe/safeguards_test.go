package anthropicbe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Claude Code's server-side auto-mode review rides on the session's own
// Messages requests: an anthropic-beta value plus a `safeguards` body field
// on the way up, `safeguard_results` on the way down. A gateway that drops,
// reorders, or re-escapes any of it makes Claude Code fall back to billed
// classifier requests, so these tests pin the pass-through byte-for-byte on
// the paths that used to re-encode JSON.

// Key order deliberately unsorted and text carrying <, >, & — the two
// things a map round-trip through encoding/json changes.
const safeguardsBody = `{"model":"auto","max_tokens":100,"stream":true,` +
	`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.283"},{"type":"text","text":"<system-reminder>a & b</system-reminder>"}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"ls <dir>","cache_control":{"type":"ephemeral"}}]}],` +
	`"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},` +
	`"safeguards":{"zeta":1,"alpha":{"mode":"auto_review","note":"<b>"}}}`

func TestAliasSwapKeepsSafeguardsBodyByteIdentical(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	call := mkCall(t, safeguardsBody, up.URL, "claude-opus-5-5")
	New().Messages(context.Background(), call, httptest.NewRecorder())

	want := strings.Replace(safeguardsBody, `"model":"auto"`, `"model":"claude-opus-5-5"`, 1)
	if string(got) != want {
		t.Errorf("alias swap changed more than the model:\n got %s\nwant %s", got, want)
	}
}

// A body that genuinely needs repair is re-encoded, but unknown fields and
// literal text must still survive, unescaped.
func TestRepairedBodyKeepsSafeguards(t *testing.T) {
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	body := strings.Replace(safeguardsBody, `"thinking":{"type":"adaptive"}`, `"thinking":{"type":"enabled","budget_tokens":2000}`, 1)
	call := mkCall(t, body, up.URL, "claude-sonnet-5")
	New().Messages(context.Background(), call, httptest.NewRecorder())

	for _, frag := range []string{
		`"safeguards":{"alpha":{"mode":"auto_review","note":"<b>"},"zeta":1}`,
		`<system-reminder>a & b</system-reminder>`,
		`"thinking":{"type":"adaptive"}`,
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("repaired body missing %s:\n%s", frag, got)
		}
	}
}

func TestOpus5KeepsEffort(t *testing.T) {
	m := map[string]any{"output_config": map[string]any{"effort": "high"}}
	normalizeForModel(m, "claude-opus-5-5")
	if oc, _ := m["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("effort stripped on opus 5.5: %#v", m)
	}
}

const safeguardsSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"model":"claude-opus-5-5","id":"msg_1","content":[],"usage":{"input_tokens":100,"cache_creation_input_tokens":8,"cache_read_input_tokens":50,"output_tokens":1,"service_tier":"standard"}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01AbC","name":"Bash","input":{}}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42},"safeguard_results":[{"tool_use_id":"toolu_01AbC","verdict":"allow","reason":"<ok> & fine"}]}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestScaledStreamOnlyTouchesCounters(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(safeguardsSSE))
	}))
	defer up.Close()

	call := mkCall(t, `{"model":"claude-opus-5-5","stream":true,"messages":[]}`, up.URL, "claude-opus-5-5")
	call.GaugeBudget = 600_000 // the auto rule's gauge: factor 1/3
	rec := httptest.NewRecorder()
	New().Messages(context.Background(), call, rec)

	want := strings.NewReplacer(
		`"input_tokens":100`, `"input_tokens":34`,
		`"cache_creation_input_tokens":8`, `"cache_creation_input_tokens":3`,
		`"cache_read_input_tokens":50`, `"cache_read_input_tokens":17`,
	).Replace(safeguardsSSE)
	if rec.Body.String() != want {
		t.Errorf("scaled stream changed more than the counters:\n got %s\nwant %s", rec.Body.String(), want)
	}
}

func TestScaledJSONOnlyTouchesCounters(t *testing.T) {
	resp := `{"id":"msg_1","type":"message","content":[{"type":"tool_use","id":"toolu_01AbC","name":"Bash","input":{"command":"a && b > c"}}],` +
		`"usage":{"output_tokens":5,"input_tokens":90,"cache_read_input_tokens":30},` +
		`"safeguard_results":[{"tool_use_id":"toolu_01AbC","verdict":"allow"}]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(resp))
	}))
	defer up.Close()

	call := mkCall(t, `{"model":"claude-opus-5-5","messages":[]}`, up.URL, "claude-opus-5-5")
	call.GaugeBudget = 600_000
	rec := httptest.NewRecorder()
	New().Messages(context.Background(), call, rec)

	want := strings.NewReplacer(`"input_tokens":90`, `"input_tokens":30`, `"cache_read_input_tokens":30`, `"cache_read_input_tokens":10`).Replace(resp)
	if rec.Body.String() != want {
		t.Errorf("scaled body changed more than the counters:\n got %s\nwant %s", rec.Body.String(), want)
	}
}

func TestAnthropicHeadersPassThroughBothWays(t *testing.T) {
	var gotReq http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
		w.Header().Set("x-should-retry", "false")
		w.Header().Set("request-id", "req_1")
		w.Header().Set("x-internal-upstream", "leak")
		w.WriteHeader(429)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer up.Close()

	call := mkCall(t, `{"model":"claude-opus-5-5","messages":[]}`, up.URL, "claude-opus-5-5")
	call.Header.Set("anthropic-some-future-capability", "on")
	call.Header.Set("x-gremlord-session", "s1")
	rec := httptest.NewRecorder()
	New().Messages(context.Background(), call, rec)

	if gotReq.Get("anthropic-some-future-capability") != "on" {
		t.Error("unknown anthropic-* request header was stripped")
	}
	if gotReq.Get("x-gremlord-session") != "" {
		t.Error("gremlord's own headers leaked upstream")
	}
	h := rec.Header()
	for k, v := range map[string]string{
		"anthropic-ratelimit-unified-status":         "allowed",
		"anthropic-ratelimit-unified-5h-utilization": "0.42",
		"x-should-retry":                             "false",
		"request-id":                                 "req_1",
	} {
		if h.Get(k) != v {
			t.Errorf("response header %s = %q, want %q", k, h.Get(k), v)
		}
	}
	if h.Get("x-internal-upstream") != "" {
		t.Error("non-contract upstream header forwarded")
	}
}

// Claude Code appends mid-conversation system reminders. On models that
// accept them the whole body must still take the splice-only path.
func TestSystemMessageKeptVerbatimOnCurrentModels(t *testing.T) {
	body := `{"model":"auto","messages":[{"role":"user","content":"hi"},{"role":"system","content":"<system-reminder>x</system-reminder>"}],"safeguards":{"b":2,"a":1}}`
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5", "claude-fable-5-1"} {
		out, err := rewriteForModel([]byte(body), model)
		want := strings.Replace(body, `"auto"`, `"`+model+`"`, 1)
		if err != nil || string(out) != want {
			t.Errorf("%s: got %s err=%v", model, out, err)
		}
	}
	out, _ := rewriteForModel([]byte(body), "claude-haiku-4-5")
	if strings.Contains(string(out), `"role":"system"`) {
		t.Errorf("haiku must still fold system messages: %s", out)
	}
}

// Verified against the API 2026-09-28: opus 5.5 rejects thinking.type
// "enabled" and temperature/top_p with 400s, so the opus alias must be
// normalized before dispatch.
func TestOpus5LegacyThinkingAndSamplingNormalized(t *testing.T) {
	m := map[string]any{
		"thinking":    map[string]any{"type": "enabled", "budget_tokens": json.Number("1024")},
		"temperature": json.Number("0.5"),
		"top_p":       json.Number("0.9"),
	}
	normalizeForModel(m, "claude-opus-5-5")
	if got, _ := m["thinking"].(map[string]any); got["type"] != "adaptive" || len(got) != 1 {
		t.Errorf("thinking = %#v", m["thinking"])
	}
	if _, ok := m["temperature"]; ok {
		t.Error("temperature survived")
	}
	if _, ok := m["top_p"]; ok {
		t.Error("top_p survived")
	}
}
