package router

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
	"github.com/gremlord/gremlord/internal/tokens"
)

const testToken = "test-token"

func newTestServer(t *testing.T, upstreamURL string, budgets *config.Budget) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Router: config.Router{Port: 0},
		Providers: map[string]config.Provider{
			"fake": {Type: config.ProviderOpenAI, BaseURL: upstreamURL},
		},
		Models: map[string]config.Model{
			"fake-model": {Provider: "fake", ID: "fake-upstream-1"},
		},
		Profiles: map[string]config.Profile{
			"main": {Model: "fake-model"},
		},
		Budgets: budgets,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func post(t *testing.T, url, token, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("x-api-key", token)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Agentic-Session", "sess-test")
	req.Header.Set("X-Agentic-Profile", "main")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}

// TestOpenAIStreamThroughRouter drives the full path: Anthropic-shaped
// streaming request in, OpenAI upstream, Anthropic SSE out, usage row in
// SQLite.
func TestOpenAIStreamThroughRouter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"fake-upstream-1"`) {
			t.Errorf("alias not resolved upstream: %s", body)
		}
		if !strings.Contains(string(body), `"include_usage":true`) {
			t.Error("stream_options.include_usage missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"x","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	ts, st := newTestServer(t, upstream.URL, nil)
	resp, body := post(t, ts.URL+"/v1/messages", testToken,
		`{"model":"fake-model","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type: %s", resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		"event: message_start", "event: ping", "event: content_block_start",
		`"text":"hi"`, `"text_delta"`, `"stop_reason":"end_turn"`, "event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE missing %q in:\n%s", want, body)
		}
	}

	// Usage attributed and recorded.
	rows, err := st.SpendSince(time.Now().Add(-time.Minute), "session")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].Key != "sess-test" || rows[0].InputTokens != 11 || rows[0].OutputTokens != 3 {
		t.Errorf("usage row: %+v", rows[0])
	}
}

func TestOpenAIResponsesStreamThroughRouter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"fake-upstream-1"`) {
			t.Errorf("alias not resolved upstream: %s", body)
		}
		if !strings.Contains(string(body), `"store":false`) {
			t.Error("store:false missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.created","response":{"id":"resp_x"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_item.added","item":{"type":"message"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_x","status":"completed","usage":{"input_tokens":11,"output_tokens":3}}}`+"\n\n")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Router: config.Router{Port: 0},
		Providers: map[string]config.Provider{
			"fake": {Type: config.ProviderOpenAI, BaseURL: upstream.URL},
		},
		Models: map[string]config.Model{
			"fake-model": {Provider: "fake", ID: "fake-upstream-1", API: config.APIResponses},
		},
		Profiles: map[string]config.Profile{
			"main": {Model: "fake-model"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, body := post(t, ts.URL+"/v1/messages", testToken,
		`{"model":"fake-model","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"event: message_start", "event: content_block_start",
		`"text":"hi"`, `"text_delta"`, `"stop_reason":"end_turn"`, "event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE missing %q in:\n%s", want, body)
		}
	}
	rows, err := st.SpendSince(time.Now().Add(-time.Minute), "session")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].InputTokens != 11 || rows[0].OutputTokens != 3 {
		t.Errorf("usage row: %+v", rows[0])
	}
}

func TestCLIProviderDispatchesThroughRouter(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"codex": {Type: config.ProviderCLI, Dialect: config.CLIDialectCodex},
		},
		Models: map[string]config.Model{
			"codex": {Provider: "codex"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.cli.LookPath = func(string) (string, error) { return "", fmt.Errorf("not installed") }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(
		`{"model":"codex","messages":[{"role":"user","content":"fix it"}]}`))
	req.Header.Set("x-api-key", testToken)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Agentic-Cwd", t.TempDir())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "Codex") && !strings.Contains(string(body), "codex") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "not found on PATH") {
		t.Errorf("cli backend refusal missing from router response: %s", body)
	}
}

func TestCLIUsageStaysUnpricedWhenModelIDHasAPIPrice(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"codex": {Type: config.ProviderCLI, Dialect: config.CLIDialectCodex},
		},
		Models: map[string]config.Model{
			"codex": {Provider: "codex", ID: "claude-sonnet-5"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.recordUsage(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), config.Resolved{
		Alias: "codex", ProviderName: "codex", Provider: cfg.Providers["codex"], Model: cfg.Models["codex"],
	}, "codex", backend.Result{Status: 200, Usage: anthropic.Usage{InputTokens: 1000, OutputTokens: 1000}}, time.Millisecond,
		tokens.Composition{}, 0)

	rows, err := st.SpendSince(time.Now().Add(-time.Minute), "model")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].CostUSD != 0 || rows[0].Unpriced != 1 {
		t.Errorf("CLI subscription usage must remain unpriced: %+v", rows[0])
	}
}

func TestCLIDelegationBypassesAPIBudgetGate(t *testing.T) {
	dir := t.TempDir()
	hardStop := true
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"codex": {Type: config.ProviderCLI, Dialect: config.CLIDialectCodex},
		},
		Models:  map[string]config.Model{"codex": {Provider: "codex"}},
		Budgets: &config.Budget{Daily: 0.01, HardStop: &hardStop},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RecordUsage(store.UsageEvent{TS: time.Now(), CostUSD: 1, Priced: true}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.cli.LookPath = func(string) (string, error) { return "", fmt.Errorf("not installed") }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(
		`{"model":"codex","messages":[{"role":"user","content":"fix it"}]}`))
	req.Header.Set("x-api-key", testToken)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Agentic-Cwd", t.TempDir())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "budget exceeded") || !strings.Contains(string(body), "not found on PATH") {
		t.Fatalf("CLI request should reach CLI backend despite API budget: status=%d body=%s", resp.StatusCode, body)
	}
}

// TestAutoRouteEndToEnd exercises dynamic routing through the full server:
// the classifier call and the routed call both hit the fake upstream.
func TestAutoRouteEndToEnd(t *testing.T) {
	var seenModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		seenModels = append(seenModels, req.Model)
		w.Header().Set("Content-Type", "application/json")
		if req.Model == "classifier-upstream" {
			fmt.Fprint(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"deep"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":1}}`)
			return
		}
		fmt.Fprint(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"planned"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"fake": {Type: config.ProviderOpenAI, BaseURL: upstream.URL},
		},
		Models: map[string]config.Model{
			"cheap": {Provider: "fake", ID: "classifier-upstream"},
			"big":   {Provider: "fake", ID: "deep-upstream"},
			"small": {Provider: "fake", ID: "light-upstream"},
		},
		Routing: map[string]config.RouteRule{
			"auto": {Classifier: "cheap", Default: "light",
				Tiers: map[string]string{"deep": "big", "light": "small"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, respBody := post(t, ts.URL+"/v1/messages", testToken,
		`{"model":"auto","max_tokens":50,"messages":[{"role":"user","content":"architect the whole system"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, respBody)
	}
	// Two classifier calls hit the fake upstream before the routed call:
	// tier routing, then goal detection (its answer here is the tier
	// classifier's plain-text reply, which fails to parse as goal JSON and
	// fails open — that's fine, this test only cares about routing).
	if len(seenModels) != 3 || seenModels[0] != "classifier-upstream" || seenModels[1] != "classifier-upstream" || seenModels[2] != "deep-upstream" {
		t.Errorf("upstream saw %v, want [classifier-upstream classifier-upstream deep-upstream]", seenModels)
	}
	if !strings.Contains(respBody, `"model":"auto"`) {
		t.Errorf("alias not echoed: %s", respBody)
	}

	// The routing decision is persisted so the statusline can show it.
	alias, tier, model, _, ok, err := st.LatestRouteDecision("sess-test")
	if err != nil || !ok || alias != "auto" || tier != "deep" || model != "big" {
		t.Errorf("route decision: alias=%s tier=%s model=%s ok=%v err=%v, want auto/deep/big/true/nil",
			alias, tier, model, ok, err)
	}
}

// TestPinModelOverridesRequestAndBypassesAutoRoute exercises the router-side
// enforcement of X-Agentic-Pin-Model: a pinned session's request must resolve
// to the pinned alias even when the request body names a different model,
// and dynamic `auto` routing (classifier + goal detection) must never fire —
// the whole point of pinning is that no other logic gets a vote.
func TestPinModelOverridesRequestAndBypassesAutoRoute(t *testing.T) {
	var seenModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		seenModels = append(seenModels, req.Model)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"fake": {Type: config.ProviderOpenAI, BaseURL: upstream.URL},
		},
		Models: map[string]config.Model{
			"cheap": {Provider: "fake", ID: "classifier-upstream"},
			"big":   {Provider: "fake", ID: "big-upstream"},
			"small": {Provider: "fake", ID: "small-upstream"},
		},
		Routing: map[string]config.RouteRule{
			"auto": {Classifier: "cheap", Default: "light",
				Tiers: map[string]string{"deep": "big", "light": "small"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Request body asks for the dynamic "auto" alias (which would normally
	// trigger the classifier), but the session is pinned to "small" — the
	// pin must win outright, with zero classifier calls.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages",
		strings.NewReader(`{"model":"auto","max_tokens":50,"messages":[{"role":"user","content":"architect the whole system"}]}`))
	req.Header.Set("x-api-key", testToken)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Agentic-Session", "sess-pinned")
	req.Header.Set("X-Agentic-Profile", "main")
	req.Header.Set("X-Agentic-Pin-Model", "small")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, respBody)
	}
	if len(seenModels) != 1 || seenModels[0] != "small-upstream" {
		t.Errorf("upstream saw %v, want exactly [small-upstream] (pin must bypass classifier entirely)", seenModels)
	}

	// No route decision recorded — pinning skips the auto-router altogether.
	if _, _, _, _, ok, _ := st.LatestRouteDecision("sess-pinned"); ok {
		t.Error("route decision recorded for a pinned session; pin should bypass auto-routing")
	}
}

// TestAutoGoalEndToEnd exercises goal detection through the full server:
// a goal-worthy classifier verdict must show up in the forwarded request's
// system prompt (as the harness's own loop sentinel) and be persisted.
func TestAutoGoalEndToEnd(t *testing.T) {
	var routedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "persistent recurring loop"):
			// The goal classifier call.
			fmt.Fprint(w, `{"id":"g","choices":[{"index":0,"message":{"role":"assistant","content":"{\"goal\":true,\"reason\":\"polling a long build\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":10}}`)
		case req.Model == "classifier-upstream":
			// The tier classifier call.
			fmt.Fprint(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"standard"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":1}}`)
		default:
			// The routed main-model call — capture what actually got sent.
			routedBody = string(body)
			fmt.Fprint(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
		}
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"fake": {Type: config.ProviderOpenAI, BaseURL: upstream.URL},
		},
		Models: map[string]config.Model{
			"cheap":    {Provider: "fake", ID: "classifier-upstream"},
			"standard": {Provider: "fake", ID: "standard-upstream"},
		},
		Routing: map[string]config.RouteRule{
			"auto": {Classifier: "cheap", Default: "standard",
				Tiers: map[string]string{"standard": "standard"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, respBody := post(t, ts.URL+"/v1/messages", testToken,
		`{"model":"auto","max_tokens":50,"messages":[{"role":"user","content":"keep checking every few minutes whether the build passes"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, respBody)
	}

	var routed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(routedBody), &routed); err != nil {
		t.Fatalf("routed body not JSON: %v\n%s", err, routedBody)
	}
	var systemContent string
	for _, m := range routed.Messages {
		if m.Role == "system" {
			systemContent = m.Content
		}
	}
	for _, want := range []string{"<system-reminder>", "<<autonomous-loop-dynamic>>", "polling a long build"} {
		if !strings.Contains(systemContent, want) {
			t.Errorf("routed request's system message missing %q:\n%s", want, systemContent)
		}
	}

	goal, reason, ok, err := st.LatestGoalDecision("sess-test")
	if err != nil || !ok || !goal || reason != "polling a long build" {
		t.Errorf("goal decision: goal=%v reason=%q ok=%v err=%v, want true/\"polling a long build\"/true/nil",
			goal, reason, ok, err)
	}

	// A tool_result continuation of the same turn must not clobber the
	// decision recorded when the turn opened — the goal classifier never
	// re-runs mid-turn (mirrors autoRoute's stickiness), so the prior
	// verdict must still be persisted afterward.
	resp, respBody = post(t, ts.URL+"/v1/messages", testToken,
		`{"model":"auto","max_tokens":50,"messages":[
			{"role":"user","content":"keep checking every few minutes whether the build passes"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"check_build","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"still running"}]}
		]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("continuation status %d: %s", resp.StatusCode, respBody)
	}
	goal, reason, ok, err = st.LatestGoalDecision("sess-test")
	if err != nil || !ok || !goal || reason != "polling a long build" {
		t.Errorf("goal decision after continuation: goal=%v reason=%q ok=%v err=%v, want true/\"polling a long build\"/true/nil (must not be clobbered)",
			goal, reason, ok, err)
	}
}

func TestBudgetHardStop(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not reach upstream when over budget")
	}))
	defer upstream.Close()

	ts, st := newTestServer(t, upstream.URL, &config.Budget{Daily: 0.01})
	// Pre-record spend over the cap.
	st.RecordUsage(store.UsageEvent{TS: time.Now(), Profile: "main", CostUSD: 0.02, Priced: true})

	resp, body := post(t, ts.URL+"/v1/messages", testToken,
		`{"model":"fake-model","max_tokens":50,"messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 (429/5xx would retry-spin)", resp.StatusCode)
	}
	if !strings.Contains(body, "daily budget exceeded") || !strings.Contains(body, "invalid_request_error") {
		t.Errorf("body: %s", body)
	}
}

func TestAuthRequired(t *testing.T) {
	ts, _ := newTestServer(t, "http://127.0.0.1:1", nil)
	resp, _ := post(t, ts.URL+"/v1/messages", "wrong-token", `{"model":"fake-model","messages":[]}`)
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestCountTokensEstimate(t *testing.T) {
	ts, _ := newTestServer(t, "http://127.0.0.1:1", nil) // upstream never reached
	resp, body := post(t, ts.URL+"/v1/messages/count_tokens", testToken,
		`{"model":"fake-model","messages":[{"role":"user","content":"`+strings.Repeat("word ", 100)+`"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "input_tokens") {
		t.Errorf("body: %s", body)
	}
}
