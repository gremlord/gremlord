package openaibe

import (
	"fmt"
	"strings"

	"github.com/gremlord/gremlord/internal/anthropic"
)

// toolResultText flattens a tool_result's text (and tool_reference) blocks
// for the OpenAI tool / function_call_output slot, and returns nested
// images so the translator can hoist them onto a trailing user message.
//
// Chat Completions tool messages and Responses function_call_output.output
// are both text-only. FileRead (and MCP image tools) nest pixels inside
// the tool_result, so flattening them to "[image omitted …]" is what made
// OpenAI-dialect models report they could not see a file they just Read.
func toolResultText(b anthropic.ContentBlock) (string, []*anthropic.ImageSource) {
	images := b.ImageSources()
	content := b.ContentText()
	if strings.TrimSpace(content) == "" {
		switch n := len(images); {
		case n == 1:
			content = "[image]"
		case n > 1:
			content = fmt.Sprintf("[%d images]", n)
		default:
			content = "(no output)"
		}
	}
	if b.IsError {
		content = "Error: " + content
	}
	return content, images
}
