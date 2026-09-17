package launch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/router"
	"github.com/gremlord/gremlord/internal/store"
)

// Uses a real installed Codex binary with a fake API: catches argument-scope
// regressions that argv-only unit tests cannot detect. No billable requests.
func TestCodexCLIGatewaySmoke(t *testing.T) {
	bin := os.Getenv("GREMLORD_CODEX_BIN")
	if bin == "" {
		t.Skip("set GREMLORD_CODEX_BIN to exercise an installed Codex CLI")
	}
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer local-token" || r.Header.Get("X-Gremlord-Pin-Model") != "astra" {
			t.Errorf("wrong gateway URL/auth/pin: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"id": "msg_smoke", "type": "message", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "READY", "annotations": []any{}}}}
		for _, event := range []any{
			map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_smoke", "status": "in_progress"}},
			map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item},
			map[string]any{"type": "response.output_text.delta", "item_id": "msg_smoke", "output_index": 0, "content_index": 0, "delta": "READY"},
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
			map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_smoke", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}}},
		} {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer up.Close()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	os.MkdirAll(codexHome, 0700)
	route := config.Resolved{Alias: "astra", Model: config.Model{ID: "gpt-6-astra", ContextWindow: 600000}}
	thread := ""
	for turn := 1; turn <= 2; turn++ {
		args := []string{"-C", home, "exec"}
		if turn == 2 {
			args = append(args, "resume")
		}
		args = append(args, "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "-c", `features.apps=false`, "-c", `features.plugins=false`, "-c", `web_search="disabled"`)
		if turn == 2 {
			args = append(args, thread)
		}
		args = append(args, "-")
		child, env := codexChild(Options{ClaudeArgs: args}, route, []string{"HOME=" + home, "CODEX_HOME=" + codexHome, "PATH=" + os.Getenv("PATH")}, up.URL, "local-token", "smoke", "")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, bin, child[1:]...)
		cmd.Env = env
		cmd.Dir = home
		cmd.Stdin = strings.NewReader("Reply READY without tools.")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err := cmd.Output()
		cancel()
		if err != nil {
			t.Fatalf("turn %d CLI failed: %v stderr=%s events=%s gateway_calls=%d", turn, err, stderr.String(), data, calls.Load())
		}
		if !bytes.Contains(data, []byte(`"type":"turn.completed"`)) {
			t.Fatalf("turn %d did not complete: %s", turn, data)
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var e struct {
				Type   string `json:"type"`
				Thread string `json:"thread_id"`
			}
			json.Unmarshal(line, &e)
			if e.Type == "thread.started" {
				if thread != "" && thread != e.Thread {
					t.Fatal("resume changed thread")
				}
				thread = e.Thread
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("gateway requests=%d, want 2", calls.Load())
	}
}

// Explicitly opt-in: uses the configured GPT API and costs real tokens. Only
// the in-process router sees provider credentials. Codex gets a clean home
// and the local token; no tools or permission bypass are needed for this test.
func TestCodexLiveCompaction(t *testing.T) {
	if os.Getenv("GREMLORD_CODEX_LIVE") != "1" {
		t.Skip("set GREMLORD_CODEX_LIVE=1 for billable native compaction smoke")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	alias := os.Getenv("GREMLORD_CODEX_LIVE_MODEL")
	if alias == "" {
		alias = "gpt-6-astra"
	}
	route, err := cfg.Resolve(alias)
	if err != nil {
		t.Fatal(err)
	}
	if route.Provider.Key() == "" {
		t.Fatal("configured provider has no API key")
	}
	route.Model.ReasoningEffort = "high"
	// Astra's native compaction_trigger requires >=20,000 when an output
	// allowance is supplied. Use the same cap as the three-turn benchmark.
	route.Model.MaxOutput = 32768
	cfg.Models[alias] = route.Model
	cfg.Budgets = &config.Budget{Daily: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	home := t.TempDir()
	st, err := store.Open(filepath.Join(home, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := router.NewServer(cfg, "local-smoke-token", home, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := srv.Handler()
	var triggers, replayed atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(data))
		var request struct {
			Input []struct {
				Type string `json:"type"`
			}
		}
		json.Unmarshal(data, &request)
		for _, item := range request.Input {
			if item.Type == "compaction_trigger" {
				triggers.Add(1)
			}
			if item.Type == "compaction" {
				replayed.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	args, env := codexChild(Options{ClaudeArgs: []string{"app-server"}}, route, []string{"HOME=" + home, "CODEX_HOME=" + codexHome, "PATH=" + os.Getenv("PATH")}, ts.URL, "local-smoke-token", "compact-smoke", "")
	// Isolate installed integrations while retaining Codex's native compactor.
	for _, override := range []string{`web_search="disabled"`, `agents.enabled=false`, `features.multi_agent=false`, `features.multi_agent_v2=false`, `features.skills=false`, `features.apps=false`, `features.plugins=false`} {
		args = append(args, "-c", override)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	cmd.Dir = home
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cancel(); cmd.Wait() }()
	messages := make(chan map[string]json.RawMessage, 128)
	go func() {
		defer close(messages)
		scan := bufio.NewScanner(out)
		scan.Buffer(make([]byte, 64<<10), 8<<20)
		for scan.Scan() {
			var m map[string]json.RawMessage
			if json.Unmarshal(scan.Bytes(), &m) == nil {
				select {
				case messages <- m:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	send := func(id int, method string, params any) {
		t.Helper()
		if err := json.NewEncoder(in).Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
	}
	next := func() map[string]json.RawMessage {
		t.Helper()
		select {
		case m, ok := <-messages:
			if !ok {
				t.Fatal("Codex app-server ended before completion")
			}
			return m
		case <-ctx.Done():
			t.Fatal("Codex compaction smoke timed out")
			return nil
		}
	}
	response := func(id int) json.RawMessage {
		t.Helper()
		for {
			m := next()
			var got int
			json.Unmarshal(m["id"], &got)
			if got == id {
				if len(m["error"]) > 0 {
					t.Fatalf("Codex RPC %d failed: %s", id, m["error"])
				}
				return m["result"]
			}
		}
	}
	send(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "gremlord_poc_smoke", "version": "0.1.0"}})
	response(1)
	json.NewEncoder(in).Encode(map[string]any{"method": "initialized"})
	send(2, "thread/start", map[string]any{"cwd": home, "approvalPolicy": "never", "sandbox": "read-only"})
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(response(2), &thread) != nil || thread.Thread.ID == "" {
		t.Fatal("missing Codex thread")
	}
	waitTurn := func() (string, bool) {
		t.Helper()
		var text strings.Builder
		compacted := false
		for {
			m := next()
			var method string
			json.Unmarshal(m["method"], &method)
			var p struct {
				Delta string `json:"delta"`
				Item  struct {
					Type string `json:"type"`
				} `json:"item"`
				Turn struct {
					Status string          `json:"status"`
					Error  json.RawMessage `json:"error"`
				} `json:"turn"`
			}
			json.Unmarshal(m["params"], &p)
			if method == "item/agentMessage/delta" {
				text.WriteString(p.Delta)
			}
			if method == "item/completed" && p.Item.Type == "contextCompaction" {
				compacted = true
			}
			if method == "turn/completed" {
				if p.Turn.Status != "completed" {
					message := strings.ReplaceAll(string(p.Turn.Error), route.Provider.BaseURL, "[upstream]")
					message = strings.ReplaceAll(message, route.Provider.Key(), "[credential]")
					t.Fatalf("Codex turn ended %s: %s", p.Turn.Status, message)
				}
				return text.String(), compacted
			}
		}
	}
	send(3, "turn/start", map[string]any{"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Remember these two project requirements for later: the release codename is ORCHID-731 and retry limit is 17. Do not use tools. Reply only READY."}}})
	response(3)
	waitTurn()
	send(4, "thread/compact/start", map[string]string{"threadId": thread.Thread.ID})
	response(4)
	_, compacted := waitTurn()
	if !compacted {
		t.Fatal("no completed contextCompaction event")
	}
	send(5, "turn/start", map[string]any{"threadId": thread.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Without using tools, return the release codename and retry limit from our earlier conversation, separated by a space."}}})
	response(5)
	text, _ := waitTurn()
	if !strings.Contains(text, "ORCHID-731") || !strings.Contains(text, "17") {
		t.Fatalf("requirements lost after compaction: %q", text)
	}
	rows, err := st.SessionUsage("compact-smoke")
	if err != nil || len(rows) < 3 {
		t.Fatalf("missing metering: rows=%d err=%v", len(rows), err)
	}
	var input, output, cached, written int64
	for _, r := range rows {
		if r.ErrType != "" {
			t.Fatalf("router error: %s", r.ErrType)
		}
		input += r.InputTokens + r.CacheReadTokens + r.CacheWriteTokens
		output += r.OutputTokens
		cached += r.CacheReadTokens
		written += r.CacheWriteTokens
	}
	t.Logf("compaction completed; requirements retained; metered requests=%d input=%d cache_read=%d cache_write=%d output=%d remote_triggers=%d opaque_compaction_replays=%d", len(rows), input, cached, written, output, triggers.Load(), replayed.Load())
}
