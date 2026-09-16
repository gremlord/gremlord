package openaibe

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/openai"
	"github.com/gremlord/gremlord/internal/tokens"
	"github.com/gremlord/gremlord/internal/wire"
)

// Responses output cannot be represented faithfully by Anthropic thinking
// blocks. Keep original items server-side and restore only exact transcript
// continuations. Prefix hashes separate subagents, retries and forks even
// when they share a launch session. No ciphertext is put in client signatures,
// logs or persistent storage. A cache miss simply uses ordinary translated input.
type responsesContinuity struct {
	mu         sync.Mutex
	entries    map[[32]byte][]*list.Element
	lru        list.List
	bytes      int
	maxBytes   int
	maxEntries int
	ttl        time.Duration
	now        func() time.Time
}

type responsesContinuation struct {
	prefix          [32]byte
	visible         [][32]byte
	items           []openai.ResponsesInputItem
	callIDs         map[string]string
	size            int
	used            time.Time
	contextTokens   int64
	visibleEstimate int64
}

type responsesTurn struct {
	cache           *responsesContinuity
	prefix          [32]byte
	inputEstimate   int64
	visibleEstimate int64
	calibration     tokens.Calibration
	model           string
}

func newResponsesContinuity() *responsesContinuity {
	return &responsesContinuity{
		entries:  make(map[[32]byte][]*list.Element),
		maxBytes: 64 << 20, maxEntries: 512, ttl: 24 * time.Hour, now: time.Now,
	}
}

// prepare replaces matched visible output spans with original Responses items.
// It also snapshots the pre-replay history hash for recording this response.
func (c *responsesContinuity) prepare(call *backend.Call, req *openai.ResponsesRequest, estimate int64) *responsesTurn {
	session := wire.Session(call.Header)
	if session == "" {
		return nil // no safe session boundary for arbitrary API clients
	}
	scope, _ := json.Marshal([]string{session, call.Route.ProviderName,
		call.Route.Provider.BaseURL, call.Route.Provider.Key(), req.Model, req.Instructions})
	prefix := sha256.Sum256(scope)
	input := normalizeResponsesInput(req.Input)
	prints := responsesFingerprints(input)
	var replay []openai.ResponsesInputItem
	callIDs := make(map[string]string)
	measuredEstimate := int64(0)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expire()
	for i := 0; i < len(input); {
		var match *responsesContinuation
		for _, el := range c.entries[prefix] {
			e := el.Value.(*responsesContinuation)
			if len(e.visible) <= len(prints)-i && equalPrints(e.visible, prints[i:i+len(e.visible)]) {
				match = e
				e.used = c.now()
				c.lru.MoveToFront(el)
				break
			}
		}
		n := 1
		if match != nil {
			// Like Codex's history accounting: last measured input + output,
			// plus newly added visible context. Never tokenize ciphertext as text.
			if match.contextTokens > 0 {
				measuredEstimate = match.contextTokens + max(int64(0), estimate-match.visibleEstimate)
			}
			replay = append(replay, match.items...)
			for clientID, upstreamID := range match.callIDs {
				callIDs[clientID] = upstreamID
			}
			n = len(match.visible)
		} else {
			item := input[i]
			if item.Type == "function_call_output" && callIDs[item.CallID] != "" {
				item.CallID = callIDs[item.CallID]
			}
			replay = append(replay, item)
		}
		for _, p := range prints[i : i+n] {
			prefix = responsesHashNext(prefix, p)
		}
		i += n
	}
	req.Input = replay
	return &responsesTurn{cache: c, prefix: prefix,
		inputEstimate: max(estimate, measuredEstimate), visibleEstimate: estimate,
		calibration: call.Calibration, model: req.Model}
}

// remember runs before message_stop (or writing JSON), so an immediate tool
// result request cannot race ahead of its continuation becoming available.
func (t *responsesTurn) remember(resp *openai.ResponsesResponse, streamed ...anthropic.MessageBody) {
	if t == nil || resp == nil || resp.Status != "completed" || resp.Error != nil {
		return
	}
	translated, err := TranslateResponsesResponse(resp, "")
	if err != nil {
		return
	}
	content := anthropic.MessageBody(translated.Content)
	if len(streamed) != 0 {
		content = streamed[0]
	}
	visible, err := translateResponsesMessage(anthropic.Message{Role: "assistant", Content: content})
	if err != nil || len(visible) == 0 {
		return // reasoning-only output has no unambiguous client-visible anchor
	}
	if len(streamed) != 0 {
		projected, err := translateResponsesMessage(anthropic.Message{Role: "assistant", Content: translated.Content})
		if err != nil || !sameResponsesOutput(projected, visible) {
			// A partial terminal payload must never replace a complete client
			// transcript and silently erase calls/text on the following turn.
			return
		}
	}
	e := &responsesContinuation{prefix: t.prefix,
		visible: responsesFingerprints(normalizeResponsesInput(visible)), callIDs: make(map[string]string)}
	e.visibleEstimate = t.visibleEstimate + t.calibration.Apply(t.model,
		tokens.Estimate(&anthropic.MessagesRequest{Messages: []anthropic.Message{{Role: "assistant", Content: content}}}))
	if resp.Usage != nil {
		e.contextTokens = resp.Usage.InputTokens + resp.Usage.OutputTokens
	}
	for _, item := range resp.Output {
		switch item.Type {
		case "reasoning":
			if item.EncryptedContent == "" {
				continue // an unstored reasoning id alone cannot be retrieved later
			}
		case "function_call":
			if item.CallID == "" || item.Name == "" || !json.Valid([]byte(item.Arguments)) {
				return
			}
			e.callIDs[toolUseID(item.CallID)] = item.CallID
		case "message":
		default:
			return // do not partially replay unsupported hosted-tool output
		}
		raw := item.Raw
		if len(raw) == 0 {
			raw, _ = json.Marshal(item)
		}
		if item.Type == "reasoning" {
			// summary is required on replay, including when there is no
			// user-visible summary (some compatible providers omit it).
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil {
				return
			}
			if len(fields["summary"]) == 0 || string(fields["summary"]) == "null" {
				fields["summary"] = json.RawMessage(`[]`)
				raw, _ = json.Marshal(fields)
			}
		}
		e.items = append(e.items, openai.ResponsesInputItem{Replay: raw})
		e.size += len(raw)
	}
	e.size += len(e.visible)*sha256.Size + 256
	for k, v := range e.callIDs {
		e.size += len(k) + len(v) + 64
	}
	c := t.cache
	if e.size > c.maxBytes || len(e.items) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expire()
	// A retry with identical visible output supersedes the prior response.
	for _, el := range c.entries[e.prefix] {
		if equalPrints(el.Value.(*responsesContinuation).visible, e.visible) {
			c.remove(el)
			break
		}
	}
	e.used = c.now()
	c.entries[e.prefix] = append(c.entries[e.prefix], c.lru.PushFront(e))
	c.bytes += e.size
	for c.bytes > c.maxBytes || c.lru.Len() > c.maxEntries {
		c.remove(c.lru.Back())
	}
}

func (c *responsesContinuity) expire() {
	for el := c.lru.Back(); el != nil; el = c.lru.Back() {
		if c.now().Sub(el.Value.(*responsesContinuation).used) < c.ttl {
			break
		}
		c.remove(el)
	}
}

func (c *responsesContinuity) remove(el *list.Element) {
	e := el.Value.(*responsesContinuation)
	for i, candidate := range c.entries[e.prefix] {
		if candidate == el {
			v := c.entries[e.prefix]
			c.entries[e.prefix] = append(v[:i], v[i+1:]...)
			break
		}
	}
	if len(c.entries[e.prefix]) == 0 {
		delete(c.entries, e.prefix)
	}
	c.bytes -= e.size
	c.lru.Remove(el)
}

// Claude Code may split one assistant response across several messages and
// reserialize tool arguments. Normalize those representational differences,
// preserving numbers exactly and all meaningful text, roles, order and IDs.
func normalizeResponsesInput(input []openai.ResponsesInputItem) []openai.ResponsesInputItem {
	out := make([]openai.ResponsesInputItem, 0, len(input))
	for _, item := range input {
		if item.Type == "function_call" {
			var value any
			d := json.NewDecoder(bytes.NewBufferString(item.Arguments))
			d.UseNumber()
			if json.Valid([]byte(item.Arguments)) && d.Decode(&value) == nil {
				b, _ := json.Marshal(value)
				item.Arguments = string(b)
			}
		}
		if item.Type == "message" && item.Role == "assistant" && len(out) > 0 {
			prev := &out[len(out)-1]
			a, aok := prev.Content.([]openai.ResponsesContentPart)
			b, bok := item.Content.([]openai.ResponsesContentPart)
			if prev.Role == "assistant" && aok && bok && len(a) == 1 && len(b) == 1 && a[0].Type == "output_text" && b[0].Type == "output_text" {
				prev.Content = []openai.ResponsesContentPart{{Type: "output_text", Text: a[0].Text + "\n" + b[0].Text}}
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func responsesFingerprints(input []openai.ResponsesInputItem) [][32]byte {
	out := make([][32]byte, len(input))
	for i, item := range input {
		b, _ := json.Marshal(item)
		out[i] = sha256.Sum256(b)
	}
	return out
}

func responsesHashNext(prefix, item [32]byte) [32]byte {
	var b [64]byte
	copy(b[:32], prefix[:])
	copy(b[32:], item[:])
	return sha256.Sum256(b[:])
}

func equalPrints(a, b [][32]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameResponsesOutput(a, b []openai.ResponsesInputItem) bool {
	// Parallel calls may complete out of output-index order. The multiset
	// must still contain exactly the output the client received.
	counts := make(map[[32]byte]int)
	for _, p := range responsesFingerprints(normalizeResponsesInput(a)) {
		counts[p]++
	}
	for _, p := range responsesFingerprints(normalizeResponsesInput(b)) {
		counts[p]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}
