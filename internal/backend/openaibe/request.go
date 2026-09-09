// Package openaibe translates the Anthropic Messages API to OpenAI-dialect
// upstreams. Chat Completions is the default (OpenAI, xAI, Ollama, vLLM,
// OpenRouter, …); the Responses flavor (/v1/responses) is opt-in per
// provider or model via config api: responses.
package openaibe

import (
	"fmt"
	"strings"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/openai"
)

// maxToolCallsPerMessage is OpenAI's hard cap on messages[].tool_calls
// length. Claude Code can emit a single assistant turn with more tool_use
// blocks than that (e.g. after context compaction); translateMessage splits
// the excess into extra assistant messages rather than sending an
// oversized array.
const maxToolCallsPerMessage = 128

// TranslateRequest maps an Anthropic request onto a ChatRequest. Fidelity
// gaps (documented in the README): cache_control stripped, server tools
// dropped, thinking blocks not round-tripped, top_k dropped, stop
// sequences truncated to 4.
func TranslateRequest(req *anthropic.MessagesRequest, route config.Resolved) (*openai.ChatRequest, error) {
	out := &openai.ChatRequest{
		Model:  route.Model.ID,
		Stream: req.Stream,
	}
	if req.Stream {
		out.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	}

	if sys := req.System.Text(); sys != "" {
		out.Messages = append(out.Messages, openai.ChatMessage{Role: "system", Content: sys})
	}

	for _, msg := range req.Messages {
		translated, err := translateMessage(msg)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, translated...)
	}

	for _, t := range req.Tools {
		if t.IsServerTool() {
			continue // web_search etc. — cannot translate; Claude Code degrades gracefully
		}
		out.Tools = append(out.Tools, openai.Tool{
			Type:     "function",
			Function: openai.Function{Name: t.Name, Description: t.Description, Parameters: sanitizeToolSchema(t.InputSchema)},
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
			out.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
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
	// max_tokens parameter name is provider-specific (o-series/GPT-5 use
	// max_completion_tokens).
	if route.Provider.MaxTokensParam == "max_completion_tokens" {
		out.MaxCompletionTokens = maxTokens
	} else {
		out.MaxTokens = maxTokens
	}

	reasoning := route.Model.Reasoning
	switch reasoning {
	case "effort":
		out.ReasoningEffort = effortFromBudget(req.Thinking)
		// Reasoning models reject sampling params.
	case "none":
		// GPT-5-class models default to some reasoning mode unless told
		// otherwise, and reject function tools in that mode on
		// /v1/chat/completions ("Function tools with reasoning_effort
		// are not supported ... set reasoning_effort to 'none'").
		// Setting it explicitly opts back into tool support.
		out.ReasoningEffort = "none"
		out.Temperature = req.Temperature
		out.TopP = req.TopP
	case "passive", "":
		out.Temperature = req.Temperature
		out.TopP = req.TopP
	}

	if n := len(req.StopSequences); n > 0 {
		if n > 4 {
			n = 4 // OpenAI limit
		}
		out.Stop = req.StopSequences[:n]
	}
	return out, nil
}

// translateMessage fans one Anthropic message out to one or more OpenAI
// messages. Ordering constraint: role:"tool" results must immediately
// follow the assistant tool_calls message, so tool_results come first and
// remaining text/images become a trailing user message.
func translateMessage(msg anthropic.Message) ([]openai.ChatMessage, error) {
	switch msg.Role {
	case "system":
		// Claude Code sends mid-conversation system messages (an
		// Anthropic API feature); OpenAI accepts system role anywhere.
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
		return []openai.ChatMessage{{Role: "system", Content: text}}, nil

	case "assistant":
		var toolCalls []openai.ToolCall
		text := ""
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				if text != "" {
					text += "\n"
				}
				text += b.Text
			case "tool_use":
				toolCalls = append(toolCalls, openai.ToolCall{
					ID:       b.ID,
					Type:     "function",
					Function: openai.FunctionCall{Name: b.Name, Arguments: string(b.Input)},
				})
			case "thinking", "redacted_thinking":
				// Dropped on resend — no OpenAI slot, and DeepSeek requires omitting it.
			}
		}
		if text == "" && len(toolCalls) == 0 {
			return nil, nil
		}
		if len(toolCalls) <= maxToolCallsPerMessage {
			out := openai.ChatMessage{Role: "assistant", ToolCalls: toolCalls}
			if text != "" {
				out.Content = text
			}
			return []openai.ChatMessage{out}, nil
		}
		// A single Anthropic turn can carry more tool_use blocks than
		// OpenAI's hard cap on messages[].tool_calls (128); split the
		// excess into additional assistant messages. The tool results for
		// all of them still arrive in the next Anthropic message and land
		// right after, so ordering holds.
		var out []openai.ChatMessage
		for i := 0; i < len(toolCalls); i += maxToolCallsPerMessage {
			end := min(i+maxToolCallsPerMessage, len(toolCalls))
			m := openai.ChatMessage{Role: "assistant", ToolCalls: toolCalls[i:end]}
			if i == 0 && text != "" {
				m.Content = text
			}
			out = append(out, m)
		}
		return out, nil

	case "user":
		var out []openai.ChatMessage
		var parts []openai.ContentPart
		for _, b := range msg.Content {
			switch b.Type {
			case "tool_result":
				content := b.FlatText()
				// ChatMessage.Content is `any` with omitempty, so an empty
				// string drops the key and the tool message arrives with no
				// content at all — which OpenAI-dialect servers reject. A
				// tool can legitimately return nothing (a Bash command with
				// no stdout, an empty content array, block types this
				// translation does not render), so say that instead.
				if strings.TrimSpace(content) == "" {
					content = "(no output)"
				}
				if b.IsError {
					content = "Error: " + content
				}
				out = append(out, openai.ChatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: content})
			case "text":
				parts = append(parts, openai.ContentPart{Type: "text", Text: b.Text})
			case "image":
				if b.Source == nil {
					continue
				}
				url := b.Source.URL
				if b.Source.Type == "base64" {
					url = fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data)
				}
				parts = append(parts, openai.ContentPart{Type: "image_url", ImageURL: &openai.ImageURL{URL: url}})
			}
		}
		if len(parts) > 0 {
			m := openai.ChatMessage{Role: "user"}
			if len(parts) == 1 && parts[0].Type == "text" {
				m.Content = parts[0].Text
			} else {
				m.Content = parts
			}
			out = append(out, m)
		}
		return out, nil

	default:
		return nil, fmt.Errorf("unsupported message role %q", msg.Role)
	}
}

// effortFromBudget maps thinking budget_tokens to reasoning_effort.
func effortFromBudget(t *anthropic.Thinking) string {
	if t == nil || t.Type == "disabled" {
		return ""
	}
	switch {
	case t.BudgetTokens == 0:
		return "medium" // adaptive thinking — no budget signal
	case t.BudgetTokens <= 1024:
		return "low"
	case t.BudgetTokens <= 8192:
		return "medium"
	default:
		return "high"
	}
}
