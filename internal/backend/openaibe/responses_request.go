package openaibe

import (
	"fmt"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/openai"
)

// TranslateResponsesRequest maps an Anthropic request onto OpenAI's
// /v1/responses body. Same fidelity gaps as TranslateRequest, plus stop
// sequences (Responses has no equivalent). Display-only thinking blocks
// are dropped here; Backend restores original output items from its
// session-scoped continuity cache before sending. The 128-tool-call split
// is unnecessary here: function_call items are not capped that way.
func TranslateResponsesRequest(req *anthropic.MessagesRequest, route config.Resolved) (*openai.ResponsesRequest, error) {
	store := false
	// Anthropic's own default is parallel tool use. Standard Responses
	// function tools support it too; Codex's separate Responses Lite path
	// uses a different tool protocol. Unless explicitly disabled below, the
	// streamer tracks concurrent function_call items by id, so this no
	// longer has to be pinned false for correctness.
	parallel := true
	out := &openai.ResponsesRequest{
		Model:             route.Model.ID,
		Stream:            req.Stream,
		Store:             &store,
		ParallelToolCalls: &parallel,
		Include:           []string{"reasoning.encrypted_content"},
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
			Parameters:  sanitizeToolSchema(t.InputSchema),
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
	if maxTokens > 0 && maxTokens < 16 {
		maxTokens = 16 // OpenAI Responses minimum
	}
	out.MaxOutputTokens = maxTokens

	switch route.Model.Reasoning {
	case "effort":
		if effort := requestReasoningEffort(req, route.Model); effort != "" {
			out.Reasoning = &openai.ResponsesReasoning{Effort: effort, Summary: "auto"}
			if effort == "none" {
				out.Reasoning.Summary = ""
			}
		}
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
				// Backend replays the original reasoning, never this summary.
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
				content, images := toolResultText(b)
				out = append(out, openai.ResponsesInputItem{
					Type:   "function_call_output",
					CallID: b.ToolUseID,
					Output: content,
				})
				// FileRead nests pixels inside tool_result;
				// function_call_output.output is a string, so hoist
				// them onto the trailing user message as input_image.
				for _, src := range images {
					if url := src.DataURL(); url != "" {
						parts = append(parts, openai.ResponsesContentPart{Type: "input_image", ImageURL: url})
					}
				}
			case "text":
				parts = append(parts, openai.ResponsesContentPart{Type: "input_text", Text: b.Text})
			case "image":
				if b.Source == nil {
					continue
				}
				if url := b.Source.DataURL(); url != "" {
					parts = append(parts, openai.ResponsesContentPart{Type: "input_image", ImageURL: url})
				}
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
