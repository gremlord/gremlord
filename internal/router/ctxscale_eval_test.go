package router

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
	"github.com/gremlord/gremlord/internal/tokens"
)

// compactAt is the simulated auto-compact trigger, as a fraction of the
// window the client believes it has. (Live measurement, July 2026: Claude
// Code triggers at ~100%; its warning line sits near 92%. The invariants
// below are threshold-agnostic — proportionality holds at any trigger.)
const compactAt = 0.92

// TestContextScalingEval is the simulation eval for virtual context
// scaling. It plays a growing Claude-Code-like conversation against the
// real router (count_tokens + messages paths) for a range of model budgets
// and asserts the two invariants the feature exists for:
//
//  1. proportionality — the fraction of the assumed 200K window the client
//     sees always equals the fraction of the REAL budget consumed (never
//     under-reported), so the client's compact trigger fires at the same
//     relative fullness on every model;
//  2. safety — at the simulated compact trigger, real usage is past the
//     trigger fraction but within one turn-increment of the real budget,
//     i.e. compaction happens neither absurdly early nor after the window
//     is already blown.
func TestContextScalingEval(t *testing.T) {
	cases := []struct {
		name      string
		window    int
		effective int
	}{
		{"tiny-8k", 8_000, 0},
		{"local-32k", 32_000, 0},
		{"mid-64k", 64_000, 0},
		{"gpt-128k", 128_000, 0},
		{"parity-200k", 200_000, 0},
		{"huge-1m", 1_000_000, 0},
		{"attention-limited", 200_000, 60_000}, // 200K window, degrades past 60K
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget := tc.effective
			if budget == 0 {
				budget = tc.window
			}
			ts, st := newScalingServer(t, tc.window, tc.effective)

			// Grow the conversation in ~budget/10 chunks until the client's
			// gauge says compact. 15 turns is enough to cross 92% with slack.
			turnTokens := budget / 10
			turnText := strings.Repeat("w ", turnTokens*7/4) // ≈ turnTokens at 3.5 chars/token, pre-margin
			var msgs []map[string]any
			compacted := false
			for turn := 0; turn < 15; turn++ {
				msgs = append(msgs,
					map[string]any{"role": "user", "content": fmt.Sprintf("turn %d: %s", turn, turnText)},
					map[string]any{"role": "assistant", "content": "ok"},
				)
				body := mustJSON(t, map[string]any{"model": "sized", "max_tokens": 100, "messages": msgs})

				reported := countTokens(t, ts.URL, body)
				trueTokens := unscaledEstimate(t, body)

				reportedFrac := float64(reported) / float64(tokens.AssumedWindow)
				trueFrac := float64(trueTokens) / float64(budget)
				if reportedFrac < trueFrac || reportedFrac > trueFrac+0.001 {
					t.Fatalf("turn %d: proportionality broken: reported %.4f of assumed window, true %.4f of budget",
						turn, reportedFrac, trueFrac)
				}

				if reportedFrac >= compactAt {
					// One turn-increment of overshoot is inherent (the gauge
					// is checked between turns); more means scaling is off.
					maxOvershoot := float64(turnTokens)/float64(budget) + 0.02
					if trueFrac < compactAt || trueFrac > compactAt+maxOvershoot {
						t.Fatalf("compact fired at %.1f%% of real budget (want %.0f%%–%.0f%%)",
							100*trueFrac, 100*compactAt, 100*(compactAt+maxOvershoot))
					}
					compacted = true
					break
				}
			}
			if !compacted {
				t.Fatal("client gauge never reached the compact trigger — scaling under-reports")
			}

			// Messages path: upstream bills 50% of the real budget; the
			// client must see 50% of the assumed window, and the store must
			// keep the TRUE number plus the scaling metadata.
			upstreamPrompt := int64(budget / 2)
			respUsage := postMessages(t, ts.URL, upstreamPrompt)
			wantReported := tokens.ScaleCount(upstreamPrompt, tokens.ScaleFactor(budget))
			if respUsage.InputTokens != wantReported {
				t.Errorf("client-visible input_tokens = %d, want %d", respUsage.InputTokens, wantReported)
			}
			if respUsage.OutputTokens != 7 {
				t.Errorf("output_tokens = %d, want 7 (never scaled)", respUsage.OutputTokens)
			}

			traj, err := st.ContextTrajectory("sess-test")
			if err != nil || len(traj) == 0 {
				t.Fatalf("trajectory: %v err=%v", traj, err)
			}
			last := traj[len(traj)-1]
			if last.TrueInput != upstreamPrompt {
				t.Errorf("store true input = %d, want %d (pricing must never see scaled numbers)", last.TrueInput, upstreamPrompt)
			}
			if last.ReportedInput != wantReported || last.CtxBudget != budget {
				t.Errorf("store reported=%d budget=%d, want %d/%d", last.ReportedInput, last.CtxBudget, wantReported, budget)
			}
		})
	}
}

// newScalingServer builds a router whose single model has the given context
// sizing, backed by a fake OpenAI upstream that bills whatever
// prompt_tokens a "bill me N" message in the conversation asks for.
func newScalingServer(t *testing.T, window, effective int) (*httptest.Server, *store.Store) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(body, &req)
		// The last user message smuggles the prompt_tokens the fake should bill.
		var billed int64 = 10
		for _, m := range req.Messages {
			if _, err := fmt.Sscanf(m.Content, "bill me %d", &billed); err == nil {
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":7}}`, billed)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"fake": {Type: config.ProviderOpenAI, BaseURL: upstream.URL},
		},
		Models: map[string]config.Model{
			"sized": {Provider: "fake", ID: "sized-upstream", ContextWindow: window, EffectiveContext: effective},
		},
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
	return ts, st
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func countTokens(t *testing.T, baseURL, body string) int64 {
	t.Helper()
	resp, raw := post(t, baseURL+"/v1/messages/count_tokens", testToken, body)
	if resp.StatusCode != 200 {
		t.Fatalf("count_tokens status %d: %s", resp.StatusCode, raw)
	}
	var out anthropic.CountTokensResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out.InputTokens
}

// unscaledEstimate is the true token count for a request body — the same
// estimator the router runs, without scaling.
func unscaledEstimate(t *testing.T, body string) int64 {
	t.Helper()
	req, err := anthropic.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return tokens.Estimate(req)
}

func postMessages(t *testing.T, baseURL string, billPromptTokens int64) anthropic.Usage {
	t.Helper()
	body := fmt.Sprintf(`{"model":"sized","max_tokens":100,"messages":[{"role":"user","content":"bill me %d"}]}`, billPromptTokens)
	resp, raw := post(t, baseURL+"/v1/messages", testToken, body)
	if resp.StatusCode != 200 {
		t.Fatalf("messages status %d: %s", resp.StatusCode, raw)
	}
	var out anthropic.MessagesResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out.Usage
}
