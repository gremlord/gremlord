package router

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
)

// gaugeServer stands up a rule with a 1M "deep" tier and a 100K "light"
// tier over one fake upstream that bills a fixed 100K prompt whichever
// model is asked, and classifies to whatever *tier points at.
func gaugeServer(t *testing.T, gauge string, tier *string) (*httptest.Server, *store.Store, *[]string) {
	t.Helper()
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		if req.Model == "classifier-up" {
			io.WriteString(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"`+*tier+
				`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":1}}`)
			return
		}
		seen = append(seen, req.Model)
		io.WriteString(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":100000,"completion_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{"fake": {Type: config.ProviderOpenAI, BaseURL: upstream.URL}},
		Models: map[string]config.Model{
			"cheap": {Provider: "fake", ID: "classifier-up"},
			"big":   {Provider: "fake", ID: "deep-up", ContextWindow: 1_000_000},
			"small": {Provider: "fake", ID: "light-up", ContextWindow: 100_000},
		},
		Routing: map[string]config.RouteRule{
			"auto": {Classifier: "cheap", Default: "deep", ContextGauge: gauge,
				Tiers: map[string]string{"deep": "big", "light": "small"}},
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
	return ts, st, &seen
}

func reportedInput(t *testing.T, body string) int64 {
	t.Helper()
	var out struct {
		Usage struct {
			InputTokens     int64 `json:"input_tokens"`
			CacheReadTokens int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response unparseable: %v\n%s", err, body)
	}
	return out.Usage.InputTokens + out.Usage.CacheReadTokens
}

// The point of anchoring the gauge: the same conversation must report the
// same fullness to the client whichever tier serves the turn. Scaling per
// serving model made a light-tier turn read 5x fuller than the identical
// deep-tier turn, and Claude Code compacts on whatever the last turn said
// — so one cheap turn threw away context the big model still had room for.
func TestGaugeIsStableAcrossTierChanges(t *testing.T) {
	tier := "deep"
	ts, _, seen := gaugeServer(t, "", &tier)

	msg := `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"design the migration"}]}`
	resp, body := post(t, ts.URL+"/v1/messages", testToken, msg)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	deepReported := reportedInput(t, body)

	tier = "light"
	resp, body = post(t, ts.URL+"/v1/messages",
		testToken, `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"now rename the symbol"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	lightReported := reportedInput(t, body)

	if len(*seen) != 2 || (*seen)[0] != "deep-up" || (*seen)[1] != "light-up" {
		t.Fatalf("upstream models = %v, want deep-up then light-up", *seen)
	}
	// 100K true tokens against the 1M anchor = 20K reported, both times.
	if deepReported != 20_000 || lightReported != 20_000 {
		t.Errorf("reported deep=%d light=%d, want 20000 for both (anchored to the 1M tier)",
			deepReported, lightReported)
	}
}

// context_gauge: min keeps every tier reachable at any length by anchoring
// to the smallest window instead.
func TestGaugeMinAnchorsToSmallestTier(t *testing.T) {
	tier := "deep"
	ts, _, _ := gaugeServer(t, config.GaugeMin, &tier)
	_, body := post(t, ts.URL+"/v1/messages",
		testToken, `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"plan it"}]}`)
	// 100K true against the 100K anchor = the full 200K assumed window.
	if got := reportedInput(t, body); got != 200_000 {
		t.Errorf("reported = %d, want 200000", got)
	}
}

// The opt-out restores per-model scaling exactly.
func TestGaugeModelOptOutScalesPerServingModel(t *testing.T) {
	tier := "deep"
	ts, _, _ := gaugeServer(t, config.GaugeModel, &tier)
	_, body := post(t, ts.URL+"/v1/messages",
		testToken, `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"plan it"}]}`)
	if got := reportedInput(t, body); got != 20_000 {
		t.Errorf("deep turn reported = %d, want 20000 (1M model)", got)
	}

	tier = "light"
	_, body = post(t, ts.URL+"/v1/messages",
		testToken, `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"rename x"}]}`)
	if got := reportedInput(t, body); got != 200_000 {
		t.Errorf("light turn reported = %d, want 200000 — the jump the opt-out preserves", got)
	}
}

// The gauge the client saw is recorded next to the serving model's own
// budget, so a trajectory stays readable after the two diverge.
func TestGaugeBudgetRecordedInUsageLog(t *testing.T) {
	tier := "light"
	ts, st, _ := gaugeServer(t, "", &tier)
	post(t, ts.URL+"/v1/messages",
		testToken, `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"rename x"}]}`)

	rows, err := st.SessionUsage("sess-test")
	if err != nil || len(rows) == 0 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	var routed *store.UsageEvent
	for i := range rows {
		if rows[i].Model == "light-up" {
			routed = &rows[i]
		}
	}
	if routed == nil {
		t.Fatalf("no usage row for the routed model: %+v", rows)
	}
	if routed.CtxBudget != 100_000 {
		t.Errorf("CtxBudget = %d, want the serving model's own 100000", routed.CtxBudget)
	}
	if routed.EstInput == 0 || routed.EstSystem+routed.EstTools > routed.EstInput {
		t.Errorf("composition not recorded sanely: %+v", routed)
	}
}

func TestUnknownContextGaugeRejected(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{"fake": {Type: config.ProviderOpenAI, BaseURL: "http://x"}},
		Models:    map[string]config.Model{"m": {Provider: "fake", ID: "up"}},
		Routing: map[string]config.RouteRule{
			"auto": {Classifier: "m", Tiers: map[string]string{"deep": "m"}, ContextGauge: "biggest"},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "context_gauge") {
		t.Errorf("err = %v, want a context_gauge validation error", err)
	}
}
