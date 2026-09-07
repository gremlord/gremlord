package pricing

import (
	"testing"

	"github.com/gremlord/gremlord/internal/config"
)

func TestEmbeddedPrices(t *testing.T) {
	tbl := Load(t.TempDir(), nil)
	// 1M in + 1M out on sonnet-5 = $3 + $15.
	cost, priced := tbl.Cost("claude-sonnet-5", 1_000_000, 1_000_000, 0, 0)
	if !priced || cost != 18.0 {
		t.Errorf("cost = %v priced=%v, want 18.0 true", cost, priced)
	}
	// Cache tokens priced separately.
	cost, _ = tbl.Cost("claude-haiku-4-5", 0, 0, 1_000_000, 1_000_000)
	if cost != 0.1+1.25 {
		t.Errorf("cache cost = %v", cost)
	}
	// Unknown model = unpriced.
	if _, priced := tbl.Cost("gpt-5.2", 1000, 1000, 0, 0); priced {
		t.Error("unknown model should be unpriced")
	}
}

func TestConfigOverrides(t *testing.T) {
	cfg := &config.Config{
		Pricing: map[string]config.Price{
			"claude-sonnet-5": {Input: 1, Output: 2},
		},
		Models: map[string]config.Model{
			"qwen": {Provider: "local", ID: "qwen3-coder-30b", Pricing: &config.Price{}},
		},
	}
	tbl := Load(t.TempDir(), cfg)
	cost, _ := tbl.Cost("claude-sonnet-5", 1_000_000, 1_000_000, 0, 0)
	if cost != 3.0 {
		t.Errorf("override cost = %v, want 3.0", cost)
	}
	// Zero-price local model counts as priced (not "untracked").
	cost, priced := tbl.Cost("qwen3-coder-30b", 1_000_000, 0, 0, 0)
	if !priced || cost != 0 {
		t.Errorf("local model: cost=%v priced=%v", cost, priced)
	}
}
