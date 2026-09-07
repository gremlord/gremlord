package router

import (
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
)

func sizedCfg() *config.Config {
	mk := func(id string, budget, maxOut int) config.Model {
		m := config.Model{Provider: "fake", ID: id}
		m.ContextWindow = budget
		m.MaxOutput = maxOut
		return m
	}
	return &config.Config{
		Providers: map[string]config.Provider{
			"fake": {Type: config.ProviderOpenAI, BaseURL: "http://x"},
		},
		Models: map[string]config.Model{
			"opus":    mk("opus-up", 128000, 0),
			"sonnet":  mk("sonnet-up", 32000, 0),
			"qwen":    mk("qwen-up", 8000, 0),
			"tiny":    mk("tiny-up", 1000, 0),
			"unknown": {Provider: "fake", ID: "unknown-up"}, // no context_window
		},
	}
}

func reqWith(text string) *anthropic.MessagesRequest {
	return &anthropic.MessagesRequest{
		MaxTokens: 100,
		Messages:  []anthropic.Message{{Role: "user", Content: anthropic.MessageBody{{Type: "text", Text: text}}}},
	}
}

func TestReservedOutput(t *testing.T) {
	cases := []struct{ req, cap, want int }{
		{0, 0, defaultReservedOutput}, // nothing set
		{100, 0, 100},                 // request max_tokens wins
		{50000, 8000, 8000},           // capped by model MaxOutput
		{4000, 8000, 4000},            // request below cap, used as-is
		{0, 8000, 8000},               // no request, use cap
	}
	for _, c := range cases {
		if got := reservedOutput(c.req, c.cap); got != c.want {
			t.Errorf("reservedOutput(%d,%d)=%d want %d", c.req, c.cap, got, c.want)
		}
	}
}

func TestClassifyTierFitNoBudgetsShortCircuits(t *testing.T) {
	// No tiers have a known budget → Required==0, all eligible, no estimate.
	cfg := &config.Config{
		Providers: map[string]config.Provider{"fake": {Type: config.ProviderOpenAI, BaseURL: "http://x"}},
		Models: map[string]config.Model{
			"opus":   {Provider: "fake", ID: "opus-up"},
			"sonnet": {Provider: "fake", ID: "sonnet-up"},
		},
	}
	rule := config.RouteRule{Tiers: map[string]string{"deep": "opus", "standard": "sonnet"}}
	fit := classifyTierFit(cfg, rule, reqWith("hello"), 0, nil)
	if fit.Required != 0 || fit.EstInput != 0 {
		t.Errorf("expected short-circuit, got Required=%d EstInput=%d", fit.Required, fit.EstInput)
	}
	if !fit.Eligible["deep"] || !fit.Eligible["standard"] {
		t.Errorf("all tiers should be eligible, got %v", fit.Eligible)
	}
}

func TestClassifyTierFitFilters(t *testing.T) {
	cfg := sizedCfg()
	rule := config.RouteRule{Tiers: map[string]string{"deep": "opus", "standard": "sonnet", "light": "qwen"}}
	// ~30000 tokens (60000 chars / 3.5 → ~17143, +10% margin ~18857, +reserved) → exceeds qwen's 8K, fits sonnet's 32K and opus's 128K.
	fit := classifyTierFit(cfg, rule, reqWith(repeat("w ", 30000)), 0, nil)
	if fit.Required == 0 {
		t.Fatal("expected an estimate")
	}
	if fit.Eligible["light"] {
		t.Error("qwen (8K) should be filtered out")
	}
	if !fit.Eligible["standard"] || !fit.Eligible["deep"] {
		t.Error("sonnet and opus should remain eligible")
	}
	if len(fit.Filtered) != 1 || fit.Filtered[0] != "light" {
		t.Errorf("Filtered=%v want [light]", fit.Filtered)
	}
}

func TestTaskMaxOutputDoesNotInflateTierRequirement(t *testing.T) {
	cfg := sizedCfg()
	largeTask := cfg.Models["tiny"]
	largeTask.MaxOutput = 100000
	largeTask.ContextWindow = 200000
	cfg.Models["large-task"] = largeTask
	rule := config.RouteRule{
		Tiers: map[string]string{"deep": "opus", "standard": "sonnet", "light": "qwen"},
		Tasks: map[string]string{"implementation": "large-task"},
	}
	fit := classifyTierFit(cfg, rule, reqWith("hello"), 0, nil)
	if !fit.Eligible["light"] || fit.Required >= 8000 {
		t.Errorf("task output cap inflated tier requirement: required=%d eligible=%v", fit.Required, fit.Eligible)
	}
}

func TestRemapTierSmallestFitting(t *testing.T) {
	cfg := sizedCfg()
	rule := config.RouteRule{Tiers: map[string]string{"deep": "opus", "standard": "sonnet", "light": "qwen"}}
	// qwen excluded; remap from light → sonnet (smallest that fits), not opus.
	fit := fitDecision{
		Eligible: map[string]bool{"deep": true, "standard": true, "light": false},
		Required: 10000,
	}
	if got := remapTier(cfg, rule, fit, "light"); got != "standard" {
		t.Errorf("remap light = %s, want standard (smallest fitting)", got)
	}
}

func TestRemapTierAlreadyEligible(t *testing.T) {
	cfg := sizedCfg()
	rule := config.RouteRule{Tiers: map[string]string{"deep": "opus", "standard": "sonnet"}}
	fit := fitDecision{Eligible: map[string]bool{"deep": true, "standard": true}, Required: 100}
	if got := remapTier(cfg, rule, fit, "deep"); got != "deep" {
		t.Errorf("eligible tier must pass through, got %s", got)
	}
}

func TestRemapTierNothingFitsFallsBackToLargest(t *testing.T) {
	cfg := sizedCfg()
	rule := config.RouteRule{Tiers: map[string]string{"deep": "opus", "standard": "sonnet", "light": "qwen"}}
	// Nothing fits: all eligible=false. remap returns the largest known budget (opus).
	fit := fitDecision{
		Eligible: map[string]bool{"deep": false, "standard": false, "light": false},
		Required: 999999,
	}
	if got := remapTier(cfg, rule, fit, "light"); got != "deep" {
		t.Errorf("nothing fits: remap = %s, want deep (largest budget)", got)
	}
}

func TestRemapTierPrefersUnknownBudgetWhenNothingKnownFits(t *testing.T) {
	cfg := sizedCfg()
	rule := config.RouteRule{Tiers: map[string]string{"deep": "unknown", "light": "qwen"}}
	// qwen (8K) doesn't fit; deep is unknown-budget (infinite). Prefer deep.
	fit := fitDecision{
		Eligible: map[string]bool{"deep": true, "light": false},
		Required: 999999,
	}
	if got := remapTier(cfg, rule, fit, "light"); got != "deep" {
		t.Errorf("should prefer unknown-budget tier, got %s", got)
	}
}

func TestPromptTooLong(t *testing.T) {
	cfg := sizedCfg()
	route, _ := cfg.Resolve("tiny") // 1000 budget

	// Small request fits.
	if overflow, _, _ := promptTooLong(route, reqWith("hi"), nil); overflow {
		t.Error("small request should not overflow")
	}
	// Large request overflows.
	overflow, required, budget := promptTooLong(route, reqWith(repeat("w ", 10000)), nil)
	if !overflow {
		t.Errorf("large request should overflow, required=%d budget=%d", required, budget)
	}
	if budget != 1000 {
		t.Errorf("budget=%d want 1000", budget)
	}
}

func TestPromptTooLongUnknownBudgetNoGuard(t *testing.T) {
	cfg := sizedCfg()
	route, _ := cfg.Resolve("unknown") // no context_window
	if overflow, _, _ := promptTooLong(route, reqWith(repeat("w ", 100000)), nil); overflow {
		t.Error("unknown budget must never trip the guard")
	}
}

func repeat(s string, n int) string { return strings.Repeat(s, n) }

// --- byte-size guard ---

func byteCfg() *config.Config {
	return &config.Config{
		Providers: map[string]config.Provider{
			// "fake" has no byte cap; "capped" caps bodies at 1024 bytes.
			"fake":   {Type: config.ProviderOpenAI, BaseURL: "http://x"},
			"capped": {Type: config.ProviderOpenAI, BaseURL: "http://x", MaxRequestBytes: 1024},
		},
		Models: map[string]config.Model{
			"big":   {Provider: "fake", ID: "big-up", ContextWindow: 128000},
			"small": {Provider: "capped", ID: "small-up", ContextWindow: 128000},
		},
	}
}

func TestBodyTooLarge(t *testing.T) {
	cfg := byteCfg()
	route, _ := cfg.Resolve("small") // capped provider, 1024-byte cap
	if tooLarge, _, _ := bodyTooLarge(route, 500); tooLarge {
		t.Error("500 bytes should fit under 1024 cap")
	}
	if tooLarge, size, cap := bodyTooLarge(route, 2000); !tooLarge {
		t.Errorf("2000 bytes should overflow 1024 cap (size=%d cap=%d)", size, cap)
	}
	if cap := route.Provider.MaxRequestBytes; cap != 1024 {
		t.Errorf("cap=%d want 1024", cap)
	}
}

func TestBodyTooLargeUnknownCapNoGuard(t *testing.T) {
	cfg := byteCfg()
	route, _ := cfg.Resolve("big") // uncapped provider
	if tooLarge, _, _ := bodyTooLarge(route, 10_000_000); tooLarge {
		t.Error("unknown cap must never trip the guard")
	}
}

func TestClassifyTierFitFiltersByBytes(t *testing.T) {
	cfg := byteCfg()
	rule := config.RouteRule{Tiers: map[string]string{"deep": "big", "light": "small"}}
	// Body of 2000 bytes exceeds small's 1024-byte cap but fits big. Both
	// tiers have the same 128K token budget, so only the byte cap differentiates.
	fit := classifyTierFit(cfg, rule, reqWith("hi"), 2000, nil)
	if fit.Eligible["light"] {
		t.Error("small (1024-byte cap) should be filtered out by a 2000-byte body")
	}
	if !fit.Eligible["deep"] {
		t.Error("big (uncapped) should remain eligible")
	}
	if len(fit.Filtered) != 1 || fit.Filtered[0] != "light" {
		t.Errorf("Filtered=%v want [light]", fit.Filtered)
	}
}

func TestRemapTierEscapesByteCap(t *testing.T) {
	cfg := byteCfg()
	rule := config.RouteRule{Tiers: map[string]string{"deep": "big", "light": "small"}}
	// Classifier picked light, but its 1024-byte cap is exceeded → remap to big.
	fit := fitDecision{
		Eligible:  map[string]bool{"deep": true, "light": false},
		Required:  100,
		BodyBytes: 2000,
	}
	if got := remapTier(cfg, rule, fit, "light"); got != "deep" {
		t.Errorf("byte overflow remap = %s, want deep", got)
	}
}
