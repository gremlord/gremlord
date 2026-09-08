package openaibe

import (
	"context"
	"encoding/json"
	"io"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/openai"
)

// RunResponses consumes a /v1/responses SSE body. There is no [DONE]
// sentinel — the stream ends on response.completed / incomplete / failed,
// or on EOF. Unknown future event types are ignored. error /
// response.failed are matched before the unknown-JSON continue so an
// upstream failure never degrades into a silent end_turn.
func (s *streamState) RunResponses(ctx context.Context, body io.Reader) (anthropic.Usage, string) {
	return s.runSSE(ctx, body, s.handleResponsesLine)
}

func (s *streamState) handleResponsesLine(data []byte) (done bool, errType string) {
	var ev openai.ResponsesStreamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return false, "" // tolerate provider noise
	}
	switch ev.Type {
	case "error":
		msg := ev.Message
		if msg == "" && ev.Error != nil {
			msg = ev.Error.Message
		}
		if msg == "" {
			msg = "upstream error"
		}
		s.sse.ErrorEvent("api_error", "upstream: "+msg)
		return true, "api_error"
	case "response.failed":
		msg := "upstream failed"
		if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
			msg = ev.Response.Error.Message
		} else if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		s.sse.ErrorEvent("api_error", "upstream: "+msg)
		return true, "api_error"
	case "response.created":
		id := "gremlord"
		if ev.Response != nil && ev.Response.ID != "" {
			id = ev.Response.ID
		}
		s.sawChunk = true
		s.startMessage("msg_" + id)
	case "response.output_item.added":
		s.sawChunk = true
		if ev.Item == nil {
			return false, ""
		}
		s.startMessage("msg_gremlord")
		switch ev.Item.Type {
		case "reasoning":
			s.ensureBlock("thinking")
		case "message":
			s.ensureBlock("text")
		case "function_call":
			// Responses v1 disables parallel_tool_calls. Keep a serial
			// function call buffered until output_item.done so a provider
			// may split the id/name/arguments across added/delta/done.
			s.holdPendingTool = true
			s.closeBlock()
			if ev.Item.CallID != "" {
				s.pendingID = ev.Item.CallID
			}
			if ev.Item.Name != "" {
				s.pendingName = ev.Item.Name
			}
			if ev.Item.Arguments != "" && s.pendingArgs == "" {
				s.pendingArgs = ev.Item.Arguments
			}
			s.havePending = true
			s.holdPendingTool = true
		}
	case "response.reasoning_summary_text.delta":
		s.sawChunk = true
		s.startMessage("msg_gremlord")
		if ev.Delta != "" {
			s.ensureBlock("thinking")
			s.sse.Event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.index - 1,
				"delta": map[string]string{"type": "thinking_delta", "thinking": ev.Delta},
			})
		}
	case "response.output_text.delta":
		s.sawChunk = true
		s.startMessage("msg_gremlord")
		if ev.Delta != "" {
			s.ensureBlock("text")
			s.sse.Event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.index - 1,
				"delta": map[string]string{"type": "text_delta", "text": ev.Delta},
			})
		}
	case "response.function_call_arguments.delta":
		s.sawChunk = true
		s.startMessage("msg_gremlord")
		if ev.Delta != "" {
			// output_item.added normally starts the buffer; tolerate a
			// delta-first provider and let output_item.done supply call_id/name.
			s.holdPendingTool = true
			s.havePending = true
			s.pendingArgs += ev.Delta
		}
	case "response.output_item.done":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			s.reconcileResponsesTool(ev.Item)
		} else if s.havePending {
			s.holdPendingTool = false
			if s.pendingName == "" {
				s.pendingName = "unknown_tool"
			}
			s.openToolBlock()
		}
		s.closeBlock()
		s.holdPendingTool = false
	case "response.completed", "response.incomplete":
		s.sawChunk = true
		id := "gremlord"
		if ev.Response != nil && ev.Response.ID != "" {
			id = ev.Response.ID
		}
		s.startMessage("msg_" + id)
		if ev.Response != nil {
			if ev.Response.Usage != nil {
				s.usage = mapResponsesUsage(ev.Response.Usage)
			}
			s.directStopReason = mapResponsesStop(ev.Response, s.sawToolCall)
		}
		return true, s.finalize()
	}
	return false, ""
}

func (s *streamState) reconcileResponsesTool(item *openai.ResponsesOutputItem) {
	if item.CallID != "" {
		s.pendingID = item.CallID
	}
	if item.Name != "" {
		s.pendingName = item.Name
	}
	// Prefer streamed deltas; some providers repeat the full argument string
	// on done, while others send it only there.
	if s.pendingArgs == "" && item.Arguments != "" {
		s.pendingArgs = item.Arguments
	}
	s.havePending = true
	if s.pendingName == "" {
		s.pendingName = "unknown_tool"
	}
	s.holdPendingTool = false
	s.openToolBlock()
}
