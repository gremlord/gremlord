package openaibe

import (
	"encoding/json"

	"github.com/gremlord/gremlord/internal/openai"
)

// Interrupted tools and client compaction can leave unmatched calls/results.
// Responses requires paired calls. Follow Codex's history normalization for
// missing results, and preserve orphaned result text as user context instead
// of sending an invalid function_call_output or silently discarding the data.
func normalizeResponsesToolHistory(input []openai.ResponsesInputItem) []openai.ResponsesInputItem {
	calls, results := make(map[string]bool), make(map[string]bool)
	for _, item := range input {
		kind, id := responsesCallIdentity(item)
		if kind == "function_call" {
			calls[id] = true
		} else if kind == "function_call_output" {
			results[id] = true
		}
	}
	out := make([]openai.ResponsesInputItem, 0, len(input))
	for _, item := range input {
		kind, id := responsesCallIdentity(item)
		if kind == "function_call_output" && !calls[id] {
			out = append(out, openai.ResponsesInputItem{Type: "message", Role: "user",
				Content: []openai.ResponsesContentPart{{Type: "input_text",
					Text: "[Tool result " + id + "; original call unavailable in this history]\n" + item.Output}}})
			continue
		}
		out = append(out, item)
		if kind == "function_call" && !results[id] {
			out = append(out, openai.ResponsesInputItem{Type: "function_call_output", CallID: id,
				Output: "Tool execution was interrupted; no result is available."})
		}
	}
	return out
}

func responsesCallIdentity(item openai.ResponsesInputItem) (string, string) {
	if len(item.Replay) != 0 {
		var identity struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(item.Replay, &identity)
		return identity.Type, identity.CallID
	}
	return item.Type, item.CallID
}
