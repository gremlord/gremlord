package openaibe

import (
	"fmt"
	"strings"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/openai"
)

// TranslateResponsesRequest maps an Anthropic request onto OpenAI's
// /v1/responses body. Same fidelity gaps as TranslateRequest, plus stop
// sequences (Responses has no equivalent). Thinking blocks are still
// dropped on resend — we send store:false and do not request encrypted
// reasoning, so there is nothing to round-trip. The 128-tool-call split
// is unnecessary here: function_call items are not capped that way.
func TranslateResponsesRequest(req *anthropic.MessagesRequest, route config.Resolved) (*openai.ResponsesRequest, error) {
	store := false
	parallel := false
	out := &openai.ResponsesRequest{
		Model:             route.Model.ID,
		Stream:            req.Stream,
		Store:             &store,
		ParallelToolCalls: &parallel,
	}

	if sys := req.System.Text(); sys != "" {
		out.Instructions = sys
	}

	for _, msg := range req.Messages {
		items, err := translateResponsesMessage(msg)
		if err != nil {
			return nil, err
		}
		out.Input = append(out.Input, items...)
	}

	for _, t := range req.Tools {
		if t.IsServerTool() {
			continue // web_search etc. — cannot translate; Claude Code degrades gracefully
		}
		out.Tools = append(out.Tools, openai.ResponsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}

	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto":
			out.ToolChoice = "auto"
		case "any":
			out.ToolChoice = "required"
		case "none":
			out.ToolChoice = "none"
		case "tool":
			out.ToolChoice = map[string]any{"type": "function", "name": tc.Name}
		}
		if tc.DisableParallelToolUse {
			f := false
			out.ParallelToolCalls = &f
		}
	}

	maxTokens := req.MaxTokens
	if cap := route.Model.MaxOutput; cap > 0 && maxTokens > cap {
		maxTokens = cap
	}
	out.MaxOutputTokens = maxTokens

	switch route.Model.Reasoning {
	case "effort":
		effort := effortFromBudget(req.Thinking)
		if effort == "" {
			effort = "medium"
		}
		out.Reasoning = &openai.ResponsesReasoning{Effort: effort, Summary: "auto"}
	case "none":
		out.Reasoning = &openai.ResponsesReasoning{Effort: "none"}
		out.Temperature = req.Temperature
		out.TopP = req.TopP
	case "passive", "":
		out.Temperature = req.Temperature
		out.TopP = req.TopP
	}
	return out, nil
}

func translateResponsesMessage(msg anthropic.Message) ([]openai.ResponsesInputItem, error) {
	switch msg.Role {
	case "system":
		text := ""
		for _, b := range msg.Content {
			if b.Type == "text" {
				if text != "" {
					text += "\n"
				}
				text += b.Text
			}
		}
		if text == "" {
			return nil, nil
		}
		return []openai.ResponsesInputItem{{
			Type:    "message",
			Role:    "system",
			Content: []openai.ResponsesContentPart{{Type: "input_text", Text: text}},
		}}, nil

	case "assistant":
		var out []openai.ResponsesInputItem
		text := ""
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				if text != "" {
					text += "\n"
				}
				text += b.Text
			case "tool_use":
				if text != "" {
					out = append(out, openai.ResponsesInputItem{
						Type:    "message",
						Role:    "assistant",
						Content: []openai.ResponsesContentPart{{Type: "output_text", Text: text}},
					})
					text = ""
				}
				out = append(out, openai.ResponsesInputItem{
					Type:      "function_call",
					CallID:    b.ID,
					Name:      b.Name,
					Arguments: string(b.Input),
				})
			case "thinking", "redacted_thinking":
				// Dropped on resend — store:false, no encrypted_content.
			}
		}
		if text != "" {
			out = append(out, openai.ResponsesInputItem{
				Type:    "message",
				Role:    "assistant",
				Content: []openai.ResponsesContentPart{{Type: "output_text", Text: text}},
			})
		}
		return out, nil

	case "user":
		var out []openai.ResponsesInputItem
		var parts []openai.ResponsesContentPart
		for _, b := range msg.Content {
			switch b.Type {
			case "tool_result":
				content := b.FlatText()
				if strings.TrimSpace(content) == "" {
					content = "(no output)"
				}
				if b.IsError {
					content = "Error: " + content
				}
				out = append(out, openai.ResponsesInputItem{
					Type:   "function_call_output",
					CallID: b.ToolUseID,
					Output: content,
				})
			case "text":
				parts = append(parts, openai.ResponsesContentPart{Type: "input_text", Text: b.Text})
			case "image":
				if b.Source == nil {
					continue
				}
				url := b.Source.URL
				if b.Source.Type == "base64" {
					url = fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data)
				}
				parts = append(parts, openai.ResponsesContentPart{Type: "input_image", ImageURL: url})
			}
		}
		if len(parts) > 0 {
			out = append(out, openai.ResponsesInputItem{
				Type:    "message",
				Role:    "user",
				Content: parts,
			})
		}
		return out, nil

	default:
		return nil, fmt.Errorf("unsupported message role %q", msg.Role)
	}
}
