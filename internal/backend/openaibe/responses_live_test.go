package openaibe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/wire"
)

// Explicit opt-in: makes two billable requests using an existing model alias.
// No tools are executed, configuration is not changed, and ciphertext/keys
// are never logged. This tests protocol acceptance, not task-quality parity.
func TestResponsesLiveContinuity(t *testing.T) {
	alias := os.Getenv("GREMLORD_LIVE_RESPONSES_MODEL")
	if alias == "" {
		t.Skip("set GREMLORD_LIVE_RESPONSES_MODEL to an existing Responses alias for a live two-request smoke test")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".gremlord", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	route, err := cfg.Resolve(alias)
	if err != nil {
		t.Fatal(err)
	}
	if route.Provider.Type != config.ProviderOpenAI || route.APIFlavor() != config.APIResponses {
		t.Fatal("live test requires an already configured OpenAI Responses alias")
	}
	route.Model.Reasoning, route.Model.ReasoningEffort = "effort", "high"
	b := New()
	transport := b.client.Transport
	requests, replayed := 0, 0
	b.client.Transport = continuityTransport(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Input []struct {
				Type      string `json:"type"`
				Encrypted string `json:"encrypted_content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		requests++
		for _, item := range req.Input {
			if item.Type == "reasoning" && item.Encrypted != "" {
				replayed++
			}
		}
		return transport.RoundTrip(r)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := &anthropic.MessagesRequest{Model: alias, MaxTokens: 4096, Stream: true,
		Messages:   []anthropic.Message{{Role: "user", Content: anthropic.MessageBody{{Type: "text", Text: "Determine the unique integer n between 1000 and 2000 inclusive such that n modulo 7 is 1, n modulo 11 is 5, and n modulo 13 is 1. If n is odd, use read_marker to retrieve left and right; if n is even, retrieve right twice. After receiving the results, add their values and reply with only the sum. Do not output the intermediate calculation. The marker values are only available through the tool."}}}},
		Tools:      []anthropic.Tool{{Name: "read_marker", Description: "Returns the integer stored in a named marker.", InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","enum":["left","right"]}},"required":["name"]}`)}},
		ToolChoice: &anthropic.ToolChoice{Type: "auto"},
	}
	call := &backend.Call{Route: route, Envelope: anthropic.Envelope{Model: alias, Stream: true}, Header: http.Header{wire.HeaderSession: []string{fmt.Sprintf("live-continuity-%d", time.Now().UnixNano())}}}
	call.Raw, _ = json.Marshal(req)
	rec := httptest.NewRecorder()
	res := b.Messages(ctx, call, rec)
	if res.Status != 200 {
		t.Fatalf("first response: status=%d error=%s", res.Status, res.ErrType)
	}
	content := assembleResponsesClient(t, rec.Body.String())
	t.Logf("first response: input=%d output=%d cached continuations=%d", res.Usage.InputSide(), res.Usage.OutputTokens, b.continuity.lru.Len())
	for el := b.continuity.lru.Front(); el != nil; el = el.Next() {
		entry := el.Value.(*responsesContinuation)
		for _, item := range entry.items {
			var shape struct {
				Type      string `json:"type"`
				Encrypted string `json:"encrypted_content"`
			}
			json.Unmarshal(item.Replay, &shape)
			t.Logf("cached item type=%s encrypted=%t", shape.Type, shape.Encrypted != "")
		}
	}
	results := anthropic.MessageBody{}
	seen := map[string]bool{}
	for _, block := range content {
		if block.Type != "tool_use" {
			continue
		}
		var args struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(block.Input, &args) != nil || (args.Name != "left" && args.Name != "right") {
			t.Fatal("model returned unexpected tool arguments")
		}
		seen[args.Name] = true
		value := "17"
		if args.Name == "right" {
			value = "25"
		}
		results = append(results, anthropic.ContentBlock{Type: "tool_result", ToolUseID: block.ID, Content: anthropic.MessageBody{{Type: "text", Text: value}}})
	}
	if len(seen) != 2 {
		t.Fatalf("model did not request both markers in the first response (got %d)", len(seen))
	}
	req.Messages = append(req.Messages, anthropic.Message{Role: "assistant", Content: content}, anthropic.Message{Role: "user", Content: results})
	req.Stream = false
	req.ToolChoice = &anthropic.ToolChoice{Type: "none"}
	call.Raw, _ = json.Marshal(req)
	rec = httptest.NewRecorder()
	res = b.Messages(ctx, call, rec)
	if res.Status != 200 {
		t.Fatalf("continuation: status=%d error=%s", res.Status, res.ErrType)
	}
	var answer anthropic.MessagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, block := range answer.Content {
		text += block.Text
	}
	if strings.TrimSpace(text) != "42" || requests != 2 || replayed == 0 {
		t.Fatalf("answer=%q requests=%d encrypted reasoning items replayed=%d", text, requests, replayed)
	}
	t.Logf("model=%s: two requests accepted, encrypted reasoning replayed, tool result sum correct; final input=%d output=%d tokens", route.Model.ID, res.Usage.InputSide(), res.Usage.OutputTokens)
}

func assembleResponsesClient(t *testing.T, raw string) anthropic.MessageBody {
	t.Helper()
	var content anthropic.MessageBody
	args := map[int]string{}
	for _, event := range parseEvents(t, raw) {
		switch event.name {
		case "content_block_start":
			data, _ := json.Marshal(event.data["content_block"])
			var block anthropic.ContentBlock
			if err := json.Unmarshal(data, &block); err != nil {
				t.Fatal(err)
			}
			content = append(content, block)
		case "content_block_delta":
			i := int(event.data["index"].(float64))
			d := event.data["delta"].(map[string]any)
			switch d["type"] {
			case "text_delta":
				content[i].Text += d["text"].(string)
			case "thinking_delta":
				content[i].Thinking += d["thinking"].(string)
			case "input_json_delta":
				args[i] += d["partial_json"].(string)
			}
		}
	}
	for i, arg := range args {
		if !json.Valid([]byte(arg)) {
			t.Fatal("invalid streamed arguments")
		}
		content[i].Input = json.RawMessage(arg)
	}
	return content
}
