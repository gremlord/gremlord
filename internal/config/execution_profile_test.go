package config

import (
	"fmt"
	"testing"
)

func TestExecutionProfileValidation(t *testing.T) {
	for _, tc := range []struct {
		provider, model, profile string
		valid                    bool
	}{
		{"type: openai, api: responses", "", "gpt-efficient-v1", true},
		{"type: openai", "api: responses,", "gpt-efficient-v1", true},
		{"type: openai", "", "gpt-efficient-v1", false},
		{"type: openai, api: responses", "api: chat_completions,", "gpt-efficient-v1", false},
		{"type: anthropic", "", "gpt-efficient-v1", false},
		{"type: cli, dialect: codex", "", "gpt-efficient-v1", false},
		{"type: openai, api: responses", "", "typo", false},
		{"type: openai", "", "", true},
	} {
		raw := fmt.Sprintf("providers:\n  p: {%s, base_url: https://example.invalid}\nmodels:\n  m: {provider: p, id: gpt-test, %s execution_profile: %q}\n", tc.provider, tc.model, tc.profile)
		_, err := Parse([]byte(raw))
		if (err == nil) != tc.valid {
			t.Errorf("%s / %s / %s: err=%v", tc.provider, tc.model, tc.profile, err)
		}
	}
}
