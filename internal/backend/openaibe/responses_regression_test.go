package openaibe

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
)

func TestResponsesStreamLateItemIDsAndAuthoritativeArguments(t *testing.T) {
	events := runResponsesStream(t, []string{
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"wrong\":"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"write"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"b\":2}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"write","arguments":"{\"b\":2}"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{\"a\":1}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{\"a\":1}"},{"type":"function_call","id":"fc_b","call_id":"call_b","name":"write","arguments":"{\"b\":2}"}]}}`,
	})
	var ids, args []string
	for _, event := range events {
		if event.name == "content_block_start" {
			block := event.data["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				ids = append(ids, block["id"].(string))
			}
		}
		if event.name == "content_block_delta" {
			delta := event.data["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				args = append(args, delta["partial_json"].(string))
			}
		}
	}
	if strings.Join(ids, ",") != "call_b,call_a" || strings.Join(args, "|") != `{"b":2}|{"a":1}` {
		t.Fatalf("ids=%v args=%v", ids, args)
	}
}

func TestResponsesStreamPrematureEOFDoesNotCommit(t *testing.T) {
	cache := newResponsesContinuity()
	call := continuityCall()
	history := continuityHistory(t)
	state := newStreamState(anthropic.NewSSEWriter(httptest.NewRecorder()), "gpt")
	state.respTurn = cache.prepare(call, continuityRequest(t, call, history[:1]), 20)
	body := `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n"
	_, errType := state.RunResponses(context.Background(), strings.NewReader(body))
	if errType != "api_error" || cache.lru.Len() != 0 {
		t.Fatalf("err=%s cached=%d", errType, cache.lru.Len())
	}
}

func TestResponsesStreamRefusal(t *testing.T) {
	events := runResponsesStream(t, []string{
		`{"type":"response.refusal.delta","item_id":"msg_1","delta":"I cannot help."}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot help."}]}]}}`,
	})
	var text string
	for _, event := range events {
		if event.name == "content_block_delta" {
			d := event.data["delta"].(map[string]any)
			if d["type"] == "text_delta" {
				text += d["text"].(string)
			}
		}
	}
	if text != "I cannot help." || events[len(events)-2].data["delta"].(map[string]any)["stop_reason"] != "refusal" {
		t.Fatalf("refusal was lost or duplicated: %s", text)
	}
}

func TestResponsesOptionalToolParametersStayOptional(t *testing.T) {
	req := parseReq(t, `{"model":"gpt","max_tokens":64,"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}},"required":["path"]}}],"messages":[{"role":"user","content":"read file"}]}`)
	out, err := TranslateResponsesRequest(req, responsesRoute("effort"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"strict":false`) || !strings.Contains(string(raw), `"required":["path"]`) {
		t.Fatalf("optional parameter semantics changed: %s", raw)
	}
}

func TestExplicitReasoningEffort(t *testing.T) {
	for _, test := range []struct{ client, pinned, want string }{
		{"low", "", "low"}, {"high", "", "high"}, {"max", "", "xhigh"},
		{"max", "medium", "medium"}, {"low", "ultra", "ultra"}, {"", "xhigh", "xhigh"},
	} {
		t.Run(test.client+"/"+test.pinned, func(t *testing.T) {
			req := parseReq(t, `{"model":"gpt","max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"`+test.client+`"},"messages":[{"role":"user","content":"work"}]}`)
			route := responsesRoute("effort")
			route.Model.ReasoningEffort = test.pinned
			resp, err := TranslateResponsesRequest(req, route)
			if err != nil || resp.Reasoning == nil || resp.Reasoning.Effort != test.want {
				t.Fatalf("responses effort=%+v err=%v", resp.Reasoning, err)
			}
			chat, err := TranslateRequest(req, route)
			if err != nil || chat.ReasoningEffort != test.want {
				t.Fatalf("chat effort=%s err=%v", chat.ReasoningEffort, err)
			}
		})
	}
}
