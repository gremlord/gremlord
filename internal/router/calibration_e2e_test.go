package router

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
	"github.com/gremlord/gremlord/internal/tokens"
)

// calibServer serves one model with a deliberately tight window, over a
// store the caller can pre-seed with usage history.
func calibServer(t *testing.T, window int, seed func(*store.Store)) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":9000,"completion_tokens":5}}`)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{"fake": {Type: config.ProviderOpenAI, BaseURL: upstream.URL}},
		Models:    map[string]config.Model{"tight": {Provider: "fake", ID: "tight-up", ContextWindow: window}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if seed != nil {
		seed(st)
	}
	srv := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// 8000 words is ~12.5K estimated tokens raw, plus the request's own
// max_tokens as output headroom. Against a 12K window that overflows;
// corrected by a measured 0.6 it needs ~7.6K and fits comfortably.
func calibRequest() string {
	return `{"model":"tight","max_tokens":100,"messages":[{"role":"user","content":"` +
		strings.Repeat("word ", 8000) + `"}]}`
}

// Uncalibrated, the estimator's bias-high guess is what the dispatch guard
// tests against — so a request the model could actually have held is
// refused. That refusal is the cost of the over-count made visible.
func TestUncalibratedEstimateRefusesRequest(t *testing.T) {
	ts := calibServer(t, 12_000, nil)
	resp, body := post(t, ts.URL+"/v1/messages", testToken, calibRequest())
	if resp.StatusCode != 400 || !strings.Contains(body, "too large for model") {
		t.Fatalf("status=%d body=%s, want the prompt-too-long guard", resp.StatusCode, body)
	}
}

// With history showing the estimator runs ~40% high on this model, the
// same request fits and is served.
func TestCalibratedEstimateRecoversWindow(t *testing.T) {
	ts := calibServer(t, 12_000, func(st *store.Store) {
		for i := 0; i < tokens.MinSamples; i++ {
			if err := st.RecordUsage(store.UsageEvent{
				TS: time.Now(), Model: "tight-up", Status: 200,
				InputTokens: 600, EstInput: 1000,
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	resp, body := post(t, ts.URL+"/v1/messages", testToken, calibRequest())
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s, want the calibrated estimate to fit", resp.StatusCode, body)
	}
}

// One outlier request is not a measurement.
func TestCalibrationIgnoresThinHistory(t *testing.T) {
	ts := calibServer(t, 12_000, func(st *store.Store) {
		if err := st.RecordUsage(store.UsageEvent{
			TS: time.Now(), Model: "tight-up", Status: 200,
			InputTokens: 1, EstInput: 100_000,
		}); err != nil {
			t.Fatal(err)
		}
	})
	resp, _ := post(t, ts.URL+"/v1/messages", testToken, calibRequest())
	if resp.StatusCode != 400 {
		t.Errorf("status=%d, want the raw estimate to still govern below MinSamples", resp.StatusCode)
	}
}
