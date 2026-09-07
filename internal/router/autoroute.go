package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/tokens"
)

// autoRouter implements dynamic tier routing: a cheap classifier model
// assesses each new user turn and picks deep/standard/light; the decision
// sticks for the rest of the turn (tool_result continuations) so a task
// doesn't flip models mid-flight.
type autoRouter struct {
	classify func(ctx context.Context, rule config.RouteRule, cfg *config.Config, summary string) (string, error)
	// classifyTask is used instead of classify when the rule is task-aware
	// (len(rule.Tasks) > 0): one combined request returns both tier and
	// task, so task-aware routing never costs a second classifier call.
	classifyTask func(ctx context.Context, rule config.RouteRule, cfg *config.Config, summary string) (tier, task string, err error)
	log          *slog.Logger

	mu    sync.Mutex
	cache map[string]decision // key: session id (or user-text hash when unattributed)
}

type decision struct {
	userHash string
	tier     string
	// task is the classified task label ("" when the rule isn't task-aware,
	// classification didn't run this turn, or the classifier's answer
	// wasn't a recognized label). Retained across sticky continuations
	// alongside tier so a task override survives mid-turn tool_result
	// round-trips the same way the tier does.
	task string
	at   time.Time
}

// resetCache clears sticky per-session decisions. Called on config reload
// (Server.Reload) so a routing-rule edit — tiers, tasks, classifier —
// applies to the very next request instead of being masked by a sticky
// decision cached under the previous config.
func (a *autoRouter) resetCache() {
	a.mu.Lock()
	a.cache = map[string]decision{}
	a.mu.Unlock()
}

const classifierPrompt = `You route requests inside a coding agent to a model tier. Reply with exactly one word: deep, standard, or light.

deep: planning, architecture, debugging hard problems, multi-step reasoning, ambiguous or high-stakes decisions
standard: writing or modifying code, ordinary multi-tool tasks
light: mechanical edits, renames, formatting, summaries, verifying provided output, short factual answers

Request to classify:
`

// route picks the concrete model alias for a dynamically-routed request. The
// returned reason is a free-text note for observability — non-empty when
// size-aware routing remapped the classifier's choice (e.g.
// "size:light→standard") and/or a task label was classified and applied
// (e.g. "task:security_review"); empty for a plain classifier decision on a
// rule with no Tasks configured. When rule.Tasks is empty this function's
// behavior is byte-for-byte identical to before task-aware routing existed:
// the task branch below is only ever reached with task=="", which is a
// documented no-op in applyTask.
func (a *autoRouter) route(ctx context.Context, rule config.RouteRule, cfg *config.Config, raw []byte, sessionID string, calib tokens.Calibration) (alias, tier, reason string) {
	fallback := rule.Default
	if fallback == "" {
		fallback = "standard"
	}
	if _, ok := rule.Tiers[fallback]; !ok {
		for t := range rule.Tiers {
			fallback = t
			break
		}
	}

	req, err := anthropic.ParseRequest(raw)
	if err != nil {
		return rule.Tiers[fallback], fallback, ""
	}

	// Size-aware fit: which tiers can hold this request? Required==0 and no
	// byte caps means no tier has a known limit — the backward-compat fast
	// path (no estimate, no filtering).
	fit := classifyTierFit(cfg, rule, req, int64(len(raw)), calib)

	userText, isNewTurn := lastUserText(req)
	hash := hashText(userText)
	key := sessionID
	if key == "" {
		key = hash
	}

	a.mu.Lock()
	prev, hasPrev := a.cache[key]
	a.mu.Unlock()

	// Continuations (tool results coming back) and retries of the same
	// turn keep the tier (and task) that opened the turn.
	if hasPrev && (!isNewTurn || prev.userHash == hash) {
		if _, ok := rule.Tiers[prev.tier]; ok {
			tier = prev.tier
			var sizeReason string
			if !fit.Eligible[tier] {
				// The sticky tier can't hold this (larger) continuation.
				// Remap upward without re-classifying, and pin the cache so
				// the rest of the turn stays on the remapped tier.
				remapped := remapTier(cfg, rule, fit, tier)
				a.logFit(rule, fit, tier, remapped, "sticky")
				sizeReason = "size:sticky:" + tier + "→" + remapped
				tier = remapped
			}
			var taskReason string
			alias, taskReason = a.applyTask(cfg, rule, fit, tier, prev.task)
			// A tool-result continuation has no user text, so preserve the
			// opening turn's hash. This keeps retries of the opening request
			// sticky after the continuation has updated the cached tier.
			cachedHash := hash
			if !isNewTurn {
				cachedHash = prev.userHash
			}
			a.cacheDecision(key, cachedHash, tier, prev.task)
			return alias, tier, combineReason(taskReason, sizeReason)
		}
	}
	if userText == "" {
		return rule.Tiers[fallback], fallback, ""
	}

	// A tier-only rule can skip classification when size filtering leaves one
	// viable tier. Task-aware rules still classify once because the task may
	// select a different model that also fits the request.
	if t, ok := onlyEligibleTier(fit); ok && len(rule.Tasks) == 0 {
		a.logFit(rule, fit, "", t, "only")
		a.cacheDecision(key, hash, t, "")
		return rule.Tiers[t], t, ""
	}

	summary := fmt.Sprintf("(conversation: %d messages, %d tools available)\n%s",
		len(req.Messages), len(req.Tools), truncate(userText, 2000))
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var task string
	if len(rule.Tasks) > 0 {
		// Task-aware rule: one combined classifier request returns both
		// tier and task — never a second call.
		tier, task, err = a.classifyTask(cctx, rule, cfg, summary)
	} else {
		tier, err = a.classify(cctx, rule, cfg, summary)
	}
	if err != nil {
		tier = fallback
		task = ""
	} else if rule.Tiers[tier] == "" {
		// A valid task remains useful even when the classifier's tier token is
		// malformed: the concrete task target comes from trusted local config.
		// With no valid task, this simply fails open to the configured tier.
		tier = fallback
	}

	// The classifier has no notion of size; if it picked a tier the request
	// won't fit, remap upward to the smallest tier that does.
	var sizeReason string
	if fit.Required > 0 && !fit.Eligible[tier] {
		chosen := tier
		tier = remapTier(cfg, rule, fit, chosen)
		a.logFit(rule, fit, chosen, tier, "remap")
		sizeReason = "size:" + chosen + "→" + tier
	}

	var taskReason string
	alias, taskReason = a.applyTask(cfg, rule, fit, tier, task)
	reason = combineReason(taskReason, sizeReason)

	a.cacheDecision(key, hash, tier, task)
	return alias, tier, reason
}

// applyTask resolves a classified task label to its rule.Tasks-mapped model
// alias, overriding the tier's own alias when the mapping exists and the
// mapped model can hold the request (aliasFits, the same size-aware check
// tiers use). When the task is empty (no task classified, or the rule isn't
// task-aware), unmapped, or size-ineligible, it falls back to the tier's
// alias — which by this point has already been remapped through the
// eligible tiers by the caller, so a size-ineligible task target still
// resolves to *some* capable model rather than an oversized one.
//
// task=="" is a pure no-op (returns tierAlias, ""): this is what keeps a
// rule with no Tasks configured byte-for-byte identical to tier-only
// routing, since task is never anything but "" in that case.
func (a *autoRouter) applyTask(cfg *config.Config, rule config.RouteRule, fit fitDecision, tier, task string) (alias, reason string) {
	tierAlias := rule.Tiers[tier]
	if task == "" {
		return tierAlias, ""
	}
	candidate, mapped := rule.Tasks[task]
	if !mapped || candidate == "" {
		return tierAlias, ""
	}
	if fit.Required == 0 || fit.aliasFits(cfg, candidate) {
		return candidate, "task:" + task
	}
	a.logFit(rule, fit, task, tier, "task-size-ineligible")
	return tierAlias, "task:" + task + ":size-ineligible"
}

// combineReason joins non-empty routing-reason components ("task:...",
// "size:...") for observability, task first since task selection is the
// conceptually primary decision and size just describes whether the
// underlying tier had to move.
func combineReason(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, ";")
}

// cacheDecision stores a tier(+task) decision under key, flushing the cache
// when it exceeds 1000 entries.
func (a *autoRouter) cacheDecision(key, hash, tier, task string) {
	a.mu.Lock()
	if len(a.cache) > 1000 {
		a.cache = map[string]decision{}
	}
	a.cache[key] = decision{userHash: hash, tier: tier, task: task, at: time.Now()}
	a.mu.Unlock()
}

// logFit emits a Debug-level autoroute_size event when size filtering is
// active. from is the classifier-chosen (or sticky) tier; to is the tier
// actually used. kind is "sticky" | "only" | "remap".
func (a *autoRouter) logFit(rule config.RouteRule, fit fitDecision, from, to, kind string) {
	if a.log == nil {
		return
	}
	a.log.Debug("autoroute_size",
		"estimated_input", fit.EstInput,
		"required", fit.Required,
		"excluded", fit.Filtered,
		"from", from,
		"to", to,
		"kind", kind,
	)
}

// lastUserText returns the newest user-authored text and whether the last
// message is a fresh user turn (vs a tool_result continuation).
func lastUserText(req *anthropic.MessagesRequest) (string, bool) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if msg.Role != "user" {
			continue
		}
		text, hasToolResult := "", false
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				if text != "" {
					text += "\n"
				}
				text += b.Text
			case "tool_result":
				hasToolResult = true
			}
		}
		if text != "" {
			return text, i == len(req.Messages)-1 && !hasToolResult
		}
		if hasToolResult {
			return "", false // continuation; keep scanning is pointless — turn already classified
		}
	}
	return "", false
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// classifyViaBackend runs the classifier request through the router's own
// backends and parses the one-word tier answer.
func (s *Server) classifyViaBackend(ctx context.Context, rule config.RouteRule, cfg *config.Config, summary string) (string, error) {
	resp, err := s.runClassifier(ctx, rule, cfg, classifierPrompt+summary, 8)
	if err != nil {
		return "", err
	}
	for _, block := range resp.Content {
		if block.Type == "text" {
			word := strings.ToLower(strings.TrimSpace(block.Text))
			word = strings.Trim(word, "`.\"' \n")
			return word, nil
		}
	}
	return "", fmt.Errorf("classifier returned no text")
}

// runClassifier sends a single-turn prompt to a classifier model alias
// through the router's own backends (no network hop out and back through
// the local port) and returns the parsed Anthropic-shaped response. Shared
// by any classification pass (tier routing, goal detection, ...).
func (s *Server) runClassifier(ctx context.Context, rule config.RouteRule, cfg *config.Config, prompt string, maxTokens int) (*anthropic.MessagesResponse, error) {
	route, err := cfg.Resolve(rule.Classifier)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"model":      rule.Classifier,
		"max_tokens": maxTokens,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return nil, err
	}
	env, _ := anthropic.ParseEnvelope(body)
	call := &backend.Call{Raw: body, Envelope: env, Route: route, Header: http.Header{}, Query: nil}

	rec := newMemWriter()
	var be backend.Backend
	switch route.Provider.Type {
	case config.ProviderAnthropic:
		be = s.anth
	case config.ProviderOpenAI:
		be = s.oai
	default:
		// Deliberately narrower than handleMessages' dispatch: config
		// validation forbids cli-type aliases as classifiers, so this
		// error is the backstop, not a missing case.
		return nil, fmt.Errorf("classifier provider type %q unsupported", route.Provider.Type)
	}
	res := be.Messages(ctx, call, rec)
	if res.Status != 200 {
		return nil, fmt.Errorf("classifier request failed: %d %s", res.Status, res.ErrType)
	}
	var resp anthropic.MessagesResponse
	if err := json.Unmarshal(rec.buf.Bytes(), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// memWriter is an in-memory http.ResponseWriter for internal requests.
type memWriter struct {
	header http.Header
	buf    bytes.Buffer
	status int
}

func newMemWriter() *memWriter { return &memWriter{header: http.Header{}, status: 200} }

func (m *memWriter) Header() http.Header         { return m.header }
func (m *memWriter) WriteHeader(code int)        { m.status = code }
func (m *memWriter) Write(p []byte) (int, error) { return m.buf.Write(p) }
