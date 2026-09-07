package openaibe

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
)

// runStream feeds recorded OpenAI chunks through the state machine and
// returns the emitted Anthropic events as (event, payload) pairs.
func runStream(t *testing.T, chunks []string) []event {
	t.Helper()
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt")
	body := ""
	for _, c := range chunks {
		body += "data: " + c + "\n\n"
	}
	body += "data: [DONE]\n\n"
	usage, errType := state.Run(context.Background(), strings.NewReader(body))
	_ = usage
	if errType != "" {
		t.Fatalf("stream errType=%q", errType)
	}
	return parseEvents(t, rec.Body.String())
}

type event struct {
	name string
	data map[string]any
}

func parseEvents(t *testing.T, raw string) []event {
	t.Helper()
	var out []event
	var name string
	for _, line := range strings.Split(raw, "\n") {
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			name = v
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			var data map[string]any
			if err := json.Unmarshal([]byte(v), &data); err != nil {
				t.Fatalf("bad event data %q: %v", v, err)
			}
			out = append(out, event{name, data})
		}
	}
	return out
}

func names(evs []event) string {
	parts := make([]string, len(evs))
	for i, e := range evs {
		parts[i] = e.name
	}
	return strings.Join(parts, " ")
}

func TestStreamTextOnly(t *testing.T) {
	evs := runStream(t, []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
	})
	want := "message_start ping content_block_start content_block_delta content_block_delta content_block_stop message_delta message_stop"
	if names(evs) != want {
		t.Fatalf("grammar:\n got %s\nwant %s", names(evs), want)
	}
	last := evs[len(evs)-2]
	delta := last.data["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", delta["stop_reason"])
	}
	usage := last.data["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 10 || usage["output_tokens"].(float64) != 2 {
		t.Errorf("usage: %v", usage)
	}
}

func TestStreamToolCalls(t *testing.T) {
	evs := runStream(t, []string{
		`{"id":"c2","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me check."}}]}`,
		`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}`,
		`{"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		`{"id":"c2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c2","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":20}}}`,
	})

	// Text block, then two tool_use blocks with sequential indices.
	var starts []map[string]any
	for _, e := range evs {
		if e.name == "content_block_start" {
			starts = append(starts, e.data)
		}
	}
	if len(starts) != 3 {
		t.Fatalf("expected 3 content_block_start, got %d: %s", len(starts), names(evs))
	}
	cb1 := starts[1]["content_block"].(map[string]any)
	cb2 := starts[2]["content_block"].(map[string]any)
	if cb1["type"] != "tool_use" || cb1["name"] != "read_file" || cb1["id"] != "call_a" {
		t.Errorf("first tool block: %v", cb1)
	}
	if cb2["name"] != "bash" {
		t.Errorf("second tool block: %v", cb2)
	}
	if starts[1]["index"].(float64) != 1 || starts[2]["index"].(float64) != 2 {
		t.Errorf("block indices: %v %v", starts[1]["index"], starts[2]["index"])
	}

	// input_json_delta fragments reassemble the arguments
	var args string
	for _, e := range evs {
		if e.name == "content_block_delta" {
			d := e.data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" && e.data["index"].(float64) == 1 {
				args += d["partial_json"].(string)
			}
		}
	}
	if args != `{"path":"a.go"}` {
		t.Errorf("reassembled args = %q", args)
	}

	last := evs[len(evs)-2]
	if last.data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Error("stop_reason should be tool_use")
	}
	if last.data["usage"].(map[string]any)["cache_read_input_tokens"].(float64) != 20 {
		t.Error("cached_tokens not surfaced as cache_read_input_tokens")
	}
	if last.data["usage"].(map[string]any)["input_tokens"].(float64) != 30 {
		t.Error("input_tokens should exclude cache reads")
	}
}

func TestStreamReasoningContent(t *testing.T) {
	evs := runStream(t, []string{
		`{"id":"c3","choices":[{"index":0,"delta":{"reasoning_content":"thinking hard"}}]}`,
		`{"id":"c3","choices":[{"index":0,"delta":{"content":"answer"}}]}`,
		`{"id":"c3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	// thinking block must close with a signature_delta before the text block opens
	seen := []string{}
	for _, e := range evs {
		switch e.name {
		case "content_block_start":
			seen = append(seen, e.data["content_block"].(map[string]any)["type"].(string))
		case "content_block_delta":
			seen = append(seen, e.data["delta"].(map[string]any)["type"].(string))
		}
	}
	want := "thinking thinking_delta signature_delta text text_delta"
	if strings.Join(seen, " ") != want {
		t.Errorf("block sequence:\n got %s\nwant %s", strings.Join(seen, " "), want)
	}
	for _, e := range evs {
		if e.name != "content_block_delta" {
			continue
		}
		delta := e.data["delta"].(map[string]any)
		if delta["type"] == "signature_delta" && delta["signature"] != "" {
			t.Errorf("translated thinking signature = %v, want empty display-only marker", delta["signature"])
		}
	}
}

func TestStreamSplitToolHeader(t *testing.T) {
	// Some providers split id and name across chunks — args must buffer, and
	// the fragments must land in a single tool block.
	evs := runStream(t, []string{
		`{"id":"c4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"arguments":"{\"a\""}}]}}]}`,
		`{"id":"c4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"late_name","arguments":":1}"}}]}}]}`,
		`{"id":"c4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	var starts []map[string]any
	var args string
	for _, e := range evs {
		if e.name == "content_block_start" {
			starts = append(starts, e.data["content_block"].(map[string]any))
		}
		if e.name == "content_block_delta" {
			d := e.data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				args += d["partial_json"].(string)
			}
		}
	}
	if len(starts) != 1 {
		t.Fatalf("expected 1 tool block, got %d: %s", len(starts), names(evs))
	}
	if starts[0]["name"] != "late_name" || starts[0]["id"] != "call_x" || args != `{"a":1}` {
		t.Errorf("opened=%v args=%q", starts[0], args)
	}
}

// Anthropic's tool_use.id must match ^[A-Za-z0-9_-]+$; vLLM-style ids like
// "Bash:0" persisted verbatim into Claude Code's transcript 400 on every
// later turn that resends the history to a real Anthropic model.
func TestStreamOpenToolBlockSanitizesID(t *testing.T) {
	evs := runStream(t, []string{
		`{"id":"c9","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"Bash:0","function":{"name":"Bash","arguments":"{}"}}]}}]}`,
		`{"id":"c9","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	for _, e := range evs {
		if e.name == "content_block_start" {
			cb := e.data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				if cb["id"] != "Bash_0" {
					t.Errorf("tool_use id = %v, want sanitized \"Bash_0\"", cb["id"])
				}
			}
		}
	}
}

func TestStreamToolCallsWithoutIndex(t *testing.T) {
	// Providers that omit tool_calls[].index (some xAI/OpenRouter/vLLM
	// builds) signal a new call with a fresh id; args-only fragments carry
	// neither and continue the open call.
	evs := runStream(t, []string{
		`{"id":"c5","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_a","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
		`{"id":"c5","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"a.go\"}"}}]}}]}`,
		`{"id":"c5","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_b","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		`{"id":"c5","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	var starts []map[string]any
	argsByIndex := map[float64]string{}
	for _, e := range evs {
		if e.name == "content_block_start" {
			starts = append(starts, e.data["content_block"].(map[string]any))
		}
		if e.name == "content_block_delta" {
			d := e.data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				argsByIndex[e.data["index"].(float64)] += d["partial_json"].(string)
			}
		}
	}
	if len(starts) != 2 {
		t.Fatalf("expected 2 tool blocks, got %d: %s", len(starts), names(evs))
	}
	if starts[0]["name"] != "read_file" || starts[1]["name"] != "bash" {
		t.Errorf("tool names: %v %v", starts[0]["name"], starts[1]["name"])
	}
	if argsByIndex[0] != `{"path":"a.go"}` || argsByIndex[1] != `{"cmd":"ls"}` {
		t.Errorf("args split wrong: %v", argsByIndex)
	}
}

func TestStreamCancellation(t *testing.T) {
	// A stalled upstream (body that never produces a byte) must not hang Run
	// past ctx cancellation.
	pr, pw := io.Pipe()
	defer pw.Close()
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan string, 1)
	go func() {
		_, errType := state.Run(ctx, pr)
		done <- errType
	}()
	cancel()
	select {
	case errType := <-done:
		if errType != "client_disconnect" {
			t.Errorf("errType = %q, want client_disconnect", errType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	// A stalled upstream that never sends a byte and never closes the
	// connection (the reported "stuck, needs a manual interrupt" symptom —
	// no scanner EOF, no ctx cancellation) must still give up once idleTimeout
	// elapses, rather than hang forever on ctx.Done().
	pr, pw := io.Pipe()
	defer pw.Close()
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt")
	state.keepAliveEvery = time.Hour // keep-alives shouldn't be what ends this
	state.idleTimeout = 20 * time.Millisecond

	done := make(chan string, 1)
	go func() {
		_, errType := state.Run(context.Background(), pr)
		done <- errType
	}()
	select {
	case errType := <-done:
		if errType != "api_error" {
			t.Errorf("errType = %q, want api_error", errType)
		}
		if !strings.Contains(rec.Body.String(), "no data for") {
			t.Errorf("expected idle-timeout error event, got:\n%s", rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after idleTimeout")
	}
}

func TestStreamIdleTimeoutResetsOnActivity(t *testing.T) {
	// Any scanner activity — including provider noise that isn't a usable
	// chunk — must reset the idle clock, so a slow-but-alive upstream isn't
	// mistaken for a stalled one.
	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt")
	state.keepAliveEvery = time.Hour
	state.idleTimeout = 60 * time.Millisecond

	done := make(chan string, 1)
	go func() {
		_, errType := state.Run(context.Background(), pr)
		done <- errType
	}()
	// Trickle blank/noise lines slower than one write per idleTimeout but
	// well within it, for longer than idleTimeout's total span.
	for i := 0; i < 4; i++ {
		time.Sleep(25 * time.Millisecond)
		io.WriteString(pw, ": keep-alive noise\n\n")
	}
	io.WriteString(pw, "data: {\"id\":\"c7\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	pw.Close()

	select {
	case errType := <-done:
		if errType != "" {
			t.Errorf("errType = %q, want success (empty)", errType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestStreamEmptyAfterKeepAlive(t *testing.T) {
	// Upstream that goes quiet long enough for a keep-alive ping to open the
	// message, then EOFs with zero chunks: still an error, not an empty
	// message (Claude Code retries on the error event).
	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt")
	state.keepAliveEvery = 5 * time.Millisecond

	done := make(chan string, 1)
	go func() {
		_, errType := state.Run(context.Background(), pr)
		done <- errType
	}()
	time.Sleep(30 * time.Millisecond) // let a ping open the message
	pw.Close()                        // EOF without a single chunk
	if errType := <-done; errType != "api_error" {
		t.Errorf("errType = %q, want api_error", errType)
	}
	if !strings.Contains(rec.Body.String(), "upstream produced no chunks") {
		t.Errorf("expected no-chunks error event, got:\n%s", rec.Body.String())
	}
}

func TestStreamKeepAliveBeforeFirstChunk(t *testing.T) {
	// While the upstream is quiet before the first token, pings must flow so
	// the client doesn't time out on a byte-silent connection.
	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "gpt")
	state.keepAliveEvery = 5 * time.Millisecond

	done := make(chan struct{})
	go func() {
		state.Run(context.Background(), pr)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	io.WriteString(pw, "data: {\"id\":\"c6\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	pw.Close()
	<-done

	evs := parseEvents(t, rec.Body.String())
	if len(evs) == 0 || evs[0].name != "message_start" {
		t.Fatalf("first event should be message_start, got: %s", names(evs))
	}
	pings := 0
	for _, e := range evs {
		if e.name == "ping" {
			pings++
		}
	}
	if pings < 2 {
		t.Errorf("expected keep-alive pings before first chunk, got %d", pings)
	}
	// The real chunk must not re-open the message.
	startCount := 0
	for _, e := range evs {
		if e.name == "message_start" {
			startCount++
		}
	}
	if startCount != 1 {
		t.Errorf("message_start emitted %d times", startCount)
	}
}

// Context scaling: the client-facing message_delta usage is scaled to the
// model's context budget, while Run returns TRUE usage for pricing.
func TestStreamUsageContextScaling(t *testing.T) {
	rec := httptest.NewRecorder()
	state := newStreamState(anthropic.NewSSEWriter(rec), "qwen")
	state.scale = 200_000.0 / 32_000.0 // 32K-budget model
	state.estInput = 51000             // scaled request estimate, emitted in message_start
	body := "data: " + `{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: " + `{"id":"c1","choices":[],"usage":{"prompt_tokens":16000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":8000}}}` + "\n\n" +
		"data: [DONE]\n\n"
	usage, errType := state.Run(context.Background(), strings.NewReader(body))
	if errType != "" {
		t.Fatalf("errType=%q", errType)
	}
	if usage.InputTokens != 8000 || usage.CacheReadInputTokens != 8000 || usage.OutputTokens != 50 {
		t.Errorf("true usage = %+v", usage)
	}
	evs := parseEvents(t, rec.Body.String())
	var delta map[string]any
	for _, e := range evs {
		if e.name == "message_start" {
			// The context gauge reads input from message_start — it must
			// carry the scaled estimate, not zero.
			start := e.data["message"].(map[string]any)["usage"].(map[string]any)
			if start["input_tokens"].(float64) != 51000 {
				t.Errorf("message_start input_tokens = %v, want 51000", start["input_tokens"])
			}
		}
		if e.name == "message_delta" {
			delta = e.data["usage"].(map[string]any)
		}
	}
	// 8000 non-cached input at half the 32K budget → 50000 reported (×6.25).
	if delta["input_tokens"].(float64) != 50000 {
		t.Errorf("reported input_tokens = %v, want 50000", delta["input_tokens"])
	}
	if delta["cache_read_input_tokens"].(float64) != 50000 {
		t.Errorf("reported cache_read = %v, want 50000", delta["cache_read_input_tokens"])
	}
	if delta["output_tokens"].(float64) != 50 {
		t.Errorf("output must stay true, got %v", delta["output_tokens"])
	}
}
