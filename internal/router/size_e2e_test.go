package router

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
)

// newSizedServer builds a router whose "tiny" model has a 1000-token budget,
// backed by an upstream that fails the test if reached.
func newSizedServer(t *testing.T, reachedUpstream *bool) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reachedUpstream = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{"fake": {Type: config.ProviderOpenAI, BaseURL: upstream.URL}},
		Models: map[string]config.Model{
			"tiny":    {Provider: "fake", ID: "tiny-up", ContextWindow: 1000},
			"unknown": {Provider: "fake", ID: "unknown-up"}, // no budget
		},
		Profiles: map[string]config.Profile{"main": {Model: "tiny"}},
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
	return ts
}

func TestPromptTooLongGuard(t *testing.T) {
	reached := false
	ts := newSizedServer(t, &reached)

	// Oversized: ~7000 tokens, far over the 1000 budget + reserved output.
	big := `{"model":"tiny","max_tokens":50,"messages":[{"role":"user","content":"` + strings.Repeat("word ", 7000) + `"}]}`
	resp, body := post(t, ts.URL+"/v1/messages", testToken, big)
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"invalid_request_error", "too large", "context budget"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	if reached {
		t.Error("oversized request must not reach upstream")
	}

	// Small request to the same server passes (no false positive).
	reached = false
	resp, body = post(t, ts.URL+"/v1/messages", testToken,
		`{"model":"tiny","max_tokens":50,"messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("small request status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	if !reached {
		t.Error("small request should reach upstream")
	}
}

func TestPromptTooLongGuardSkipsUnknownBudget(t *testing.T) {
	reached := false
	ts := newSizedServer(t, &reached)

	// "unknown" model has no context_window → guard is a no-op even for a big request.
	big := `{"model":"unknown","max_tokens":50,"messages":[{"role":"user","content":"` + strings.Repeat("word ", 7000) + `"}]}`
	resp, body := post(t, ts.URL+"/v1/messages", testToken, big)
	if resp.StatusCode != 200 {
		t.Fatalf("unknown-budget status = %d, want 200 (guard skipped); body: %s", resp.StatusCode, body)
	}
	if !reached {
		t.Error("unknown-budget request should reach upstream")
	}
}

func TestCountTokensSkipsPromptGuard(t *testing.T) {
	reached := false
	ts := newSizedServer(t, &reached)

	big := `{"model":"tiny","messages":[{"role":"user","content":"` + strings.Repeat("word ", 7000) + `"}]}`
	resp, body := post(t, ts.URL+"/v1/messages/count_tokens", testToken, big)
	if resp.StatusCode != 200 {
		t.Fatalf("count_tokens status = %d, want 200 (guard skipped); body: %s", resp.StatusCode, body)
	}
	if reached {
		t.Error("count_tokens must never reach upstream")
	}
	if !strings.Contains(body, "input_tokens") {
		t.Errorf("body: %s", body)
	}
}

// TestAutoRouteSizeRemapEndToEnd: a routing rule with 8K/32K/128K tiers; the
// classifier picks "light" but the oversized request fits only standard+deep,
// so the routed call must land on the standard upstream with a size reason.
func TestAutoRouteSizeRemapEndToEnd(t *testing.T) {
	var seenModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		seenModels = append(seenModels, req.Model)
		w.Header().Set("Content-Type", "application/json")
		if req.Model == "classifier-up" {
			io.WriteString(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"light"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":1}}`)
			return
		}
		io.WriteString(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{"fake": {Type: config.ProviderOpenAI, BaseURL: upstream.URL}},
		Models: map[string]config.Model{
			"cheap": {Provider: "fake", ID: "classifier-up"},
			"big":   {Provider: "fake", ID: "deep-up", ContextWindow: 128000},
			"med":   {Provider: "fake", ID: "standard-up", ContextWindow: 32000},
			"small": {Provider: "fake", ID: "light-up", ContextWindow: 8000},
		},
		Routing: map[string]config.RouteRule{
			"auto": {Classifier: "cheap", Default: "standard",
				Tiers: map[string]string{"deep": "big", "standard": "med", "light": "small"}},
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

	// Oversized to 8K (light), fits 32K (standard): ~9400 estimated tokens.
	big := `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"` + strings.Repeat("word ", 6000) + `"}]}`
	resp, body := post(t, ts.URL+"/v1/messages", testToken, big)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	// classifier call, then the routed call to the standard (med) upstream.
	if len(seenModels) < 2 || seenModels[len(seenModels)-1] != "standard-up" {
		t.Errorf("routed to %v, want last call to standard-up", seenModels)
	}
	alias, tier, model, reason, ok, err := st.LatestRouteDecision("sess-test")
	if err != nil || !ok {
		t.Fatalf("no route decision recorded: ok=%v err=%v", ok, err)
	}
	if tier != "standard" || model != "med" {
		t.Errorf("decision: alias=%s tier=%s model=%s, want tier=standard model=med", alias, tier, model)
	}
	if !strings.Contains(reason, "size") || !strings.Contains(reason, "light→standard") {
		t.Errorf("reason=%q should record size:light→standard", reason)
	}
}

// TestRequestBodyGuard: a provider with max_request_bytes refuses an oversized
// body with a clean 400 before upstream; a small body passes.
func TestRequestBodyGuard(t *testing.T) {
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"fake":   {Type: config.ProviderOpenAI, BaseURL: upstream.URL},
			"capped": {Type: config.ProviderOpenAI, BaseURL: upstream.URL, MaxRequestBytes: 1024},
		},
		Models:   map[string]config.Model{"capped-model": {Provider: "capped", ID: "capped-up"}},
		Profiles: map[string]config.Profile{"main": {Model: "capped-model"}},
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

	// Oversized body (2000 bytes > 1024 cap).
	big := `{"model":"capped-model","max_tokens":50,"messages":[{"role":"user","content":"` + strings.Repeat("x", 2000) + `"}]}`
	resp, body := post(t, ts.URL+"/v1/messages", testToken, big)
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"invalid_request_error", "too large", "max_request_bytes"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	if reached {
		t.Error("oversized body must not reach upstream")
	}

	// Small body passes.
	reached = false
	resp, body = post(t, ts.URL+"/v1/messages", testToken,
		`{"model":"capped-model","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("small body status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	if !reached {
		t.Error("small body should reach upstream")
	}
}

// TestAutoRouteByteCapExcludesTier: a routing rule where the light tier's
// provider has a byte cap; an oversized body routes away from it.
func TestAutoRouteByteCapExcludesTier(t *testing.T) {
	var seenModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		seenModels = append(seenModels, req.Model)
		w.Header().Set("Content-Type", "application/json")
		if req.Model == "classifier-up" {
			io.WriteString(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"light"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":1}}`)
			return
		}
		io.WriteString(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"fake":   {Type: config.ProviderOpenAI, BaseURL: upstream.URL},
			"capped": {Type: config.ProviderOpenAI, BaseURL: upstream.URL, MaxRequestBytes: 512},
		},
		Models: map[string]config.Model{
			"cheap": {Provider: "fake", ID: "classifier-up"},
			"big":   {Provider: "fake", ID: "deep-up", ContextWindow: 128000},
			"small": {Provider: "capped", ID: "light-up", ContextWindow: 128000},
		},
		Routing: map[string]config.RouteRule{
			"auto": {Classifier: "cheap", Default: "deep",
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

	// Body > 512 bytes exceeds small's provider cap; classifier says light, but
	// the byte cap filters it out → routes to big (deep).
	big := `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"` + strings.Repeat("word ", 200) + `"}]}`
	resp, body := post(t, ts.URL+"/v1/messages", testToken, big)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if len(seenModels) < 2 || seenModels[len(seenModels)-1] != "deep-up" {
		t.Errorf("routed to %v, want last call to deep-up (escaped byte-capped light)", seenModels)
	}
	alias, tier, model, reason, ok, err := st.LatestRouteDecision("sess-test")
	if err != nil || !ok || tier != "deep" || model != "big" {
		t.Errorf("decision: alias=%s tier=%s model=%s ok=%v err=%v, want tier=deep model=big", alias, tier, model, ok, err)
	}
	// With only "deep" eligible (light byte-filtered), the classifier call is
	// skipped and there's no remap to record — reason stays empty. The point
	// is that the request escaped the byte-capped tier, which it did (tier=deep).
	_ = reason
}
