package router

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
	"github.com/gremlord/gremlord/internal/wire"
)

func nativeServer(t *testing.T, upstream string, edit func(*config.Config)) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Providers: map[string]config.Provider{"openai": {Type: config.ProviderOpenAI, API: config.APIResponses, BaseURL: upstream, APIKey: "upstream-secret"}},
		Models:    map[string]config.Model{"astra": {Provider: "openai", ID: "gpt-native-test", ContextWindow: 600000}},
		Profiles:  map[string]config.Profile{"main": {Model: "astra"}},
		Pricing:   map[string]config.Price{"gpt-native-test": {Input: 2, Output: 10, CacheRead: .2, CacheWrite: 3}},
	}
	if edit != nil {
		edit(cfg)
	}
	st, err := store.Open(filepath.Join(dir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(cfg, testToken, dir, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

const nativeUsage = `"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":10}}`

func nativeRequest(t *testing.T, url, body string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set(wire.HeaderSession, "native-session")
	r.Header.Set(wire.HeaderProfile, "main")
	r.Header.Set(wire.HeaderPinModel, "astra")
	return r
}

func doNative(t *testing.T, r *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(data)
}

func TestNativeResponsesPreservesProtocolAndMeters(t *testing.T) {
	var calls atomic.Int32
	stream := ":keepalive\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"opaque==\"}]," + nativeUsage + "}}\r\n\r\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Path != "/responses" || r.URL.Query().Get("beta") != "1" {
			t.Errorf("URL: %s", r.URL)
		}
		for _, key := range []string{wire.HeaderSession, wire.HeaderProfile, wire.HeaderPinModel, "X-Agentic-Cwd", "X-Api-Key", "Cookie", "X-Hop"} {
			if r.Header.Get(key) != "" {
				t.Errorf("private/hop header leaked: %s", key)
			}
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("wrong upstream authentication")
		}
		if r.Header.Get("X-Openai-Internal-Codex-Responses-Lite") != "true" || r.Header.Get("X-Codex-Turn-State") != "state" {
			t.Error("native headers dropped")
		}
		var b map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&b)
		for key, want := range map[string]string{"model": `"gpt-native-test"`, "max_output_tokens": "200", "parallel_tool_calls": "false", "custom": `{"future":9007199254740993}`, "store": "false"} {
			if string(b[key]) != want {
				t.Errorf("%s=%s want %s", key, b[key], want)
			}
		}
		var reasoning map[string]string
		json.Unmarshal(b["reasoning"], &reasoning)
		if reasoning["effort"] != "high" || reasoning["context"] != "all_turns" {
			t.Errorf("reasoning=%v", reasoning)
		}
		if n == 2 && !strings.Contains(string(b["input"]), `"encrypted_content":"opaque=="`) {
			t.Error("reasoning replay dropped")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "next-state")
		fmt.Fprint(w, stream)
	}))
	defer up.Close()
	srv, st := nativeServer(t, up.URL, func(c *config.Config) {
		m := c.Models["astra"]
		m.MaxOutput = 200
		m.ReasoningEffort = "high"
		c.Models["astra"] = m
	})
	for i := 0; i < 2; i++ {
		r := nativeRequest(t, srv.URL+"/v1/responses?beta=1", `{"model":"other-model","stream":true,"store":false,"parallel_tool_calls":false,"max_output_tokens":500,"reasoning":{"effort":"low","context":"all_turns"},"input":[{"type":"reasoning","encrypted_content":"opaque=="},{"type":"custom_tool_call_output","call_id":"call1","output":"result"}],"custom":{"future":9007199254740993}}`)
		r.Header.Set("X-Openai-Internal-Codex-Responses-Lite", "true")
		r.Header.Set("X-Codex-Turn-State", "state")
		r.Header.Set("X-Agentic-Cwd", "/private/workspace")
		r.Header.Set("X-Api-Key", "local-secret")
		r.Header.Set("Cookie", "private=value")
		r.Header.Set("Connection", "X-Hop")
		r.Header.Set("X-Hop", "private")
		resp, body := doNative(t, r)
		if resp.StatusCode != 200 || body != stream || resp.Header.Get("X-Codex-Turn-State") != "next-state" {
			t.Fatalf("response changed: status=%d body=%q", resp.StatusCode, body)
		}
	}
	rows, err := st.SessionUsage("native-session")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.InputTokens != 30 || row.CacheReadTokens != 60 || row.CacheWriteTokens != 10 || row.OutputTokens != 7 || row.ReportedInput != 100 || row.Alias != "astra" || row.Profile != "main" || math.Abs(row.CostUSD-.000172) > 1e-10 {
			t.Errorf("usage=%+v", row)
		}
	}
}

func TestNativeCompactionBudgetAndReplay(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/responses/compact" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		if body["max_output_tokens"] != nil || body["reasoning"] != nil {
			t.Error("generation-only fields injected into compaction")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"compact=="}],`+nativeUsage+`}`)
	}))
	defer up.Close()
	srv, st := nativeServer(t, up.URL, func(c *config.Config) {
		c.Budgets = &config.Budget{Daily: .0001}
		m := c.Models["astra"]
		m.MaxOutput = 200
		m.ReasoningEffort = "high"
		c.Models["astra"] = m
	})
	resp, body := doNative(t, nativeRequest(t, srv.URL+"/v1/responses/compact", `{"model":"astra","input":[]}`))
	if resp.StatusCode != 200 || !strings.Contains(body, `"encrypted_content":"compact=="`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, body = doNative(t, nativeRequest(t, srv.URL+"/v1/responses", `{"model":"astra","input":[{"type":"compaction","encrypted_content":"compact=="}]}`))
	if resp.StatusCode != 400 || !strings.Contains(body, "budget exceeded") || calls.Load() != 1 {
		t.Fatalf("budget: %d %s calls=%d", resp.StatusCode, body, calls.Load())
	}
	rows, _ := st.SessionUsage("native-session")
	if len(rows) != 1 || rows[0].CostUSD == 0 {
		t.Fatalf("unmetered compaction: %+v", rows)
	}
}

func TestNativeBudgetRecordedBeforeTerminalFlush(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed",`+nativeUsage+"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	defer close(release)
	srv, _ := nativeServer(t, up.URL, func(c *config.Config) {
		p := c.Profiles["main"]
		p.Budget = &config.Budget{Daily: .0001}
		c.Profiles["main"] = p
	})
	resp, err := http.DefaultClient.Do(nativeRequest(t, srv.URL+"/v1/responses", `{"model":"astra","stream":true,"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scan := bufio.NewScanner(resp.Body)
	for scan.Scan() {
		if scan.Text() == "" {
			break
		}
	}
	r, body := doNative(t, nativeRequest(t, srv.URL+"/v1/responses", `{"model":"astra","input":[]}`))
	if r.StatusCode != 400 || !strings.Contains(body, "budget exceeded") {
		t.Fatalf("budget race: %d %s", r.StatusCode, body)
	}
}

func TestNativeFailuresAreVisibleAndMetered(t *testing.T) {
	for _, tc := range []struct {
		name, body, ctype, want string
		status                  int
	}{
		{"incomplete", `data: {"type":"response.incomplete","response":{"status":"incomplete",` + nativeUsage + "}}\n\n", "text/event-stream", "response_incomplete", 200},
		{"truncated", "data: {\"type\":\"response.created\"}\n\n", "text/event-stream", "incomplete_stream", 200},
		{"missing-usage", `{"status":"completed"}`, "application/json", "usage_unavailable", 200},
		{"rate-limit", `{"error":{"message":"retry later"}}`, "application/json", "upstream_http_error", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer up.Close()
			srv, st := nativeServer(t, up.URL, nil)
			resp, body := doNative(t, nativeRequest(t, srv.URL+"/v1/responses", `{"model":"astra","input":[]}`))
			if resp.StatusCode != tc.status || body != tc.body {
				t.Fatalf("error rewritten: %d %q", resp.StatusCode, body)
			}
			// Truncated streams are recorded after EOF, rather than a terminal event.
			var rows []store.UsageEvent
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				rows, _ = st.SessionUsage("native-session")
				if len(rows) > 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if len(rows) != 1 || rows[0].ErrType != tc.want {
				t.Fatalf("rows=%+v", rows)
			}
			if tc.name == "incomplete" && rows[0].OutputTokens != 7 {
				t.Error("partial completion usage lost")
			}
		})
	}
}

func TestNativeRejectsUnsupportedRequests(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unsupported request reached upstream") }))
	defer up.Close()
	srv, _ := nativeServer(t, up.URL, nil)
	for _, body := range []string{`null`, `[]`, `{"model":"astra","background":true}`, `{"model":"astra","store":true}`, `{"model":"astra","store":null}`, `{"model":"astra","previous_response_id":"resp_1"}`, `{"model":"astra","conversation":"conv_1"}`} {
		resp, _ := doNative(t, nativeRequest(t, srv.URL+"/v1/responses", body))
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d", body, resp.StatusCode)
		}
	}
	r := nativeRequest(t, srv.URL+"/v1/responses", `{"model":"astra"}`)
	r.Header.Set("Content-Encoding", "zstd")
	resp, _ := doNative(t, r)
	if resp.StatusCode != 415 {
		t.Errorf("compression=%d", resp.StatusCode)
	}
	r = nativeRequest(t, srv.URL+"/v1/responses", `{"model":"astra"}`)
	r.Header.Set("Authorization", "Bearer wrong")
	resp, _ = doNative(t, r)
	if resp.StatusCode != 401 {
		t.Errorf("auth=%d", resp.StatusCode)
	}
	r = nativeRequest(t, srv.URL+"/v1/responses/resp_other", ``)
	r.Method = http.MethodGet
	resp, _ = doNative(t, r)
	if resp.StatusCode != 400 {
		t.Errorf("unsupported endpoint=%d", resp.StatusCode)
	}
	r = nativeRequest(t, srv.URL+"/v1/future-codex-endpoint", `{}`)
	r.Header.Set(wire.HeaderHarness, "codex")
	resp, body := doNative(t, r)
	if resp.StatusCode != 400 || !strings.Contains(body, "unsupported") {
		t.Fatalf("native traffic fell through to Anthropic: %d %s", resp.StatusCode, body)
	}
	for _, edit := range []func(*config.Config){
		func(c *config.Config) {
			p := c.Providers["openai"]
			p.API = config.APIChatCompletions
			c.Providers["openai"] = p
		},
		func(c *config.Config) { c.Routing = map[string]config.RouteRule{"astra": {}} },
		func(c *config.Config) { p := c.Providers["openai"]; p.MaxRequestBytes = 5; c.Providers["openai"] = p },
	} {
		srv, _ := nativeServer(t, up.URL, edit)
		resp, _ := doNative(t, nativeRequest(t, srv.URL+"/v1/responses", `{"model":"astra"}`))
		if resp.StatusCode != 400 {
			t.Errorf("unsupported route=%d", resp.StatusCode)
		}
	}
}
