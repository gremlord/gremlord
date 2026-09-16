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

	"github.com/gremlord/gremlord/internal/backend/openaibe"
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

func TestExecutionProfileArmsAndTelemetry(t *testing.T) {
	if validateArms("gremlord", "gpt-efficient") != nil || validateArms("codex", "gpt-efficient") != nil || validateArms("typo", "gremlord") == nil || validateArms("gremlord", "gremlord") == nil {
		t.Fatal("invalid arm selection")
	}
	for _, arm := range []string{"gremlord", "gpt-efficient"} {
		t.Run(arm, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
			}))
			defer up.Close()
			p, logPath := testProxy(t, up.URL)
			p.arms["session"] = arm
			for _, withProfile := range []bool{false, true} {
				instructions := "PRIVATE_SYSTEM"
				if withProfile {
					instructions += openaibe.GPTEfficientPrompt
				}
				payload := map[string]any{"model": "sol", "reasoning": map[string]string{"effort": "high"}, "instructions": instructions, "tools": []any{}, "input": []any{map[string]string{"type": "reasoning", "encrypted_content": "PRIVATE_CIPHER"}, map[string]string{"type": "message", "content": "PRIVATE_VISIBLE"}}}
				data, _ := json.Marshal(payload)
				req := httptest.NewRequest("POST", "/upstream/session/v1/responses", bytes.NewReader(data))
				req.Header.Set("Authorization", "Bearer test-token")
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, req)
				want := 200
				if withProfile != (arm == "gpt-efficient") {
					want = 400
				}
				if rec.Code != want {
					t.Fatalf("profile=%v code=%d want=%d", withProfile, rec.Code, want)
				}
			}
			data, _ := os.ReadFile(logPath)
			for _, private := range []string{"PRIVATE_SYSTEM", "PRIVATE_CIPHER", "PRIVATE_VISIBLE", openaibe.GPTEfficientPrompt} {
				if bytes.Contains(data, []byte(private)) {
					t.Fatal("telemetry exposed request content")
				}
			}
			for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
				var m measurement
				json.Unmarshal(line, &m)
				if m.Status == 200 && (m.FirstOutputMS == nil || m.VisibleInputBytes == 0 || len(m.InstructionsSHA256) != 64) {
					t.Fatalf("missing telemetry: %+v", m)
				}
			}
		})
	}
}

func TestPromptComponentMeasurements(t *testing.T) {
	var hashes []string
	for _, supplement := range []string{"", "\n\n" + openaibe.GPTEfficientPrompt} {
		body := map[string]json.RawMessage{"tools": json.RawMessage(`[{"description":"read safely","parameters":{"type":"object"}}]`), "input": json.RawMessage(`[{"type":"reasoning","encrypted_content":"excluded"}]`)}
		body["instructions"], _ = json.Marshal("base instructions" + supplement)
		var m measurement
		measureInput(body, &m)
		if m.ToolCount != 1 || m.ToolDescriptionBytes != len("read safely") || m.ToolParameterBytes != len(`{"type":"object"}`) || m.VisibleInputBytes != 0 {
			t.Fatalf("wrong component sizes: %+v", m)
		}
		hashes = append(hashes, m.BaseInstructionsSHA256)
	}
	if hashes[0] != hashes[1] {
		t.Fatal("base prompt comparison includes the treatment")
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
