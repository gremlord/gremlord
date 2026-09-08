package openaibe

import (
	"fmt"
	"strings"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/openai"
)

// TranslateResponsesResponse maps a non-streaming /v1/responses body back
// to the Anthropic response shape. alias is echoed as the model id so
// Claude Code sees the name it asked for.
func TranslateResponsesResponse(resp *openai.ResponsesResponse, alias string) (*anthropic.MessagesResponse, error) {
	if resp.Error != nil && resp.Error.Message != "" {
		return nil, fmt.Errorf("upstream: %s", resp.Error.Message)
	}
	out := &anthropic.MessagesResponse{
		ID:    "msg_" + strings.TrimPrefix(resp.ID, "resp_"),
		Type:  "message",
		Role:  "assistant",
		Model: alias,
	}
	if out.ID == "msg_" {
		out.ID = "msg_gremlord"
	}

	hasTools := false
	for _, item := range resp.Output {
		switch item.Type {
		case "reasoning":
			if r := responsesReasoningText(item); r != "" {
				out.Content = append(out.Content, anthropic.ContentBlock{Type: "thinking", Thinking: r})
			}
		case "message":
			for _, c := range item.Content {
				switch c.Type {
				case "output_text":
					if c.Text != "" {
						out.Content = append(out.Content, anthropic.ContentBlock{Type: "text", Text: c.Text})
					}
				case "refusal":
					if c.Refusal != "" {
						out.Content = append(out.Content, anthropic.ContentBlock{Type: "text", Text: c.Refusal})
					}
					out.StopReason = "refusal"
				}
			}
		case "function_call":
			input, err := repairJSON(item.Arguments)
			if err != nil {
				return nil, fmt.Errorf("tool call %q returned malformed arguments: %w", item.Name, err)
			}
			out.Content = append(out.Content, anthropic.ContentBlock{
				Type: "tool_use", ID: toolUseID(item.CallID), Name: item.Name, Input: input,
			})
			hasTools = true
		}
	}

	if out.StopReason == "" {
		out.StopReason = mapResponsesStop(resp, hasTools)
	}
	if resp.Usage != nil {
		out.Usage = mapResponsesUsage(resp.Usage)
	}
	return out, nil
}

func responsesReasoningText(item openai.ResponsesOutputItem) string {
	var b strings.Builder
	for _, s := range item.Summary {
		if s.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s.Text)
	}
	return b.String()
}

func mapResponsesStop(resp *openai.ResponsesResponse, hasTools bool) string {
	if resp.Status == "incomplete" && resp.IncompleteDetails != nil {
		switch resp.IncompleteDetails.Reason {
		case "max_output_tokens":
			return "max_tokens"
		case "content_filter":
			return "refusal"
		}
	}
	if hasTools {
		return "tool_use"
	}
	return "end_turn"
}

// mapResponsesUsage keeps the Anthropic invariant that input_tokens
// excludes cache reads. Responses reports input_tokens inclusive of
// cached_tokens — subtract so pricing and the context gauge stay honest.
func mapResponsesUsage(u *openai.ResponsesUsage) anthropic.Usage {
	var cached int64
	if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
	}
	in := u.InputTokens - cached
	if in < 0 {
		in = 0
	}
	return anthropic.Usage{
		InputTokens:          in,
		OutputTokens:         u.OutputTokens,
		CacheReadInputTokens: cached,
	}
}
