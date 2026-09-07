package router

import (
	"context"
	"errors"
	"testing"

	"github.com/gremlord/gremlord/internal/config"
)

func testRule() config.RouteRule {
	return config.RouteRule{
		Classifier: "cheap",
		Default:    "standard",
		Tiers:      map[string]string{"deep": "opus", "standard": "sonnet", "light": "qwen"},
	}
}

func newAuto(answer string, err error, calls *int) *autoRouter {
	return &autoRouter{
		cache: map[string]decision{},
		classify: func(ctx context.Context, rule config.RouteRule, cfg *config.Config, summary string) (string, error) {
			*calls++
			return answer, err
		},
	}
}

func body(msgs string) []byte {
	return []byte(`{"model":"auto","max_tokens":100,"messages":[` + msgs + `]}`)
}

func TestAutoRouteClassifiesNewTurn(t *testing.T) {
	calls := 0
	a := newAuto("deep", nil, &calls)
	alias, tier, _ := a.route(context.Background(), testRule(), nil, body(`{"role":"user","content":"design the architecture for a big refactor"}`), "s1", nil)
	if alias != "opus" || tier != "deep" || calls != 1 {
		t.Errorf("alias=%s tier=%s calls=%d", alias, tier, calls)
	}
}

func TestAutoRouteSticksWithinTurn(t *testing.T) {
	calls := 0
	a := newAuto("deep", nil, &calls)
	newTurn := body(`{"role":"user","content":"plan the migration"}`)
	a.route(context.Background(), testRule(), nil, newTurn, "s1", nil)

	// Continuation: assistant tool_use answered by tool_result — no new user text.
	continuation := body(`{"role":"user","content":"plan the migration"},
	  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read_file","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"data"}]}`)
	alias, tier, _ := a.route(context.Background(), testRule(), nil, continuation, "s1", nil)
	if alias != "opus" || tier != "deep" {
		t.Errorf("continuation: alias=%s tier=%s", alias, tier)
	}
	if calls != 1 {
		t.Errorf("classifier ran %d times; continuations must reuse the decision", calls)
	}

	// A genuinely new user turn re-classifies.
	a.classify = func(ctx context.Context, rule config.RouteRule, cfg *config.Config, summary string) (string, error) {
		calls++
		return "light", nil
	}
	alias, tier, _ = a.route(context.Background(), testRule(), nil, body(`{"role":"user","content":"rename that variable"}`), "s1", nil)
	if alias != "qwen" || tier != "light" || calls != 2 {
		t.Errorf("new turn: alias=%s tier=%s calls=%d", alias, tier, calls)
	}
}

func TestAutoRouteTaskOverride(t *testing.T) {
	calls := 0
	rule := testRule()
	rule.Tasks = map[string]string{"implementation": "grok", "architecture": "fable"}
	a := newAuto("", nil, &calls)
	a.classifyTask = func(context.Context, config.RouteRule, *config.Config, string) (string, string, error) {
		calls++
		return "standard", "implementation", nil
	}
	alias, tier, reason := a.route(context.Background(), rule, nil, body(`{"role":"user","content":"implement the endpoint"}`), "tasks", nil)
	if alias != "grok" || tier != "standard" || reason != "task:implementation" || calls != 1 {
		t.Errorf("alias=%s tier=%s reason=%q calls=%d", alias, tier, reason, calls)
	}
}

func TestAutoRouteTaskSticksWithinTurn(t *testing.T) {
	calls := 0
	rule := testRule()
	rule.Tasks = map[string]string{"architecture": "fable"}
	a := newAuto("", nil, &calls)
	a.classifyTask = func(context.Context, config.RouteRule, *config.Config, string) (string, string, error) {
		calls++
		return "deep", "architecture", nil
	}
	a.route(context.Background(), rule, nil, body(`{"role":"user","content":"design the service"}`), "sticky-task", nil)
	continuation := body(`{"role":"user","content":"design the service"},
	  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"data"}]}`)
	alias, tier, reason := a.route(context.Background(), rule, nil, continuation, "sticky-task", nil)
	if alias != "fable" || tier != "deep" || reason != "task:architecture" || calls != 1 {
		t.Errorf("alias=%s tier=%s reason=%q calls=%d", alias, tier, reason, calls)
	}
}

func TestAutoRouteValidTaskWithInvalidTier(t *testing.T) {
	calls := 0
	rule := testRule()
	rule.Tasks = map[string]string{"security_review": "fable"}
	a := newAuto("", nil, &calls)
	a.classifyTask = func(context.Context, config.RouteRule, *config.Config, string) (string, string, error) {
		return "banana", "security_review", nil
	}
	alias, tier, reason := a.route(context.Background(), rule, nil, body(`{"role":"user","content":"review this auth flow"}`), "invalid-tier", nil)
	if alias != "fable" || tier != "standard" || reason != "task:security_review" {
		t.Errorf("alias=%s tier=%s reason=%q", alias, tier, reason)
	}
}

func TestAutoRouteTaskFailureDropsOverride(t *testing.T) {
	calls := 0
	rule := testRule()
	rule.Tasks = map[string]string{"implementation": "grok"}
	a := newAuto("", nil, &calls)
	a.classifyTask = func(context.Context, config.RouteRule, *config.Config, string) (string, string, error) {
		return "deep", "implementation", errors.New("classifier down")
	}
	alias, tier, reason := a.route(context.Background(), rule, nil, body(`{"role":"user","content":"implement it"}`), "task-failure", nil)
	if alias != "sonnet" || tier != "standard" || reason != "" {
		t.Errorf("alias=%s tier=%s reason=%q", alias, tier, reason)
	}
}

func TestAutoRouteTaskClassificationRunsWithOneEligibleTier(t *testing.T) {
	calls := 0
	rule := sizedRule()
	rule.Tasks = map[string]string{"security_review": "opus"}
	a := newAuto("", nil, &calls)
	a.classifyTask = func(context.Context, config.RouteRule, *config.Config, string) (string, string, error) {
		calls++
		return "deep", "security_review", nil
	}
	huge := []byte(`{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"` + repeat("w ", 80000) + `"}]}`)
	alias, tier, reason := a.route(context.Background(), rule, sizedCfg(), huge, "one-tier-task", nil)
	if alias != "opus" || tier != "deep" || reason != "task:security_review" || calls != 1 {
		t.Errorf("alias=%s tier=%s reason=%q calls=%d", alias, tier, reason, calls)
	}
}

func TestAutoRouteTaskSizeFallback(t *testing.T) {
	calls := 0
	rule := sizedRule()
	rule.Tasks = map[string]string{"implementation": "qwen"}
	a := newAuto("", nil, &calls)
	a.classifyTask = func(context.Context, config.RouteRule, *config.Config, string) (string, string, error) {
		return "standard", "implementation", nil
	}
	alias, tier, reason := a.route(context.Background(), rule, sizedCfg(), bigBody("implement "), "task-size", nil)
	if alias != "sonnet" || tier != "standard" || !contains(reason, "task:implementation:size-ineligible") {
		t.Errorf("alias=%s tier=%s reason=%q", alias, tier, reason)
	}
}

func TestAutoRouteContinuationPreservesOpeningHash(t *testing.T) {
	calls := 0
	a := newAuto("deep", nil, &calls)
	opening := body(`{"role":"user","content":"plan the migration"}`)
	a.route(context.Background(), testRule(), nil, opening, "retry", nil)
	continuation := body(`{"role":"user","content":"plan the migration"},
	  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"data"}]}`)
	a.route(context.Background(), testRule(), nil, continuation, "retry", nil)
	a.route(context.Background(), testRule(), nil, opening, "retry", nil)
	if calls != 1 {
		t.Errorf("opening-turn retry reclassified after continuation: calls=%d", calls)
	}
}

func TestAutoRouterResetCache(t *testing.T) {
	calls := 0
	a := newAuto("deep", nil, &calls)
	request := body(`{"role":"user","content":"plan it"}`)
	a.route(context.Background(), testRule(), nil, request, "reload", nil)
	a.resetCache()
	a.route(context.Background(), testRule(), nil, request, "reload", nil)
	if calls != 2 {
		t.Errorf("classifier calls=%d, want 2 after cache reset", calls)
	}
}

func TestAutoRouteFallsBackOnFailure(t *testing.T) {
	calls := 0
	a := newAuto("", errors.New("classifier down"), &calls)
	alias, tier, _ := a.route(context.Background(), testRule(), nil, body(`{"role":"user","content":"hello"}`), "s2", nil)
	if alias != "sonnet" || tier != "standard" {
		t.Errorf("fallback: alias=%s tier=%s", alias, tier)
	}

	// Garbage classifier answer also falls back.
	a = newAuto("banana", nil, &calls)
	alias, tier, _ = a.route(context.Background(), testRule(), nil, body(`{"role":"user","content":"hello"}`), "s3", nil)
	if alias != "sonnet" || tier != "standard" {
		t.Errorf("garbage answer: alias=%s tier=%s", alias, tier)
	}
}

func TestRoutingConfigValidation(t *testing.T) {
	yaml := `
providers:
  anthropic: {type: anthropic, base_url: https://api.anthropic.com, api_key_env: A}
models:
  opus:  {provider: anthropic, id: claude-opus-4-8}
  haiku: {provider: anthropic, id: claude-haiku-4-5}
routing:
  auto:
    classifier: haiku
    default: standard
    tiers: {deep: opus, standard: opus, light: haiku}
`
	if _, err := config.Parse([]byte(yaml)); err != nil {
		t.Fatalf("valid routing config rejected: %v", err)
	}

	bad := map[string]string{
		"unknown classifier": `
providers:
  anthropic: {type: anthropic, base_url: https://x, api_key_env: A}
models:
  opus: {provider: anthropic, id: claude-opus-4-8}
routing:
  auto: {classifier: ghost, tiers: {deep: opus}}
`,
		"unknown tier target": `
providers:
  anthropic: {type: anthropic, base_url: https://x, api_key_env: A}
models:
  opus: {provider: anthropic, id: claude-opus-4-8}
routing:
  auto: {classifier: opus, tiers: {deep: ghost}}
`,
		"collides with model alias": `
providers:
  anthropic: {type: anthropic, base_url: https://x, api_key_env: A}
models:
  opus: {provider: anthropic, id: claude-opus-4-8}
routing:
  opus: {classifier: opus, tiers: {deep: opus}}
`,
	}
	for name, y := range bad {
		if _, err := config.Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

// --- Phase 3: size-aware routing ---

func sizedRule() config.RouteRule {
	return config.RouteRule{
		Classifier: "cheap",
		Default:    "standard",
		Tiers:      map[string]string{"deep": "opus", "standard": "sonnet", "light": "qwen"},
	}
}

// bigBody builds a request whose estimated input exceeds the 8K "light" tier
// but fits the 32K "standard" tier.
func bigBody(msgs string) []byte {
	// ~30000 tokens of padding in the user message.
	padding := repeat("w ", 30000)
	return []byte(`{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"` + msgs + padding + `"}]}`)
}

func TestAutoRouteSizeRemapsUpFromLight(t *testing.T) {
	calls := 0
	a := newAuto("light", nil, &calls)
	alias, tier, reason := a.route(context.Background(), sizedRule(), sizedCfg(), bigBody("plan "), "s1", nil)
	if tier != "standard" || alias != "sonnet" {
		t.Errorf("remapped to tier=%s alias=%s, want standard/sonnet", tier, alias)
	}
	if reason == "" || !contains(reason, "light→standard") {
		t.Errorf("reason=%q should record light→standard", reason)
	}
}

func TestAutoRouteSizeRemapPicksSmallest(t *testing.T) {
	// Fits 32K (standard) and 128K (deep) but not 8K (light). Classifier picks
	// light; remap must choose standard (smallest fitting), not deep.
	calls := 0
	a := newAuto("light", nil, &calls)
	_, tier, _ := a.route(context.Background(), sizedRule(), sizedCfg(), bigBody("plan "), "s1", nil)
	if tier != "standard" {
		t.Errorf("remap = %s, want standard (smallest that fits)", tier)
	}
}

func TestAutoRouteSizeOnlyEligibleSkipsClassifier(t *testing.T) {
	// So large only opus (128K) fits → classifier must not run, opus is chosen directly.
	calls := 0
	a := newAuto("standard", nil, &calls) // would pick standard if called
	huge := []byte(`{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"` + repeat("w ", 80000) + `"}]}`)
	alias, tier, reason := a.route(context.Background(), sizedRule(), sizedCfg(), huge, "s1", nil)
	if tier != "deep" || alias != "opus" {
		t.Errorf("only-eligible = tier=%s alias=%s, want deep/opus", tier, alias)
	}
	if calls != 0 {
		t.Errorf("classifier ran %d times; with one eligible tier it must be skipped", calls)
	}
	if reason != "" {
		t.Errorf("reason=%q should be empty (no remap, just skipped classify)", reason)
	}
}

func TestAutoRouteStickyButTooBigRemaps(t *testing.T) {
	calls := 0
	a := newAuto("light", nil, &calls)
	// Turn 1: small request, classifier picks light (qwen 8K). Decision cached.
	small := body(`{"role":"user","content":"hi"}`)
	a.route(context.Background(), sizedRule(), sizedCfg(), small, "s1", nil)
	if calls != 1 {
		t.Fatalf("turn 1: calls=%d want 1", calls)
	}
	// Continuation of the same turn (no new user text) but grown past 8K.
	continuation := []byte(`{"model":"auto","max_tokens":100,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + repeat("w ", 30000) + `"}]}
	]}`)
	alias, tier, reason := a.route(context.Background(), sizedRule(), sizedCfg(), continuation, "s1", nil)
	if tier != "standard" || alias != "sonnet" {
		t.Errorf("sticky overflow: tier=%s alias=%s, want standard/sonnet", tier, alias)
	}
	if calls != 1 {
		t.Errorf("continuation must not re-classify: calls=%d want 1", calls)
	}
	if reason == "" || !contains(reason, "sticky") {
		t.Errorf("reason=%q should note sticky remap", reason)
	}
	// Cache was updated: a further continuation sticks to standard, not light.
	again := continuation
	alias, tier, _ = a.route(context.Background(), sizedRule(), sizedCfg(), again, "s1", nil)
	if tier != "standard" {
		t.Errorf("after sticky remap, further continuation tier=%s want standard", tier)
	}
}

func TestAutoRouteUnknownBudgetEligible(t *testing.T) {
	// deep tier has no context_window (infinite); others small. Oversized request
	// overflows standard+light, so it must route to the unknown-budget deep.
	cfg := sizedCfg()
	rule := config.RouteRule{
		Classifier: "cheap", Default: "standard",
		Tiers: map[string]string{"deep": "unknown", "standard": "sonnet", "light": "qwen"},
	}
	calls := 0
	a := newAuto("light", nil, &calls)
	huge := []byte(`{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"` + repeat("w ", 80000) + `"}]}`)
	alias, tier, _ := a.route(context.Background(), rule, cfg, huge, "s1", nil)
	if tier != "deep" || alias != "unknown" {
		t.Errorf("unknown-budget eligible: tier=%s alias=%s, want deep/unknown", tier, alias)
	}
}

func TestAutoRouteNoBudgetsNoFiltering(t *testing.T) {
	// No budgets anywhere → classifier answer honored unchanged (backward compat).
	cfg := &config.Config{
		Providers: map[string]config.Provider{"fake": {Type: config.ProviderOpenAI, BaseURL: "http://x"}},
		Models: map[string]config.Model{
			"opus": {Provider: "fake", ID: "opus-up"}, "sonnet": {Provider: "fake", ID: "sonnet-up"},
		},
	}
	rule := config.RouteRule{Classifier: "cheap", Default: "standard",
		Tiers: map[string]string{"deep": "opus", "standard": "sonnet"}}
	calls := 0
	a := newAuto("light", nil, &calls) // garbage tier (not in rule) → falls back to standard
	alias, tier, reason := a.route(context.Background(), rule, cfg, bigBody("hi"), "s1", nil)
	if tier != "standard" || alias != "sonnet" {
		t.Errorf("no-budgets: tier=%s alias=%s, want standard/sonnet (fallback)", tier, alias)
	}
	if reason != "" {
		t.Errorf("no-budgets reason=%q should be empty", reason)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
