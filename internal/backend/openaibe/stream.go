package openaibe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/openai"
	"github.com/gremlord/gremlord/internal/tokens"
)

// streamState turns an OpenAI chunk stream into the exact Anthropic SSE
// grammar Claude Code's SDK validates:
//
//	message_start → ping → { content_block_start → delta* → stop }* →
//	message_delta(stop_reason+usage) → message_stop
type streamState struct {
	sse   *anthropic.SSEWriter
	alias string

	started      bool
	sawChunk     bool   // a real upstream chunk arrived (vs synthetic keep-alive start)
	index        int    // next content block index
	openType     string // "", "thinking", "text", "tool"
	openaiToolIx int    // openai tool_call index of the open tool block
	openToolID   string // openai tool_call id of the open tool block
	pendingArgs  string // tool args buffered before the block could open
	pendingID    string
	pendingName  string
	havePending  bool

	finishReason   string
	sawToolCall    bool
	usage          anthropic.Usage // true upstream usage; scaled only at emit time
	scale          float64         // context-scaling factor for client-facing usage
	estInput       int64           // scaled input estimate for message_start (see startMessage)
	keepAliveEvery time.Duration   // ping cadence while the upstream is quiet
	idleTimeout    time.Duration   // give up if the upstream sends nothing at all for this long
}

// maxIdleStream is how long Run waits for upstream activity — a scanner
// line, in either direction — before giving up. Distinct from
// keepAliveEvery (which pings the *client*): a vLLM box can stall
// byte-silently without closing the connection, and NewTransport's
// ResponseHeaderTimeout only guards the initial headers, not an
// already-established stream. Generous because slow reasoning models can
// legitimately sit quiet for a minute before the first token.
const maxIdleStream = 5 * time.Minute

func newStreamState(sse *anthropic.SSEWriter, alias string) *streamState {
	return &streamState{sse: sse, alias: alias, openaiToolIx: -1, scale: 1,
		keepAliveEvery: 15 * time.Second, idleTimeout: maxIdleStream}
}

// Run consumes the upstream SSE body until EOF, [DONE], ctx cancellation, or
// idleTimeout of silence — returning final usage. A mid-stream upstream
// error is forwarded as an Anthropic error event (Claude Code retries on
// it). While the upstream is quiet — slow reasoning models can sit for a
// minute before the first token — pings keep the client from timing out on
// a byte-silent connection.
func (s *streamState) Run(ctx context.Context, body io.Reader) (anthropic.Usage, string) {
	lines := make(chan []byte)
	scanErr := make(chan error, 1)
	quit := make(chan struct{}) // frees the reader if Run returns before EOF
	defer close(quit)
	go func() {
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			case <-quit:
				return
			}
		}
		scanErr <- scanner.Err()
	}()

	keepAlive := time.NewTicker(s.keepAliveEvery)
	defer keepAlive.Stop()
	idle := time.NewTimer(s.idleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			// Client hung up or the turn was cancelled; the transport closes
			// the upstream body for us, so just stop translating.
			return s.usage, "client_disconnect"
		case <-idle.C:
			// Upstream has produced nothing — not even scanner noise — for
			// idleTimeout. Without this the select loop above waits on
			// ctx.Done() forever: a stalled vLLM box that never closes the
			// connection hangs the request indefinitely (the reported "stuck,
			// needs a manual interrupt" symptom).
			s.sse.ErrorEvent("api_error", fmt.Sprintf("upstream stream: no data for %s, giving up", s.idleTimeout))
			return s.usage, "api_error"
		case <-keepAlive.C:
			s.ping()
		case err := <-scanErr:
			if err != nil {
				s.sse.ErrorEvent("api_error", "upstream stream: "+err.Error())
				return s.usage, "api_error"
			}
			return s.usage, s.finalize()
		case line := <-lines:
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(s.idleTimeout)
			data, ok := bytes.CutPrefix(line, []byte("data: "))
			if !ok {
				continue
			}
			if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				return s.usage, s.finalize()
			}
			var chunk openai.Chunk
			if err := json.Unmarshal(data, &chunk); err != nil {
				continue // tolerate provider noise between data lines
			}
			if chunk.Error != nil {
				s.sse.ErrorEvent("api_error", "upstream: "+chunk.Error.Message)
				return s.usage, "api_error"
			}
			s.handleChunk(&chunk)
		}
	}
}

// ping keeps the client connection alive while the upstream is quiet. The
// SSE grammar requires message_start first, so if no chunk has arrived yet
// the message is opened with a synthetic id (handleChunk won't re-open it).
func (s *streamState) ping() {
	if !s.started {
		s.startMessage("msg_gremlord_pending")
		return // startMessage already pings
	}
	s.sse.Ping()
}

// startMessage emits message_start + ping once; later calls are no-ops.
// Claude Code's context gauge reads input tokens from message_start's usage
// (the Anthropic API reports them there), NOT from message_delta — with a
// zero here, auto-compact never fires on translated models. OpenAI upstreams
// only report usage in the final chunk, so message_start carries the local
// (scaled, bias-high) estimate and message_delta the true scaled numbers.
func (s *streamState) startMessage(id string) {
	if s.started {
		return
	}
	s.started = true
	s.sse.Event("message_start", map[string]any{
		"type": "message_start",
		"message": anthropic.MessagesResponse{
			ID: id, Type: "message", Role: "assistant",
			Model: s.alias, Content: []anthropic.ContentBlock{},
			Usage: anthropic.Usage{InputTokens: s.estInput},
		},
	})
	s.sse.Ping()
}

func (s *streamState) handleChunk(chunk *openai.Chunk) {
	s.sawChunk = true
	s.startMessage("msg_" + chunk.ID)
	if chunk.Usage != nil {
		s.usage = mapUsage(chunk.Usage)
	}
	if len(chunk.Choices) == 0 {
		return // usage-only final chunk
	}
	choice := chunk.Choices[0]
	delta := choice.Delta

	if r := reasoningText(delta.ReasoningContent, delta.Reasoning); r != "" {
		s.ensureBlock("thinking")
		s.sse.Event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.index - 1,
			"delta": map[string]string{"type": "thinking_delta", "thinking": r},
		})
	}
	if delta.Content != "" {
		s.ensureBlock("text")
		s.sse.Event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.index - 1,
			"delta": map[string]string{"type": "text_delta", "text": delta.Content},
		})
	}
	for _, tc := range delta.ToolCalls {
		s.handleToolDelta(tc)
	}
	if choice.FinishReason != "" {
		s.finishReason = choice.FinishReason
	}
}

func (s *streamState) handleToolDelta(tc openai.ToolCall) {
	// A tool call is in progress while its block is open or its header is
	// still buffering. A new call is signaled by a different openai index,
	// or — for providers that omit the index — by a fresh id. Fragments with
	// neither continue the call in progress.
	inTool := s.openType == "tool" || s.havePending
	newTool := !inTool
	if tc.Index != nil {
		newTool = newTool || *tc.Index != s.openaiToolIx
	} else if tc.ID != "" && tc.ID != s.currentToolID() {
		newTool = true
	}
	if newTool {
		s.closeBlock()
		s.openaiToolIx = -1
		if tc.Index != nil {
			s.openaiToolIx = *tc.Index
		}
		s.pendingID, s.pendingName, s.pendingArgs = tc.ID, tc.Function.Name, ""
		s.havePending = true
	} else if s.havePending {
		// Still waiting for a name — some providers split id/name/args.
		if tc.ID != "" {
			s.pendingID = tc.ID
		}
		if tc.Function.Name != "" {
			s.pendingName = tc.Function.Name
		}
	}
	if s.havePending {
		s.pendingArgs += tc.Function.Arguments
		if s.pendingName == "" {
			return // can't open the block without a name yet
		}
		s.openToolBlock()
		return
	}
	if tc.Function.Arguments != "" {
		s.argsDelta(tc.Function.Arguments)
	}
}

// currentToolID is the openai id of the call in progress: the buffered
// header's id before the block opens, the open block's id after.
func (s *streamState) currentToolID() string {
	if s.havePending {
		return s.pendingID
	}
	return s.openToolID
}

func (s *streamState) openToolBlock() {
	id := s.pendingID
	if id == "" {
		id = "toolu_gremlord_missing"
	} else {
		id = sanitizeToolUseID(id)
	}
	s.sse.Event("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.index,
		"content_block": map[string]any{"type": "tool_use", "id": id, "name": s.pendingName, "input": map[string]any{}},
	})
	s.openType = "tool"
	s.openToolID = s.pendingID
	s.sawToolCall = true
	s.index++
	args := s.pendingArgs
	s.havePending = false
	s.pendingID, s.pendingName, s.pendingArgs = "", "", ""
	if args != "" {
		s.argsDelta(args)
	}
}

func (s *streamState) argsDelta(fragment string) {
	s.sse.Event("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.index - 1,
		"delta": map[string]string{"type": "input_json_delta", "partial_json": fragment},
	})
}

// ensureBlock opens a block of the wanted type, closing any other open one.
func (s *streamState) ensureBlock(kind string) {
	if s.openType == kind {
		return
	}
	s.closeBlock()
	block := map[string]any{"type": kind}
	if kind == "text" {
		block["text"] = ""
	} else {
		block["thinking"] = ""
	}
	s.sse.Event("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.index, "content_block": block,
	})
	s.openType = kind
	s.index++
}

func (s *streamState) closeBlock() {
	if s.havePending {
		// Tool block that never got a name — open it with a placeholder so
		// buffered args aren't lost.
		if s.pendingName == "" {
			s.pendingName = "unknown_tool"
		}
		s.openToolBlock()
	}
	if s.openType == "" {
		return
	}
	if s.openType == "thinking" {
		// Anthropic grammar: signature_delta before closing a thinking block.
		s.sse.Event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.index - 1,
			"delta": map[string]string{"type": "signature_delta", "signature": ""},
		})
	}
	s.sse.Event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index - 1})
	s.openType = ""
	s.openaiToolIx = -1
	s.openToolID = ""
}

// finalize closes the stream grammar, returning a non-empty errType if the
// stream ended without anything usable.
func (s *streamState) finalize() string {
	if !s.sawChunk {
		// Upstream sent nothing usable. Checked against sawChunk, not
		// started: a keep-alive ping may have opened the message
		// synthetically, but an empty stream is still an error — Claude
		// Code retries on the error event, not on an empty message.
		s.sse.ErrorEvent("api_error", "upstream produced no chunks")
		return "api_error"
	}
	s.closeBlock()
	reported := tokens.ScaleUsage(s.usage, s.scale)
	s.sse.Event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": mapFinishReason(s.finishReason, s.sawToolCall), "stop_sequence": nil},
		"usage": map[string]int64{
			"input_tokens":            reported.InputTokens,
			"output_tokens":           reported.OutputTokens,
			"cache_read_input_tokens": reported.CacheReadInputTokens,
		},
	})
	s.sse.Event("message_stop", map[string]any{"type": "message_stop"})
	return ""
}
