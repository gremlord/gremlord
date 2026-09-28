package anthropicbe

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/gremlord/gremlord/internal/rawjson"
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
// Even then only the three input-side usage counters are touched, spliced
// in place by byte offset so every neighbouring byte — key order, escaping,
// safeguard_results and whatever ships next — reaches the client as sent.

var inputCounters = []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"}

// scaleCounters rewrites the input-side counters of the usage object at
// path. Counters that are absent or not integers are left exactly as found.
func scaleCounters(data []byte, factor float64, path ...string) []byte {
	for _, field := range inputCounters {
		p := append(append([]string{}, path...), field)
		s, e, ok := rawjson.Span(data, p...)
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(string(data[s:e]), 10, 64)
		if err != nil {
			continue
		}
		data, _ = rawjson.Replace(data, strconv.AppendInt(nil, tokens.ScaleCount(v, factor), 10), p...)
	}
	return data
}

// scaleResponseBody rewrites usage in a non-streaming Messages response.
// Returns the input unchanged on any parse failure: a response the client
// can read with stale numbers beats one mangled into unreadability.
func scaleResponseBody(body []byte, factor float64) []byte {
	if factor == 1 {
		return body
	}
	return scaleCounters(body, factor, "usage")
}

// scaleSSEData rewrites usage in one SSE `data:` payload, returning the
// payload unchanged when it is not a message_start or cannot be parsed.
// Only message_start carries the input-side counters that feed the
// client's context gauge; message_delta's output tokens stay true.
func scaleSSEData(data []byte, factor float64) []byte {
	if factor == 1 || !bytes.Contains(data, []byte(`"usage"`)) {
		return data
	}
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Type != "message_start" {
		return data
	}
	return scaleCounters(data, factor, "message", "usage")
}
