package openaibe

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/openai"
)

func TestResponsesToolHistoryRepair(t *testing.T) {
	input := []openai.ResponsesInputItem{
		{Replay: json.RawMessage(`{"type":"function_call","call_id":"native_call","name":"read_file","arguments":"{}"}`)},
		{Type: "function_call", CallID: "interrupted", Name: "read_file", Arguments: `{}`},
		{Type: "function_call_output", CallID: "native_call", Output: "package main"},
		{Type: "function_call_output", CallID: "orphan", Output: "important result"},
	}
	out := normalizeResponsesToolHistory(input)
	if len(out) != 5 {
		t.Fatalf("input = %+v", out)
	}
	if out[2].Type != "function_call_output" || out[2].CallID != "interrupted" || !strings.Contains(out[2].Output, "interrupted") {
		t.Fatal("missing interrupted result")
	}
	if out[3].CallID != "native_call" {
		t.Fatal("original call pairing changed")
	}
	if out[4].Role != "user" || !strings.Contains(out[4].Content.([]openai.ResponsesContentPart)[0].Text, "important result") {
		t.Fatal("orphan data lost")
	}
	first, _ := json.Marshal(out)
	second, _ := json.Marshal(normalizeResponsesToolHistory(out))
	if string(first) != string(second) {
		t.Fatal("normalization is not idempotent")
	}
}
