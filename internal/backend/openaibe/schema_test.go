package openaibe

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeToolSchemaStripsUnicodePropertyPattern(t *testing.T) {
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {
				"type": "string",
				"pattern": "^(?!__.*__$)[^\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}\"\\\\./[\\]]{1,200}$"
			},
			"nested": {
				"anyOf": [
					{"type": "string", "pattern": "\\p{L}+"},
					{"type": "integer"}
				]
			}
		}
	}`)
	out := sanitizeToolSchema(raw)
	if strings.Contains(string(out), "pattern") {
		t.Fatalf("pattern survived sanitization: %s", out)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("sanitized schema is not valid JSON: %v", err)
	}
	props, ok := v["properties"].(map[string]any)
	if !ok || props["title"] == nil {
		t.Fatalf("sanitization dropped unrelated schema content: %s", out)
	}
}

func TestSanitizeToolSchemaLeavesOrdinaryPatternsAlone(t *testing.T) {
	raw := json.RawMessage(`{"type":"string","pattern":"^[a-z0-9_-]{1,64}$"}`)
	out := sanitizeToolSchema(raw)
	if string(out) != string(raw) {
		t.Fatalf("ordinary pattern was rewritten: got %s want %s", out, raw)
	}
}

func TestSanitizeToolSchemaPassesThroughInvalidJSON(t *testing.T) {
	raw := json.RawMessage(`not json`)
	out := sanitizeToolSchema(raw)
	if string(out) != string(raw) {
		t.Fatalf("invalid JSON was mutated: got %s want %s", out, raw)
	}
}

func TestSanitizeToolSchemaEmpty(t *testing.T) {
	if out := sanitizeToolSchema(nil); out != nil {
		t.Fatalf("nil schema should stay nil, got %s", out)
	}
}

func TestHasUnicodePropertyEscape(t *testing.T) {
	cases := map[string]bool{
		`^[a-z]+$`:            false,
		`\p{L}+`:              true,
		`\P{Cc}`:              true,
		`literal \p no brace`: false,
		``:                    false,
	}
	for pattern, want := range cases {
		if got := hasUnicodePropertyEscape(pattern); got != want {
			t.Errorf("hasUnicodePropertyEscape(%q) = %v, want %v", pattern, got, want)
		}
	}
}
