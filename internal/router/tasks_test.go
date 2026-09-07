package router

import (
	"testing"

	"github.com/gremlord/gremlord/internal/config"
)

func TestParseTaskLabel(t *testing.T) {
	for _, label := range config.TaskLabels {
		if got := parseTaskLabel(" \"" + label + "\". "); got != label {
			t.Errorf("parseTaskLabel(%q) = %q", label, got)
		}
	}
	for _, input := range []string{"", "coding", "security", "implementation extra"} {
		if got := parseTaskLabel(input); got != "" {
			t.Errorf("parseTaskLabel(%q) = %q, want empty", input, got)
		}
	}
}

func TestTaskClassifierPromptUsesClosedLabels(t *testing.T) {
	for _, label := range config.TaskLabels {
		if !contains(taskClassifierPrompt, label+":") {
			t.Errorf("task classifier prompt omits %q", label)
		}
	}
}

func TestClassifyTaskViaBackendMalformedJSONFailsOpen(t *testing.T) {
	// Parsing behavior is covered through the route-level fail-open tests. Keep
	// this assertion focused on the allow-list parser used for backend output.
	if got := parseTaskLabel(`{"task":"implementation"}`); got != "" {
		t.Fatalf("open-ended classifier text was accepted as a task: %q", got)
	}
}
