//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/pricing"
	"github.com/gremlord/gremlord/internal/store"
)

// The shared meter is part of the experiment: verify that it preserves the
// wire payload, keeps credentials out of artifacts, and counts cached input
// separately rather than adding it to total input twice.
func TestMeterStreamingAndJSON(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			response := `{"status":"completed","usage":{"input_tokens":1000,"output_tokens":80,"input_tokens_details":{"cached_tokens":900},"output_tokens_details":{"reasoning_tokens":60}},"output":[{"type":"reasoning","encrypted_content":"PRIVATE_OUTPUT"},{"type":"function_call"}]}`
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer REAL_TEST_KEY" {
					t.Error("upstream request credentials/path")
				}
				if r.Header.Get("X-Openai-Internal-Codex-Responses-Lite") != "true" || r.Header.Get("X-Codex-Turn-State") != "test-state" || r.Header.Get("X-Hop-Only") != "" {
					t.Error("native protocol headers lost or hop header leaked")
				}
				w.Header().Set("X-Codex-Turn-State", "next-state")
				var req map[string]any
				json.NewDecoder(r.Body).Decode(&req)
				if req["max_output_tokens"] != float64(32768) {
					t.Error("missing shared output cap")
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, response)
				}
			}))
			defer up.Close()
			p, logPath := testProxy(t, up.URL)
			req := httptest.NewRequest("POST", "/upstream/session/v1/responses", strings.NewReader(`{"model":"sol","reasoning":{"effort":"high"},"parallel_tool_calls":true,"input":[{"type":"reasoning","encrypted_content":"PRIVATE_INPUT"}]}`))
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("X-Openai-Internal-Codex-Responses-Lite", "true")
			req.Header.Set("X-Codex-Turn-State", "test-state")
			req.Header.Set("Connection", "X-Hop-Only")
			req.Header.Set("X-Hop-Only", "drop")
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Header().Get("X-Codex-Turn-State") != "next-state" {
				t.Error("response protocol header lost")
			}
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), response) {
				t.Fatalf("response changed: %d %s", rec.Code, rec.Body.String())
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"PRIVATE_OUTPUT", "PRIVATE_INPUT", "REAL_TEST_KEY", "test-token"} {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatal("credential/ciphertext leaked")
				}
			}
			var m measurement
			if err := json.Unmarshal(bytes.TrimSpace(data), &m); err != nil {
				t.Fatal(err)
			}
			if m.Input != 1000 || m.Cached != 900 || m.Output != 80 || m.Reasoning != 60 || m.EncryptedInput != 1 || m.EncryptedOutput != 1 || m.Tools != 1 || m.Error != "" {
				t.Fatalf("bad metering: %+v", m)
			}
		})
	}
}

func TestMeterRejectsDifferentEffortAndUnknownSession(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid benchmark request reached API") }))
	defer up.Close()
	p, _ := testProxy(t, up.URL)
	for _, tc := range []struct{ session, effort string }{{"session", "low"}, {"unknown", "high"}} {
		req := httptest.NewRequest("POST", "/upstream/"+tc.session+"/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"sol","reasoning":{"effort":%q}}`, tc.effort)))
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("status %d", rec.Code)
		}
	}
}

func TestMeterUnterminatedStreamIsAnError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	}))
	defer up.Close()
	p, path := testProxy(t, up.URL)
	req := httptest.NewRequest("POST", "/upstream/session/v1/responses", strings.NewReader(`{"model":"sol","reasoning":{"effort":"high"}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	p.ServeHTTP(httptest.NewRecorder(), req)
	data, _ := os.ReadFile(path)
	var m measurement
	json.Unmarshal(bytes.TrimSpace(data), &m)
	if !strings.Contains(m.Error, "without final response") {
		t.Fatalf("unmetered EOF: %+v", m)
	}
}

func testProxy(t *testing.T, url string) (*proxy, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, config.DBName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	path := filepath.Join(dir, "requests.jsonl")
	log, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return &proxy{route: config.Resolved{Provider: config.Provider{BaseURL: url}, Model: config.Model{ID: "sol"}}, key: "REAL_TEST_KEY", token: "test-token", client: http.DefaultClient, store: db, prices: pricing.Load(dir, nil), log: log, arms: map[string]string{"session": "codex"}, counts: map[string]int{}}, path
}
