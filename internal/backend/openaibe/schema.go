package openaibe

import (
	"encoding/json"
)

// sanitizeToolSchema strips JSON Schema "pattern" constraints that use
// Unicode property escapes (\p{...}, \P{...}). Claude Code's own built-in
// tools (Artifact among them) emit such patterns, which Go's regexp package
// compiles fine but OpenAI's function-calling schema validator rejects
// outright with "is not a 'regex'" — turning every request that offers the
// tool into a hard 400, not a degraded one. Dropping the unsupported
// constraint costs input validation Claude Code never relied on gremlord to
// enforce anyway; keeping the tool callable is worth more than keeping the
// pattern.
//
// On any unmarshal/marshal failure the original schema is returned
// unchanged — a schema this can't safely rewrite is better sent as-is than
// silently dropped.
func sanitizeToolSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	changed := stripUnsupportedPatterns(v)
	if !changed {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// stripUnsupportedPatterns walks a decoded JSON Schema (as produced by
// encoding/json into map[string]any / []any) and deletes any "pattern" key
// whose value uses a Unicode property escape. It recurses into every map
// value and slice element — not just known schema keywords — since a
// pattern can appear nested under properties, items, additionalProperties,
// anyOf/oneOf/allOf, $defs/definitions, or a provider-specific extension.
func stripUnsupportedPatterns(v any) bool {
	changed := false
	switch node := v.(type) {
	case map[string]any:
		if p, ok := node["pattern"].(string); ok && hasUnicodePropertyEscape(p) {
			delete(node, "pattern")
			changed = true
		}
		for _, child := range node {
			if stripUnsupportedPatterns(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range node {
			if stripUnsupportedPatterns(child) {
				changed = true
			}
		}
	}
	return changed
}

// hasUnicodePropertyEscape reports whether a regex pattern contains a
// \p{...} or \P{...} Unicode property/script escape — supported by Go's
// RE2-derived regexp package but rejected by OpenAI's schema validator.
func hasUnicodePropertyEscape(pattern string) bool {
	for i := 0; i+2 < len(pattern); i++ {
		if pattern[i] == '\\' && (pattern[i+1] == 'p' || pattern[i+1] == 'P') && pattern[i+2] == '{' {
			return true
		}
	}
	return false
}
