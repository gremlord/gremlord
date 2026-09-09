// Package router serves the Anthropic Messages API surface Claude Code
// talks to, dispatching per-model to provider backends.
package router

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/backend/anthropicbe"
	"github.com/gremlord/gremlord/internal/backend/clibe"
	"github.com/gremlord/gremlord/internal/backend/openaibe"
	"github.com/gremlord/gremlord/internal/budget"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/pricing"
	"github.com/gremlord/gremlord/internal/store"
	"github.com/gremlord/gremlord/internal/tokens"
	"github.com/gremlord/gremlord/internal/wire"
)

type Server struct {
	cfg     atomic.Pointer[config.Config]
	pricing atomic.Pointer[pricing.Table]
	token   string
	dataDir string
	store   *store.Store
	anth    *anthropicbe.Backend
	oai     *openaibe.Backend
	cli     *clibe.Backend
	gate    *budget.Gate
	calib   *calibrator
	auto    *autoRouter
	goal    *goalRouter
	log     *slog.Logger
}

func NewServer(cfg *config.Config, token, dataDir string, st *store.Store, logger *slog.Logger) *Server {
	s := &Server{
		token: token, dataDir: dataDir, store: st,
		anth: anthropicbe.New(), oai: openaibe.New(), cli: clibe.New(), log: logger,
	}
	s.cfg.Store(cfg)
	s.pricing.Store(pricing.Load(dataDir, cfg))
	s.gate = budget.NewGate(cfg, st, logger)
	s.calib = newCalibrator(st, logger)
	s.auto = &autoRouter{classify: s.classifyViaBackend, classifyTask: s.classifyTaskViaBackend, cache: map[string]decision{}, log: logger}
	s.goal = &goalRouter{classify: s.classifyGoalViaBackend}
	return s
}

// Reload re-reads the config file; called by the reload path so config CLI
// edits apply to live sessions without restart.
func (s *Server) Reload() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	s.cfg.Store(cfg)
	s.pricing.Store(pricing.Load(s.dataDir, cfg))
	s.gate.SetConfig(cfg)
	// A routing-rule edit (tiers, tasks, classifier) must apply to the very
	// next request, not be masked by a sticky decision cached under the
	// previous config.
	s.auto.resetCache()
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+wire.PathHealth, s.handleHealth)
	mux.HandleFunc("POST "+wire.PathReload, s.auth(s.handleReload))
	// Pre-rename paths, so an gremlord CLI can still reach this router.
	mux.HandleFunc("GET "+wire.LegacyPathHealth, s.handleHealth)
	mux.HandleFunc("POST "+wire.LegacyPathReload, s.auth(s.handleReload))
	mux.HandleFunc("POST /v1/messages", s.auth(s.handleMessages(false)))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.auth(s.handleMessages(true)))
	// Catch-all: unknown /v1/* endpoints go to the default anthropic
	// provider so new Claude Code calls keep working.
	mux.HandleFunc("/v1/", s.auth(s.handleCatchAll))
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": Version})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := s.Reload(); err != nil {
		anthropic.WriteError(w, 500, "api_error", "reload failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Claude Code sends BOTH headers when a /login managed key and
		// ANTHROPIC_AUTH_TOKEN are present: x-api-key carries the managed key
		// and Authorization carries our local token. Accept the local token
		// from either header rather than trusting only the first one found.
		apiKey := r.Header.Get("x-api-key")
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		apiKeyOK := apiKey != "" && subtle.ConstantTimeCompare([]byte(apiKey), []byte(s.token)) == 1
		bearerOK := bearer != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(s.token)) == 1
		if !apiKeyOK && !bearerOK {
			anthropic.WriteError(w, 401, "authentication_error",
				"gremlord router: invalid local token (launch sessions via `gremlord`)")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleMessages(countTokens bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			anthropic.WriteError(w, 400, "invalid_request_error", "gremlord: reading body: "+err.Error())
			return
		}
		env, err := anthropic.ParseEnvelope(raw)
		if err != nil || env.Model == "" {
			anthropic.WriteError(w, 400, "invalid_request_error", "gremlord: request body is not a Messages API request")
			return
		}
		cfg := s.cfg.Load()
		calib := s.calib.get()
		sessionID := wire.Session(r.Header)
		// gauge is the budget this session's client-facing token counts
		// are scaled against — one budget per routing rule, so the
		// context gauge does not change meaning when the tier does. 0
		// for a directly-addressed alias or a pinned session: those
		// scale against the model they name.
		gauge := 0
		pinModel := wire.PinModel(r.Header)
		resolveAlias := env.Model
		if pinModel != "" {
			// A pinned session (eval candidates, pin_tiers profiles) must
			// never escape to a different model — not even via dynamic
			// `auto` routing. This is the router-side backstop for the
			// env-var pinning done at launch: if Claude Code's own
			// subagent-spawn logic (or a bug, or a future SDK default)
			// requests a different model, remap it back here rather than
			// silently letting the request reach a different provider.
			if resolveAlias != pinModel {
				s.log.Info("pin_remap", "session", sessionID, "requested", resolveAlias, "pinned", pinModel)
				resolveAlias = pinModel
			}
		} else if rule, ok := cfg.Routing[env.Model]; ok {
			chosen, tier, reason := s.auto.route(r.Context(), rule, cfg, raw, sessionID, calib)
			gauge = gaugeBudget(cfg, rule)
			s.log.Info("autoroute", "alias", env.Model, "tier", tier, "model", chosen, "reason", reason)
			resolveAlias = chosen
			if sessionID != "" {
				if err := s.store.RecordRouteDecision(sessionID, env.Model, tier, chosen, reason, time.Now()); err != nil {
					s.log.Warn("route decision insert failed", "err", err)
				}
				worthy, reason, isNewTurn := s.goal.check(r.Context(), rule, cfg, raw, sessionID)
				if worthy {
					if injected, err := injectGoalReminder(raw, reason); err == nil {
						raw = injected
					} else {
						s.log.Warn("goal reminder injection failed", "err", err)
					}
					s.log.Info("autogoal", "session", sessionID, "reason", reason)
				}
				// Continuations (tool results) never re-classify, so they
				// must not clobber a decision recorded when the turn opened.
				if isNewTurn {
					if err := s.store.RecordGoalDecision(sessionID, worthy, reason, time.Now()); err != nil {
						s.log.Warn("goal decision insert failed", "err", err)
					}
				}
			}
		}
		route, err := cfg.Resolve(resolveAlias)
		if err != nil {
			anthropic.WriteError(w, 404, "not_found_error", "gremlord: "+err.Error()+" (see ~/.gremlord/config.yaml)")
			return
		}

		// Dispatch-time prompt-too-long guard: for a model with a known
		// context budget, refuse requests the budget can't hold before they
		// reach upstream and fail with a mangled, provider-specific error.
		// 400 (not 413) so Claude Code doesn't retry-spin — same rationale as
		// the budget gate below. count_tokens never dispatches, so skip it.
		var comp tokens.Composition
		if !countTokens {
			if req, perr := anthropic.ParseRequest(raw); perr == nil {
				comp = tokens.Compose(req)
				if overflow, required, budget := promptTooLong(route, req, calib); overflow {
					msg := fmt.Sprintf("gremlord: request too large for model %q context budget "+
						"(estimated %d + reserved output exceeds budget %d); "+
						"reduce the conversation or switch models",
						route.Model.ID, required, budget)
					anthropic.WriteError(w, 400, "invalid_request_error", msg)
					s.log.Info("prompt_too_long",
						"model", route.Model.ID, "alias", resolveAlias,
						"estimated_input", required, "budget", budget)
					return
				}
			}
			// Request body byte cap (e.g. an upstream nginx limit). Refuse
			// oversized bodies — common with accumulated images/attachments —
			// before dispatch, instead of a mangled upstream 413 retry loop.
			if tooLarge, size, cap := bodyTooLarge(route, int64(len(raw))); tooLarge {
				msg := fmt.Sprintf("gremlord: request body too large for provider %q "+
					"(%d bytes exceeds max_request_bytes %d); "+
					"run /compact or remove images/attachments",
					route.ProviderName, size, cap)
				anthropic.WriteError(w, 400, "invalid_request_error", msg)
				s.log.Info("request_too_large",
					"model", route.Model.ID, "alias", resolveAlias,
					"bytes", size, "max_request_bytes", cap)
				return
			}
		}

		profile := wire.Profile(r.Header)
		if !countTokens && route.Provider.Type != config.ProviderCLI {
			// CLI delegation spends against the peer CLI's subscription, outside
			// gremlord's pricing data. A $0 API budget must not block capacity it
			// neither meters nor pays for.
			if msg := s.gate.Check(profile); msg != "" {
				// 400 deliberately — 429/5xx would make Claude Code retry-spin;
				// a 400 surfaces the message verbatim in the TUI.
				anthropic.WriteError(w, 400, "invalid_request_error", msg)
				return
			}
		}

		call := &backend.Call{
			Raw: raw, Envelope: env, Route: route,
			Header: r.Header, Query: r.URL.Query(),
			GaugeBudget: gauge, Calibration: calib,
		}
		var be backend.Backend
		switch route.Provider.Type {
		case config.ProviderAnthropic:
			be = s.anth
		case config.ProviderOpenAI:
			be = s.oai
		case config.ProviderCLI:
			be = s.cli
		default:
			anthropic.WriteError(w, 501, "api_error",
				fmt.Sprintf("gremlord: provider type %q not implemented (model %q)", route.Provider.Type, env.Model))
			return
		}
		var res backend.Result
		if countTokens {
			res = be.CountTokens(r.Context(), call, w)
		} else {
			res = be.Messages(r.Context(), call, w)
		}

		if !countTokens {
			s.recordUsage(r, route, env.Model, res, time.Since(start), comp, gauge)
		}
	}
}

func (s *Server) recordUsage(r *http.Request, route config.Resolved, alias string, res backend.Result, dur time.Duration, comp tokens.Composition, gauge int) {
	u := res.Usage
	if u == (anthropic.Usage{}) && res.ErrType == "" {
		return
	}
	cost, priced := s.pricing.Load().Cost(route.Model.ID,
		u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
	if route.Provider.Type == config.ProviderCLI {
		// Even when the optional CLI model ID happens to match an entry in the
		// API price table, subscription spend is opaque to gremlord.
		cost, priced = 0, false
	}
	budget := route.Model.ContextBudget()
	ev := store.UsageEvent{
		TS:               time.Now(),
		SessionID:        wire.Session(r.Header),
		Profile:          wire.Profile(r.Header),
		Provider:         route.ProviderName,
		Model:            route.Model.ID,
		Alias:            alias,
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		CostUSD:          cost,
		Priced:           priced,
		RequestID:        r.Header.Get("request-id"),
		Status:           res.Status,
		ErrType:          res.ErrType,
		CtxBudget:        budget,
		ReportedInput:    res.ReportedInput,
		DurationMS:       dur.Milliseconds(),
		EstInput:         comp.Total(),
		EstSystem:        comp.System,
		EstTools:         comp.Tools,
		GaugeBudget:      gauge,
	}
	if err := s.store.RecordUsage(ev); err != nil {
		s.log.Warn("usage insert failed", "err", err)
	}
	s.gate.Add(ev.Profile, cost)
	if res.Status >= 400 && res.ErrMsg != "" {
		s.log.Warn("upstream error",
			"model", alias, "upstream", route.Model.ID, "status", res.Status, "err", res.ErrMsg)
	}
	attrs := []any{
		"model", alias, "upstream", route.Model.ID, "status", res.Status,
		"in", u.InputTokens, "out", u.OutputTokens,
		"cache_read", u.CacheReadInputTokens, "cost_usd", fmt.Sprintf("%.4f", cost),
		"ms", dur.Milliseconds(),
	}
	if budget > 0 {
		trueIn := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		attrs = append(attrs,
			"ctx_budget", budget,
			"ctx_reported", res.ReportedInput,
			"ctx_pct", fmt.Sprintf("%.1f", 100*float64(trueIn)/float64(budget)))
	}
	if gauge > 0 && gauge != budget {
		attrs = append(attrs, "ctx_gauge", gauge)
	}
	if comp.Total() > 0 {
		// The fixed tax: system prompt + tool schemas are re-sent on every
		// request whatever the turn is about.
		attrs = append(attrs, "est_in", comp.Total(),
			"est_fixed", comp.System+comp.Tools, "est_tools", comp.Tools)
	}
	s.log.Info("request", attrs...)
}

// handleCatchAll forwards unrecognized /v1/* calls to the default
// anthropic provider unmodified.
func (s *Server) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	p, ok := cfg.Providers[config.ProviderAnthropic]
	if !ok {
		anthropic.WriteError(w, 404, "not_found_error", "gremlord: no anthropic provider configured for "+r.URL.Path)
		return
	}
	u := strings.TrimSuffix(p.BaseURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, r.Body)
	if err != nil {
		anthropic.WriteError(w, 500, "api_error", "gremlord: "+err.Error())
		return
	}
	req.Header = r.Header.Clone()
	req.Header.Set("x-api-key", p.Key())
	req.Header.Del("Authorization")
	resp, err := s.anthClient().Do(req)
	if err != nil {
		anthropic.WriteError(w, 500, "api_error", "anthropic upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}

var catchAllClient = &http.Client{Transport: backend.NewTransport()}

func (s *Server) anthClient() *http.Client { return catchAllClient }

func flushCopy(w http.ResponseWriter, r io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
