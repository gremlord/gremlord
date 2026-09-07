package router

import (
	"testing"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/tokens"
)

func gaugeCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"anthropic": {Type: config.ProviderAnthropic, BaseURL: "https://api.anthropic.com"},
			"local":     {Type: config.ProviderOpenAI, BaseURL: "http://localhost:8000/v1"},
		},
		Models: map[string]config.Model{
			"opus":  {Provider: "anthropic", ID: "claude-opus-5"},
			"big":   {Provider: "local", ID: "big-1", ContextWindow: 1_000_000},
			"small": {Provider: "local", ID: "small-1", ContextWindow: 32_768},
			"task":  {Provider: "local", ID: "task-1", ContextWindow: 400_000},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestGaugeBudget(t *testing.T) {
	cfg := gaugeCfg(t)
	rule := func(gauge string, tasks map[string]string) config.RouteRule {
		return config.RouteRule{
			Classifier:   "small",
			Tiers:        map[string]string{"deep": "big", "standard": "small"},
			Tasks:        tasks,
			ContextGauge: gauge,
		}
	}

	if got := gaugeBudget(cfg, rule("", nil)); got != 1_000_000 {
		t.Errorf("default gauge = %d, want the largest tier budget 1000000", got)
	}
	if got := gaugeBudget(cfg, rule(config.GaugeMin, nil)); got != 32_768 {
		t.Errorf("min gauge = %d, want the smallest tier budget 32768", got)
	}
	// Opting out means "scale against whichever model served the turn",
	// which the backends spell as budget 0.
	if got := gaugeBudget(cfg, rule(config.GaugeModel, nil)); got != 0 {
		t.Errorf("model gauge = %d, want 0", got)
	}
	// Task targets are routable, so they anchor the gauge too.
	if got := gaugeBudget(cfg, rule(config.GaugeMin, map[string]string{"sql_data": "task"})); got != 32_768 {
		t.Errorf("task target changed the min anchor: %d", got)
	}
	if got := gaugeBudget(cfg, config.RouteRule{Tiers: map[string]string{"standard": "small"},
		Tasks: map[string]string{"sql_data": "task"}}); got != 400_000 {
		t.Errorf("task target ignored by max anchor: %d, want 400000", got)
	}
}

// An undeclared window is the client's assumed window, not "skip me":
// an all-Anthropic rule must keep scaling by exactly 1 as it always has.
func TestGaugeBudgetUndeclaredWindow(t *testing.T) {
	cfg := gaugeCfg(t)
	allAnthropic := config.RouteRule{Tiers: map[string]string{"deep": "opus", "standard": "opus"}}
	if got := gaugeBudget(cfg, allAnthropic); got != tokens.AssumedWindow {
		t.Errorf("gauge = %d, want AssumedWindow %d", got, tokens.AssumedWindow)
	}
	if f := tokens.ScaleFactor(gaugeBudget(cfg, allAnthropic)); f != 1 {
		t.Errorf("all-anthropic rule must not scale, factor = %v", f)
	}
	mixed := config.RouteRule{Tiers: map[string]string{"deep": "opus", "standard": "small"}}
	if got := gaugeBudget(cfg, mixed); got != tokens.AssumedWindow {
		t.Errorf("mixed rule gauge = %d, want the 200K anthropic anchor", got)
	}
}

// Anchoring the gauge high does not change what an undeclared window
// means: unsized stays unconstrained. Guessing that an undeclared model
// is a 200K one would be wrong for exactly the models most likely to be
// left unsized — a Claude 5 tier holds far more than that. A model whose
// window really is small says so with context_window, which is now
// accepted on anthropic providers too.
func TestUnsizedTiersStayUnconstrainedUnderAnyAnchor(t *testing.T) {
	cfg := gaugeCfg(t)
	mixed := config.RouteRule{Tiers: map[string]string{"deep": "big", "standard": "opus"}}
	if got := gaugeBudget(cfg, mixed); got != 1_000_000 {
		t.Fatalf("anchor = %d, want the 1M tier", got)
	}
	fit := classifyTierFit(cfg, mixed, reqWith(repeat("word ", 220_000)), 0, nil)
	if fit.EstInput < 250_000 {
		t.Fatalf("test request too small to exercise the case: est=%d", fit.EstInput)
	}
	if !fit.Eligible["standard"] {
		t.Error("an unsized tier must stay eligible; declare context_window to bound it")
	}

	// Declaring the window is what bounds it.
	sized := cfg.Models["opus"]
	sized.ContextWindow = 200_000
	cfg.Models["opus"] = sized
	fit = classifyTierFit(cfg, mixed, reqWith(repeat("word ", 220_000)), 0, nil)
	if fit.Eligible["standard"] {
		t.Error("a declared 200K tier should be filtered out of a 300K request")
	}
	if got := remapTier(cfg, mixed, fit, "standard"); got != "deep" {
		t.Errorf("remapTier = %q, want deep", got)
	}
}
