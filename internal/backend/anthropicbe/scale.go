package anthropicbe

import (
	"bytes"
	"encoding/json"

	"github.com/gremlord/gremlord/internal/tokens"
)

// The passthrough is byte-faithful by default and that is the whole point
// of it — cache_control, signed thinking blocks, and fields gremlord has
// never heard of survive untouched. Scaling is the one deliberate
// exception, and it stays off (factor 1) unless something asks for it:
//
//   - a routing rule anchored its gauge to a budget other than the
//     client's assumed window, so an Anthropic tier in that rule must
//     report on the same scale as its translated siblings or the gauge
//     jumps on every tier change — exactly what the anchor exists to stop;
//   - an anthropic model declares effective_context, forcing compaction
//     before a real Claude window is full.
//
// Even then only the three input-side usage counters are touched, via a
// generic map so every neighbouring field round-trips as written.

// scaleUsageMap rewrites the input-side counters of a decoded usage
// object in place. Values arrive as json.Number; anything unparseable is
// left exactly as found.
func scaleUsageMap(usage map[string]any, factor float64) {
	for _, field := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		n, ok := usage[field].(json.Number)
		if !ok {
			continue
		}
		v, err := n.Int64()
		if err != nil {
			continue
		}
		usage[field] = tokens.ScaleCount(v, factor)
	}
}

// scaleResponseBody rewrites usage in a non-streaming Messages response.
// Returns the input unchanged on any parse failure: a response the client
// can read with stale numbers beats one mangled into unreadability.
func scaleResponseBody(body []byte, factor float64) []byte {
	if factor == 1 {
		return body
	}
	var m map[string]any
	if err := decodeNumbers(body, &m); err != nil {
		return body
	}
	usage, ok := m["usage"].(map[string]any)
	if !ok {
		return body
	}
	scaleUsageMap(usage, factor)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// scaleSSEData rewrites usage in one SSE `data:` payload, returning the
// payload unchanged when it is not a message_start or cannot be parsed.
// Only message_start carries the input-side counters that feed the
// client's context gauge; message_delta's output tokens stay true.
func scaleSSEData(data []byte, factor float64) []byte {
	if factor == 1 || !bytes.Contains(data, []byte(`"usage"`)) {
		return data
	}
	var m map[string]any
	if err := decodeNumbers(data, &m); err != nil {
		return data
	}
	if t, _ := m["type"].(string); t != "message_start" {
		return data
	}
	msg, ok := m["message"].(map[string]any)
	if !ok {
		return data
	}
	usage, ok := msg["usage"].(map[string]any)
	if !ok {
		return data
	}
	scaleUsageMap(usage, factor)
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

func decodeNumbers(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}
