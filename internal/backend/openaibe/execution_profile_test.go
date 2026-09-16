package openaibe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
)

func TestExecutionProfileRequestAndCount(t *testing.T) {
	const raw = `{"model":"gpt","max_tokens":128,"system":[{"type":"text","text":"Original permissions and instructions."}],"messages":[{"role":"user","content":"Fix the code."}],"tools":[{"name":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}`
	var requests []map[string]json.RawMessage
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		requests = append(requests, body)
		io.WriteString(w, `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer up.Close()
	b := New()
	var counts []int64
	for _, profile := range []string{"", "gpt-efficient-v1", ""} {
		route := responsesRoute("effort")
		route.Provider.BaseURL = up.URL
		route.Model.ExecutionProfile = profile
		env, _ := anthropic.ParseEnvelope([]byte(raw))
		call := &backend.Call{Raw: []byte(raw), Envelope: env, Route: route}
		rec := httptest.NewRecorder()
		if got := b.Messages(context.Background(), call, rec); got.Status != 200 {
			t.Fatalf("profile %q: %+v %s", profile, got, rec.Body.String())
		}
		count := httptest.NewRecorder()
		b.CountTokens(context.Background(), call, count)
		var n anthropic.CountTokensResponse
		json.Unmarshal(count.Body.Bytes(), &n)
		counts = append(counts, n.InputTokens)
		if string(call.Raw) != raw {
			t.Fatal("profile changed caller's raw request")
		}
	}
	for i, body := range requests {
		var instructions string
		json.Unmarshal(body["instructions"], &instructions)
		want := "Original permissions and instructions."
		if i == 1 {
			want += "\n\n" + GPTEfficientPrompt
		}
		if instructions != want {
			t.Fatal("original instructions replaced, duplicated, or profile leaked")
		}
		for _, field := range []string{"input", "tools", "reasoning", "parallel_tool_calls"} {
			if string(body[field]) != string(requests[0][field]) {
				t.Fatalf("profile changed %s", field)
			}
		}
	}
	if counts[1] <= counts[0] || counts[2] != counts[0] {
		t.Fatalf("prompt not counted or leaked across calls: %v", counts)
	}
}

func TestExecutionProfileContinuityBoundary(t *testing.T) {
	c := newResponsesContinuity()
	call := continuityCall()
	history := continuityHistory(t)
	req := continuityRequest(t, call, history[:1])
	req.Instructions = "base"
	c.prepare(call, req, 20).remember(continuityResponse(t))
	continued := continuityRequest(t, call, history)
	continued.Instructions = "base\n\n" + GPTEfficientPrompt
	c.prepare(call, continued, 50)
	data, _ := json.Marshal(continued.Input)
	if strings.Contains(string(data), "encrypted_content") {
		t.Fatal("profile change reused reasoning from different instructions")
	}
}

func TestExecutionProfileDoesNotAffectChat(t *testing.T) {
	req := &anthropic.MessagesRequest{System: anthropic.SystemPrompt{{Type: "text", Text: "base"}}}
	route := responsesRoute("effort")
	route.Model.API = config.APIChatCompletions
	route.Model.ExecutionProfile = "gpt-efficient-v1"
	ApplyExecutionProfile(req, route)
	if req.System.Text() != "base" {
		t.Fatal("Responses-only profile leaked into Chat Completions")
	}
}
