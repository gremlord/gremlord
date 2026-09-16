package openaibe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/openai"
)

// respPendingTool is one in-flight Responses function_call, keyed by item
// id (or "idx:N" from output_index when a provider omits the id). Concurrent
// items under parallel_tool_calls:true each get their own buffer so argument
// deltas don't concatenate across calls.
type respPendingTool struct {
	id   string
	name string
	args string
}

// RunResponses consumes a /v1/responses SSE body. There is no [DONE]
// sentinel — the stream ends on response.completed / incomplete / failed,
// or on EOF. Unknown future event types are ignored. error /
// response.failed are matched before the unknown-JSON continue so an
// upstream failure never degrades into a silent end_turn.
func (s *streamState) RunResponses(ctx context.Context, body io.Reader) (anthropic.Usage, string) {
	s.requireTerminal = true
	return s.runSSE(ctx, body, s.handleResponsesLine)
}

func (s *streamState) handleResponsesLine(data []byte) (done bool, errType string) {
	var ev openai.ResponsesStreamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return false, "" // tolerate provider noise
	}
	itemID := ""
	if ev.Item != nil {
		itemID = ev.Item.ID
	}
	key := s.responsesKey(itemID, ev.ItemID, ev.OutputIndex)
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
			// Buffer until output_item.done so a provider may split
			// id/name/arguments across added/delta/done, and so concurrent
			// items under parallel_tool_calls:true don't share one buffer.
			t := s.respTool(key)
			if ev.Item.CallID != "" {
				t.id = ev.Item.CallID
			}
			if ev.Item.Name != "" {
				t.name = ev.Item.Name
			}
			if ev.Item.Arguments != "" && t.args == "" {
				t.args = ev.Item.Arguments
			}
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
	case "response.output_text.delta", "response.refusal.delta":
		s.sawChunk = true
		s.startMessage("msg_gremlord")
		if ev.Delta != "" {
			if s.respTextSeen == nil {
				s.respTextSeen = make(map[string]bool)
			}
			s.respTextSeen[key] = true
			s.responsesText(ev.Delta)
		}
		if ev.Type == "response.refusal.delta" {
			s.directStopReason = "refusal"
		}
	case "response.function_call_arguments.delta":
		s.sawChunk = true
		s.startMessage("msg_gremlord")
		if ev.Delta != "" {
			// output_item.added normally starts the buffer; tolerate a
			// delta-first provider and let output_item.done supply call_id/name.
			t := s.respTool(key)
			t.args += ev.Delta
		}
	case "response.output_item.done":
		s.sawChunk = true
		s.startMessage("msg_gremlord")
		if ev.Item != nil {
			if s.respOutput == nil {
				s.respOutput = make(map[int]openai.ResponsesOutputItem)
			}
			s.respOutput[ev.OutputIndex] = *ev.Item
		}
		if ev.Item != nil && ev.Item.Type == "message" {
			s.completeResponsesText(key, ev.Item)
		}
		if ev.Item != nil && ev.Item.Type == "function_call" {
			if s.respEmitted[key] {
				break
			}
			t := s.respTool(key)
			if ev.Item.CallID != "" {
				t.id = ev.Item.CallID
			}
			if ev.Item.Name != "" {
				t.name = ev.Item.Name
			}
			// The completed item is authoritative, including when a provider
			// sends only some deltas or supplied an initial argument prefix.
			if ev.Item.Arguments != "" {
				t.args = ev.Item.Arguments
			}
			s.emitResponsesTool(t)
			s.markResponsesEmitted(key)
			s.dropRespTool(key)
			break
		}
		s.closeBlock()
		// Bare done (no item), or a provider that omits type: flush a
		// matching pending function_call so later text/thinking blocks
		// don't leapfrog it. Keyed lookup, never create-on-miss.
		if t := s.lookupRespTool(key); t != nil {
			s.emitResponsesTool(t)
			s.markResponsesEmitted(key)
			s.dropRespTool(key)
		}
	case "response.completed", "response.incomplete":
		s.sawChunk = true
		if ev.Response != nil && ev.Response.Error != nil {
			s.sse.ErrorEvent("api_error", "upstream: "+ev.Response.Error.Message)
			return true, "api_error"
		}
		id := "gremlord"
		if ev.Response != nil && ev.Response.ID != "" {
			id = ev.Response.ID
		}
		s.startMessage("msg_" + id)
		if ev.Response != nil {
			// Terminal output is a fallback for providers that omit item.done.
			// Prefer done items when the terminal payload omits the output array.
			if len(ev.Response.Output) == 0 {
				indices := make([]int, 0, len(s.respOutput))
				for i := range s.respOutput {
					indices = append(indices, i)
				}
				sort.Ints(indices)
				for _, i := range indices {
					ev.Response.Output = append(ev.Response.Output, s.respOutput[i])
				}
			}
			for i := range ev.Response.Output {
				item := &ev.Response.Output[i]
				k := s.responsesKey(item.ID, "", i)
				switch item.Type {
				case "function_call":
					if !s.respEmitted[k] && item.Name != "" && item.CallID != "" {
						t := s.respTool(k)
						t.id, t.name = item.CallID, item.Name
						if item.Arguments != "" {
							t.args = item.Arguments
						}
						s.emitResponsesTool(t)
						s.markResponsesEmitted(k)
						s.dropRespTool(k)
					}
				case "message":
					s.completeResponsesText(k, item)
				}
			}
			s.flushRespTools()
			if ev.Response.Usage != nil {
				s.usage = mapResponsesUsage(ev.Response.Usage)
			}
			if s.directStopReason != "refusal" {
				s.directStopReason = mapResponsesStop(ev.Response, s.sawToolCall)
			}
			s.respTurn.remember(ev.Response, s.respVisible)
		}
		return true, s.finalize()
	}
	return false, ""
}

func (s *streamState) responsesText(text string) {
	if s.openType == "text" && len(s.respVisible) > 0 && s.respVisible[len(s.respVisible)-1].Type == "text" {
		s.respVisible[len(s.respVisible)-1].Text += text
	} else {
		s.respVisible = append(s.respVisible, anthropic.ContentBlock{Type: "text", Text: text})
	}
	s.ensureBlock("text")
	s.sse.Event("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.index - 1,
		"delta": map[string]string{"type": "text_delta", "text": text},
	})
}

func (s *streamState) completeResponsesText(key string, item *openai.ResponsesOutputItem) {
	if s.respEmitted[key] {
		return
	}
	if !s.respTextSeen[key] {
		for _, part := range item.Content {
			if part.Type == "output_text" && part.Text != "" {
				s.responsesText(part.Text)
			} else if part.Type == "refusal" {
				s.responsesText(part.Refusal)
				s.directStopReason = "refusal"
			}
		}
	}
	s.markResponsesEmitted(key)
	s.closeBlock()
}

func (s *streamState) markResponsesEmitted(key string) {
	if s.respEmitted == nil {
		s.respEmitted = make(map[string]bool)
	}
	s.respEmitted[key] = true
}

// An item may acquire its id only on done. Alias output_index to that id
// and migrate any delta-first buffer instead of accidentally making a second
// call. Later events without item_id still find the same buffer by index.
func (s *streamState) responsesKey(itemID, eventItemID string, index int) string {
	key := responsesItemKey(itemID, eventItemID, index)
	old := s.respAliases[index]
	if itemID == "" && eventItemID == "" && old != "" {
		return old
	}
	if old == "" {
		old = responsesItemKey("", "", index)
	}
	if old != key {
		if t := s.respPending[old]; t != nil {
			delete(s.respPending, old)
			s.respPending[key] = t
			for i, k := range s.respOrder {
				if k == old {
					s.respOrder[i] = key
				}
			}
		}
		if s.respTextSeen[old] {
			s.respTextSeen[key] = true
			delete(s.respTextSeen, old)
		}
		if s.respEmitted[old] {
			s.respEmitted[key] = true
			delete(s.respEmitted, old)
		}
	}
	if s.respAliases == nil {
		s.respAliases = make(map[int]string)
	}
	s.respAliases[index] = key
	return key
}

// responsesItemKey prefers the item's own id, then the event's item_id, then
// a stable fallback from output_index so a delta-first provider that never
// sends an id still demuxes concurrent calls by position.
func responsesItemKey(itemID, eventItemID string, outputIndex int) string {
	if itemID != "" {
		return itemID
	}
	if eventItemID != "" {
		return eventItemID
	}
	return fmt.Sprintf("idx:%d", outputIndex)
}

func (s *streamState) respTool(key string) *respPendingTool {
	if t := s.lookupRespTool(key); t != nil {
		return t
	}
	if s.respPending == nil {
		s.respPending = make(map[string]*respPendingTool)
	}
	t := &respPendingTool{}
	s.respPending[key] = t
	s.respOrder = append(s.respOrder, key)
	return t
}

func (s *streamState) lookupRespTool(key string) *respPendingTool {
	if s.respPending == nil {
		return nil
	}
	return s.respPending[key]
}

func (s *streamState) dropRespTool(key string) {
	delete(s.respPending, key)
	for i, k := range s.respOrder {
		if k == key {
			s.respOrder = append(s.respOrder[:i], s.respOrder[i+1:]...)
			return
		}
	}
}

func (s *streamState) emitResponsesTool(t *respPendingTool) {
	s.closeBlock()
	s.pendingID = t.id
	s.pendingName = t.name
	s.pendingArgs = t.args
	s.havePending = true
	if s.pendingName == "" {
		s.pendingName = "unknown_tool"
	}
	s.openToolBlock()
	s.closeBlock()
	s.respVisible = append(s.respVisible, anthropic.ContentBlock{
		Type: "tool_use", ID: toolUseID(t.id), Name: t.name, Input: json.RawMessage(t.args),
	})
}

// flushRespTools emits any function_call items that never got an
// output_item.done, in the order they were first seen. Used when the stream
// ends on completed/incomplete without per-item done events.
func (s *streamState) flushRespTools() {
	for _, key := range append([]string(nil), s.respOrder...) {
		if t := s.respPending[key]; t != nil {
			s.emitResponsesTool(t)
			s.markResponsesEmitted(key)
		}
	}
	s.respPending = nil
	s.respOrder = nil
}
