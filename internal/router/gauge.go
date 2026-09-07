package router

import (
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/tokens"
)

// gaugeBudget resolves the one context budget a dynamically-routed
// session's gauge is scaled against, for the whole session.
//
// Scaling per resolved model (the original behavior, still available as
// context_gauge: model) makes the client's gauge mean something different
// on every turn: the same conversation reads 30% full on a 1M model and
// 95% on a 200K one. Claude Code compacts on whatever the last turn
// reported, so a single light-tier turn can trigger a compaction the big
// model never needed — the effective window of the whole session collapses
// to the smallest tier it happens to touch.
//
// Anchoring the gauge to one budget for the rule fixes that. The default
// anchor is the largest budget the rule can route to, because the router
// already has the machinery to make it safe: classifyTierFit filters out
// tiers that cannot hold the request and remapTier moves the turn up to
// one that can, so growing past a small tier's window costs a remap, not a
// failure. Rules that would rather keep every tier reachable at any
// conversation length can anchor to the smallest budget instead.
//
// Returns 0 when the rule opts out (context_gauge: model), meaning
// "scale against whichever model actually served this request".
func gaugeBudget(cfg *config.Config, rule config.RouteRule) int {
	if cfg == nil || rule.ContextGauge == config.GaugeModel {
		return 0
	}
	wantMin := rule.ContextGauge == config.GaugeMin
	out := 0
	consider := func(alias string) {
		r, err := cfg.Resolve(alias)
		if err != nil {
			return // defensive: config validation rejects unresolvable targets
		}
		// An undeclared window is treated as the client's assumed window
		// rather than skipped: "unknown" already behaves as 200K today
		// (factor 1), and dropping it from the comparison would let one
		// unsized tier silently pull the anchor to an unrelated model.
		b := tokens.BudgetOrAssumed(r.Model.ContextBudget())
		switch {
		case out == 0, wantMin && b < out, !wantMin && b > out:
			out = b
		}
	}
	for _, alias := range rule.Tiers {
		consider(alias)
	}
	// Task targets are routable too, so they anchor the gauge alongside
	// tiers — otherwise a task model with a different window reintroduces
	// exactly the jump this exists to remove.
	for _, alias := range rule.Tasks {
		consider(alias)
	}
	return out
}
