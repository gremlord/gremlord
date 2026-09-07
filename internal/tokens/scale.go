package tokens

import (
	"math"

	"github.com/gremlord/gremlord/internal/anthropic"
)

// AssumedWindow is the context window Claude Code believes it has. Claude
// Code sizes its auto-compact threshold against the model id it was
// launched with (a claude-* name → ~200K); it cannot be told the real
// window of a routed model. Instead the router scales every token count it
// reports so that the routed model's real budget maps onto this assumed
// window: at 100% of the real budget the client sees 100% of 200K and
// compacts. Works in both directions — small models compact early, 1M
// models compact late.
const AssumedWindow = 200_000

// BudgetOrAssumed maps an unknown (0) budget onto AssumedWindow. A model
// with no declared window is scaled by factor 1, which is precisely the
// same thing as declaring a budget equal to the client's assumed window —
// making the two comparable when picking one budget for a whole session.
func BudgetOrAssumed(budget int) int {
	if budget <= 0 {
		return AssumedWindow
	}
	return budget
}

// ScaleFactor returns the multiplier applied to client-facing token counts
// for a model with the given context budget. budget <= 0 means unknown →
// no scaling.
func ScaleFactor(budget int) float64 {
	if budget <= 0 {
		return 1
	}
	return float64(AssumedWindow) / float64(budget)
}

// ScaleCount scales one token count, rounding up so the bias-high property
// of Estimate survives scaling.
func ScaleCount(n int64, factor float64) int64 {
	if factor == 1 || n <= 0 {
		return n
	}
	return int64(math.Ceil(float64(n) * factor))
}

// ScaleUsage scales the input-side fields of a usage block — the ones
// Claude Code sums into its context gauge. Output tokens are left true:
// they roll into the next request's input count anyway, and scaling them
// would distort client-side per-message displays for no gauge benefit.
func ScaleUsage(u anthropic.Usage, factor float64) anthropic.Usage {
	u.InputTokens = ScaleCount(u.InputTokens, factor)
	u.CacheReadInputTokens = ScaleCount(u.CacheReadInputTokens, factor)
	u.CacheCreationInputTokens = ScaleCount(u.CacheCreationInputTokens, factor)
	return u
}
