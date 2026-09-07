package router

import (
	"math"
	"sort"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/tokens"
)

// defaultReservedOutput is the output headroom reserved when neither the
// request's max_tokens nor the model's MaxOutput constrains it. Sits in the
// router (not tokens) because it is a routing/dispatch policy, not an
// estimator property. Tunable: smaller = tighter fit (more remapping), larger
// = safer (more conservative).
const defaultReservedOutput = 8192

// reservedOutput returns the output headroom to reserve when testing whether
// a request fits a context budget. Prefers the request's own max_tokens
// (what the model will actually try to produce), caps it against the model's
// MaxOutput, and falls back to defaultReservedOutput when neither applies.
func reservedOutput(reqMaxTokens, modelMaxOutput int) int {
	r := reqMaxTokens
	if modelMaxOutput > 0 && (r == 0 || r > modelMaxOutput) {
		r = modelMaxOutput
	}
	if r <= 0 {
		return defaultReservedOutput
	}
	return r
}

// tierBudget resolves the context budget for a tier's model alias. Returns 0
// when the budget is unknown (the tier is then treated as having infinite
// capacity) or the alias fails to resolve (defensive — config validation
// should prevent that).
func tierBudget(cfg *config.Config, alias string) int {
	if cfg == nil {
		return 0
	}
	r, err := cfg.Resolve(alias)
	if err != nil {
		return 0
	}
	return r.Model.ContextBudget()
}

// tierMaxRequestBytes resolves the request-body byte cap for a tier's provider.
// Returns 0 when unknown (no cap) or the alias fails to resolve.
func tierMaxRequestBytes(cfg *config.Config, alias string) int {
	if cfg == nil {
		return 0
	}
	r, err := cfg.Resolve(alias)
	if err != nil {
		return 0
	}
	return r.Provider.MaxRequestBytes
}

// fitDecision summarizes the size-aware filtering of a route rule's tiers.
type fitDecision struct {
	Eligible  map[string]bool // tier name -> fits (budget/bytes unknown => true)
	Required  int64           // uncalibrated estimate + reserved output; 0 when sizing is inactive
	EstInput  int64           // the raw estimate, or 0 when no tier had a known budget
	Reserved  int64           // output headroom, shared by every tier
	BodyBytes int64           // raw request body size, for byte-cap filtering
	Filtered  []string        // tiers excluded because they don't fit (sorted)

	// calib corrects EstInput per model before it is compared to a
	// budget. Held here rather than applied up front because the
	// correction is a property of the model being tested, not of the
	// request: the same conversation is a different number of tokens to
	// a Claude tokenizer than to a Qwen one.
	calib tokens.Calibration
}

// requiredFor is the token requirement to test alias against: its
// calibrated view of the input, plus the shared output headroom.
func (f fitDecision) requiredFor(cfg *config.Config, alias string) int64 {
	if cfg == nil {
		return f.Required
	}
	r, err := cfg.Resolve(alias)
	if err != nil {
		return f.Required
	}
	return f.calib.Apply(r.Model.ID, f.EstInput) + f.Reserved
}

// aliasFits reports whether a model alias (a tier's target or a task's
// target — anything resolvable via config) can hold this request,
// considering only limits that are actually known (0/unset never filters).
func (f fitDecision) aliasFits(cfg *config.Config, alias string) bool {
	if b := tierBudget(cfg, alias); b > 0 && f.requiredFor(cfg, alias) > int64(b) {
		return false
	}
	if mb := tierMaxRequestBytes(cfg, alias); mb > 0 && f.BodyBytes > int64(mb) {
		return false
	}
	return true
}

// classifyTierFit computes which tiers can hold the request. A tier is eligible
// only if it fits BOTH the token budget and the provider's request-byte cap
// (where either is known). When no tier — nor any task-mapped alias, see
// below — has any known limit, it returns every tier eligible with
// Required==0 — the backward-compat fast path.
//
// Task-mapped aliases (rule.Tasks) are folded into the "is sizing active at
// all" and "shared output cap" computations even though Eligible is keyed by
// tier only: a task target's own eligibility is checked separately (see
// fitDecision.aliasFits, used by autoRouter.applyTask) against the same
// estimate, so a task-only budget (no tier declares one) still gets a real
// estimate to check against instead of always reading as "unknown budget,
// always eligible".
func classifyTierFit(cfg *config.Config, rule config.RouteRule, req *anthropic.MessagesRequest, bodyBytes int64, calib tokens.Calibration) fitDecision {
	elig := map[string]bool{}
	for t := range rule.Tiers {
		elig[t] = true
	}

	anyTokenKnown := false
	anyByteKnown := false
	checkAlias := func(alias string) {
		if tierBudget(cfg, alias) > 0 {
			anyTokenKnown = true
		}
		if tierMaxRequestBytes(cfg, alias) > 0 {
			anyByteKnown = true
		}
	}
	for _, alias := range rule.Tiers {
		checkAlias(alias)
	}
	for _, alias := range rule.Tasks {
		checkAlias(alias)
	}
	if !anyTokenKnown && !anyByteKnown {
		return fitDecision{Eligible: elig, BodyBytes: bodyBytes, calib: calib}
	}

	est := tokens.Estimate(req)
	// Shared output cap: the largest MaxOutput among capability tiers. Task
	// targets are checked after classification and must not make unrelated tier
	// targets ineligible merely because a specialist model supports more output.
	maxCap := 0
	updateCap := func(alias string) {
		if m, ok := cfg.Models[alias]; ok && m.MaxOutput > maxCap {
			maxCap = m.MaxOutput
		}
	}
	for _, alias := range rule.Tiers {
		updateCap(alias)
	}
	reqMax := 0
	if req != nil {
		reqMax = req.MaxTokens
	}
	reserved := int64(reservedOutput(reqMax, maxCap))
	fit := fitDecision{
		Eligible:  elig,
		Required:  est + reserved,
		EstInput:  est,
		Reserved:  reserved,
		BodyBytes: bodyBytes,
		calib:     calib,
	}

	var filtered []string
	for tier, alias := range rule.Tiers {
		fits := fit.aliasFits(cfg, alias)
		elig[tier] = fits
		if !fits {
			filtered = append(filtered, tier)
		}
	}
	sort.Strings(filtered)
	fit.Filtered = filtered
	return fit
}

// onlyEligibleTier returns the single eligible tier when exactly one remains,
// and ok=true. Used to skip the classifier call when size filtering has left
// only one viable tier. Inactive when no filtering is active (Required==0 and
// no byte filtering occurred).
func onlyEligibleTier(fit fitDecision) (tier string, ok bool) {
	if fit.Required == 0 && len(fit.Filtered) == 0 {
		return "", false // filtering inactive
	}
	for t, ok2 := range fit.Eligible {
		if ok2 {
			if ok {
				return "", false // more than one eligible
			}
			tier, ok = t, true
		}
	}
	return tier, ok
}

// remapTier takes a classifier-chosen tier that was filtered out by fit and
// returns the smallest-budget tier that still fits. Tiers with an unknown
// budget (infinite) are preferred over overflow but not chosen as "smallest
// that fits" when a known-budget tier fits. If nothing fits, returns the
// largest-budget tier (best-effort) so the request still dispatches — the
// dispatch guard then returns a clean error if it truly can't fit. A tier
// that is already eligible is returned unchanged.
func remapTier(cfg *config.Config, rule config.RouteRule, fit fitDecision, chosen string) string {
	if fit.Eligible[chosen] {
		return chosen
	}
	type cand struct {
		tier   string
		budget int
	}
	var known []cand
	var unknown []string
	for tier := range fit.Eligible {
		if !fit.Eligible[tier] {
			continue
		}
		b := tierBudget(cfg, rule.Tiers[tier])
		if b > 0 {
			known = append(known, cand{tier, b})
		} else {
			unknown = append(unknown, tier)
		}
	}
	// Smallest known budget that fits.
	if len(known) > 0 {
		sort.Slice(known, func(i, j int) bool { return known[i].budget < known[j].budget })
		return known[0].tier
	}
	// Nothing known fits — prefer an unknown-budget (infinite) tier.
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return unknown[0]
	}
	// Nothing fits at all — largest known budget, best-effort.
	var all []cand
	for tier, alias := range rule.Tiers {
		if b := tierBudget(cfg, alias); b > 0 {
			all = append(all, cand{tier, b})
		}
	}
	if len(all) == 0 {
		return chosen // no known budgets anywhere; nothing to remap to
	}
	sort.Slice(all, func(i, j int) bool { return all[i].budget > all[j].budget })
	return all[0].tier
}

// promptTooLong reports whether the resolved model has a known context budget
// that the estimated input (plus reserved output headroom) exceeds. Returns
// false when the budget is unknown (no guard) or the request fits. required is
// the estimated input + reserved output, budget the model's context budget.
func promptTooLong(route config.Resolved, req *anthropic.MessagesRequest, calib tokens.Calibration) (overflow bool, required int64, budget int) {
	budget = route.Model.ContextBudget()
	if budget <= 0 {
		return false, 0, 0
	}
	reqMax := 0
	if req != nil {
		reqMax = req.MaxTokens
	}
	required = calib.Apply(route.Model.ID, tokens.Estimate(req)) + int64(reservedOutput(reqMax, route.Model.MaxOutput))
	return required > int64(budget), required, budget
}

// bodyTooLarge reports whether the resolved model's provider has a known
// request-byte cap that the raw body exceeds. Returns false when the cap is
// unknown (no guard). Used as a pre-dispatch guard so an oversized body —
// common with accumulated images/attachments — is refused with a clean 400
// instead of a mangled upstream 413 retry loop.
func bodyTooLarge(route config.Resolved, bodyBytes int64) (tooLarge bool, size int64, cap int) {
	cap = route.Provider.MaxRequestBytes
	if cap <= 0 {
		return false, 0, 0
	}
	return bodyBytes > int64(cap), bodyBytes, cap
}

// requestFits reports whether a resolved model can serve a request of the
// given token requirement and body size. False when either the token budget or
// the byte cap (where known) is exceeded. Used by the dispatch guard to cover
// both limits in one check.
func requestFits(route config.Resolved, req *anthropic.MessagesRequest, bodyBytes int64, calib tokens.Calibration) bool {
	if overflow, _, _ := promptTooLong(route, req, calib); overflow {
		return false
	}
	if tooLarge, _, _ := bodyTooLarge(route, bodyBytes); tooLarge {
		return false
	}
	return true
}

// budgetSortKey orders budgets ascending with unknown (0) treated as +Inf,
// so known budgets sort first and unknown last.
func budgetSortKey(b int) int {
	if b <= 0 {
		return math.MaxInt
	}
	return b
}
