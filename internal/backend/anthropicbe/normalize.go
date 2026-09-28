package anthropicbe

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/gremlord/gremlord/internal/rawjson"
)

// rewriteForModel swaps the model field and normalizes capability-gated
// parameters for the target model. Needed because Claude Code picks its
// thinking/sampling config by pattern-matching the model NAME — an alias
// like "auto" or "cheap" matches nothing, so it sends legacy parameters
// that current Claude models reject.
//
// When normalization changes nothing but the model — the usual case for an
// `auto` or tier alias resolving to a current Claude model — only the model
// value is spliced, so the rest of the body (safeguards, cache_control,
// system block order, escaping) reaches Anthropic byte-for-byte. Only a body
// that genuinely needs repair is re-encoded.
func rewriteForModel(raw []byte, model string) ([]byte, error) {
	m, err := decodeRequest(raw)
	if err != nil {
		return nil, err
	}
	orig, _ := decodeRequest(raw)
	m["model"] = model
	orig["model"] = model
	normalizeForModel(m, model)
	if reflect.DeepEqual(m, orig) {
		enc, err := rawjson.Marshal(model)
		if err != nil {
			return nil, err
		}
		if out, ok := rawjson.Replace(raw, enc, "model"); ok {
			return out, nil
		}
	}
	return rawjson.Marshal(m)
}

func decodeRequest(raw []byte) (map[string]any, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func normalizeForModel(m map[string]any, model string) {
	// A history may contain tool ids emitted earlier by an OpenAI-compatible
	// backend (notably vLLM ids like "Bash:0"). Anthropic validates both
	// tool_use.id and matching tool_result.tool_use_id against
	// ^[A-Za-z0-9_-]+$; normalize the pair in-flight so already-persisted
	// transcripts recover automatically without destructive file edits.
	sanitizeToolIDs(m)
	stripUnsignedThinking(m)

	switch {
	case strings.HasPrefix(model, "claude-fable") || strings.HasPrefix(model, "claude-mythos"):
		// Thinking is always on; any explicit config (enabled, disabled,
		// budget_tokens) returns a 400. Sampling params are removed.
		delete(m, "thinking")
		deleteSampling(m)

	case strings.HasPrefix(model, "claude-opus-4-7"),
		strings.HasPrefix(model, "claude-opus-4-8"),
		strings.HasPrefix(model, "claude-opus-5"), // covers claude-opus-5-5
		strings.HasPrefix(model, "claude-sonnet-5"):
		// budget_tokens is removed on these models; adaptive is the only
		// on-mode. Non-default sampling params are rejected.
		if thinkingType(m) == "enabled" {
			m["thinking"] = map[string]any{"type": "adaptive"}
		}
		deleteSampling(m)

	case strings.HasPrefix(model, "claude-opus-4-6"),
		strings.HasPrefix(model, "claude-sonnet-4-6"):
		// budget_tokens is deprecated here; adaptive is supported and
		// preferred. Sampling params are still allowed — leave them.
		if thinkingType(m) == "enabled" {
			m["thinking"] = map[string]any{"type": "adaptive"}
		}
	}
	// Older models (haiku-4-5, sonnet-4-5, …) keep enabled+budget_tokens.

	if !supportsEffort(model) {
		if oc, ok := m["output_config"].(map[string]any); ok {
			delete(oc, "effort")
			if len(oc) == 0 {
				delete(m, "output_config")
			}
		}
	}
	if !supportsSystemMessages(model) {
		// Fold mid-conversation system messages into the preceding user
		// turn so tier switches to an older model don't 400. Models that
		// accept them get the history untouched — folding rewrites earlier
		// turns, which forces a re-encode and can break preserved thinking.
		foldSystemMessages(m)
	}
}

// hasInvalidToolID reports whether raw request history contains a tool id
// outside Anthropic's accepted character set. It intentionally parses only
// the small envelope needed to preserve byte-faithful passthrough for clean
// requests; malformed JSON is handled later by rewriteForModel.
func hasInvalidToolID(raw []byte) bool {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	msgs, _ := m["messages"].([]any)
	for _, rawMsg := range msgs {
		msg, _ := rawMsg.(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			var id string
			switch block["type"] {
			case "tool_use":
				id, _ = block["id"].(string)
			case "tool_result":
				id, _ = block["tool_use_id"].(string)
			}
			if id != "" && validToolID(id) != id {
				return true
			}
		}
	}
	return false
}

// hasUnsignedThinking reports whether translated reasoning has been replayed
// as an Anthropic thinking block without a real signature. Claude Code keeps
// response blocks in history; Anthropic rejects these display-only blocks on
// a later cross-tier turn unless the passthrough takes the repair path.
func hasUnsignedThinking(raw []byte) bool {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	msgs, _ := m["messages"].([]any)
	for _, rawMsg := range msgs {
		msg, _ := rawMsg.(map[string]any)
		if msg["role"] != "assistant" {
			continue
		}
		blocks, _ := msg["content"].([]any)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			if block["type"] == "thinking" && !hasThinkingSignature(block) {
				return true
			}
		}
	}
	return false
}

// stripUnsignedThinking removes only display-only reasoning minted by an
// OpenAI-compatible backend. Genuine Anthropic thinking has a non-empty
// signature and must be replayed byte-for-byte; redacted_thinking uses a
// different data field and is never touched.
func stripUnsignedThinking(m map[string]any) {
	msgs, ok := m["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		out := make([]any, 0, len(blocks))
		var reasoning string
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok || block["type"] != "thinking" || hasThinkingSignature(block) {
				out = append(out, rawBlock)
				continue
			}
			if text, ok := block["thinking"].(string); ok && strings.TrimSpace(text) != "" {
				if reasoning != "" {
					reasoning += "\n"
				}
				reasoning += text
			}
		}
		if len(out) == 0 && reasoning != "" {
			// A reasoning-only translated response would otherwise become an
			// invalid empty assistant message. Preserve it as ordinary context.
			out = append(out, map[string]any{"type": "text", "text": reasoning})
		}
		msg["content"] = out
	}
}

func hasThinkingSignature(block map[string]any) bool {
	signature, ok := block["signature"].(string)
	return ok && signature != ""
}

// sanitizeToolIDs rewrites every invalid tool_use.id and
// tool_result.tool_use_id in messages[] with the same deterministic mapping,
// preserving their references while satisfying Anthropic's id validation.
func sanitizeToolIDs(m map[string]any) {
	msgs, ok := m["messages"].([]any)
	if !ok {
		return
	}
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "tool_use":
				if id, ok := block["id"].(string); ok {
					block["id"] = validToolID(id)
				}
			case "tool_result":
				if id, ok := block["tool_use_id"].(string); ok {
					block["tool_use_id"] = validToolID(id)
				}
			}
		}
	}
}

func validToolID(id string) string {
	if id == "" {
		return "toolu_gremlord_missing"
	}
	clean := true
	for _, r := range id {
		if !validToolIDRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return id
	}
	out := make([]rune, 0, len(id))
	for _, r := range id {
		if validToolIDRune(r) {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

func validToolIDRune(r rune) bool {
	return r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// supportsSystemMessages reports whether the model accepts role "system"
// entries inside messages[]. Verified against the API 2026-09-28: Opus 5.5,
// Sonnet 5, and Fable 5.1 accept them; Haiku 4.5 rejects them.
func supportsSystemMessages(model string) bool {
	for _, prefix := range []string{"claude-opus-4-8", "claude-opus-5", "claude-sonnet-5", "claude-fable"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// supportsEffort reports whether the model accepts output_config.effort
// (Opus 4.5+, Sonnet 4.6+, Fable/Mythos; Haiku and older Sonnets reject it).
func supportsEffort(model string) bool {
	for _, prefix := range []string{
		"claude-fable", "claude-mythos",
		"claude-opus-4-5", "claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8",
		"claude-opus-5", // covers claude-opus-5-5
		"claude-sonnet-4-6", "claude-sonnet-5",
	} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// foldSystemMessages merges {"role":"system"} entries inside messages[]
// into the nearest preceding user message as a <system-reminder> text
// block, preserving role alternation.
func foldSystemMessages(m map[string]any) {
	msgs, ok := m["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "system" {
			out = append(out, raw)
			continue
		}
		text := contentText(msg["content"])
		if text == "" {
			continue
		}
		reminder := map[string]any{"type": "text",
			"text": "<system-reminder>\n" + text + "\n</system-reminder>"}
		// Find the nearest preceding user message to absorb the block.
		folded := false
		for i := len(out) - 1; i >= 0; i-- {
			prev, ok := out[i].(map[string]any)
			if !ok || prev["role"] != "user" {
				continue
			}
			prev["content"] = append(contentBlocks(prev["content"]), reminder)
			folded = true
			break
		}
		if !folded {
			out = append(out, map[string]any{"role": "user", "content": []any{reminder}})
		}
	}
	m["messages"] = out
}

// contentText flattens a message content (string or block list) to text.
func contentText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	text := ""
	for _, b := range contentBlocks(content) {
		if block, ok := b.(map[string]any); ok && block["type"] == "text" {
			if t, ok := block["text"].(string); ok {
				if text != "" {
					text += "\n"
				}
				text += t
			}
		}
	}
	return text
}

// contentBlocks returns content as a block list, converting the string form.
func contentBlocks(content any) []any {
	if blocks, ok := content.([]any); ok {
		return blocks
	}
	if s, ok := content.(string); ok && s != "" {
		return []any{map[string]any{"type": "text", "text": s}}
	}
	return nil
}

func thinkingType(m map[string]any) string {
	t, _ := m["thinking"].(map[string]any)
	s, _ := t["type"].(string)
	return s
}

func deleteSampling(m map[string]any) {
	delete(m, "temperature")
	delete(m, "top_p")
	delete(m, "top_k")
}
