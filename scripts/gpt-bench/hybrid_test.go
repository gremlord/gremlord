//go:build unix

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/pricing"
)

func TestHybridBillsBothComponentsAndSharesRequestLimit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"completed","usage":{"input_tokens":100,"output_tokens":20}}`)
	}))
	defer up.Close()
	p, path := testProxy(t, up.URL)
	p.arms["session"], p.counts["session"] = "claude-codex", 62
	p.turns = map[string]int{"session": 2}
	for i, session := range []string{"session", "session~worker", "session~worker"} {
		r := httptest.NewRequest("POST", "/upstream/"+session+"/v1/responses", strings.NewReader(`{"model":"sol","reasoning":{"effort":"high"}}`))
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		want := 200
		if i == 2 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("request %d: %d, want %d", i, w.Code, want)
		}
	}
	rows, err := p.store.SessionUsage("session")
	if err != nil || len(rows) != 3 {
		t.Fatalf("coordinator/worker usage not attributed together: %v %v", rows, err)
	}
	var input, output int64
	for _, row := range rows {
		input += row.InputTokens
		output += row.OutputTokens
	}
	if input != 200 || output != 40 {
		t.Fatalf("missing component usage: %d/%d", input, output)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for _, component := range []string{"coordinator", "worker", "worker"} {
		var m measurement
		if !scan.Scan() || json.Unmarshal(scan.Bytes(), &m) != nil || m.Component != component || m.Turn != 2 || m.Session != "session" {
			t.Fatalf("lost component/parent/turn attribution: %+v", m)
		}
	}
}

func TestHybridUsesComponentProviderModelAndPrice(t *testing.T) {
	makeUpstream := func(model, key string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Model string `json:"model"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Model != model || r.Header.Get("Authorization") != "Bearer "+key {
				t.Errorf("wrong component upstream: model=%s", body.Model)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"completed","usage":{"input_tokens":100,"output_tokens":20}}`)
		}))
	}
	worker := makeUpstream("sol", "REAL_TEST_KEY")
	defer worker.Close()
	coordinator := makeUpstream("cheap", "COORDINATOR_TEST_KEY")
	defer coordinator.Close()
	p, _ := testProxy(t, worker.URL)
	p.arms["session"] = "claude-codex"
	p.coordinator = &config.Resolved{Provider: config.Provider{BaseURL: coordinator.URL, APIKey: "COORDINATOR_TEST_KEY"}, Model: config.Model{ID: "cheap"}}
	p.prices = pricing.Load(t.TempDir(), &config.Config{Pricing: map[string]config.Price{
		"sol": {Input: 10, Output: 50}, "cheap": {Input: 2, Output: 6},
	}})
	for _, tc := range []struct{ session, model string }{{"session", "cheap"}, {"session~worker", "sol"}} {
		r := httptest.NewRequest("POST", "/upstream/"+tc.session+"/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"reasoning":{"effort":"high"}}`, tc.model)))
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	rows, err := p.store.SessionUsage("session")
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, row := range rows {
		want := .002
		if row.Model == "cheap" {
			want = .00032
		}
		if !row.Priced || row.CostUSD-want > 1e-10 || want-row.CostUSD > 1e-10 {
			t.Fatalf("component mispriced: %+v", row)
		}
	}
}
