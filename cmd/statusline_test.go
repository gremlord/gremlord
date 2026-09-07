package cmd

import (
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/config"
)

// TestStatusLineBudgetSuffixAttachesToDaySpend guards against a regression
// where the budget suffix ("/$600 [bar]") was appended after the goal
// segment instead of right after the day-spend figure it describes, making
// it visually read as part of the goal reason (see screenshot in the PR).
func TestStatusLineBudgetSuffixAttachesToDaySpend(t *testing.T) {
	budgets := &config.Budget{Daily: 600}
	line := statusLine("main", "auto→opus (deep)", 91.68, 328.95, budgets, "Monitor task completion, wait for CI")

	dayFigure := "day $328.95"
	budgetSuffix := "/$600"
	goalSegment := "⟳ goal"

	dayIdx := strings.Index(line, dayFigure)
	suffixIdx := strings.Index(line, budgetSuffix)
	goalIdx := strings.Index(line, goalSegment)

	if dayIdx == -1 || suffixIdx == -1 || goalIdx == -1 {
		t.Fatalf("line missing an expected segment: %q", line)
	}
	// The budget suffix must immediately follow the day figure (only the
	// closing color-reset/bracket bytes of the day segment between them),
	// and the goal segment must come strictly after the budget suffix.
	if suffixIdx < dayIdx {
		t.Errorf("budget suffix must come after the day figure: %q", line)
	}
	if goalIdx < suffixIdx {
		t.Errorf("goal segment must come after the budget suffix, not before it: %q", line)
	}
	// Nothing (in particular no " · " separator) sits between "day $328.95"
	// and "/$600" — that's what makes the suffix read as attached to day
	// spend rather than floating after an unrelated segment.
	between := line[dayIdx+len(dayFigure) : suffixIdx]
	if strings.Contains(between, "·") {
		t.Errorf("a segment separator sits between day spend and its budget suffix: %q", line)
	}
}

func TestStatusLineNoGoalOmitsSegment(t *testing.T) {
	line := statusLine("main", "sonnet", 1, 2, nil, "")
	if strings.Contains(line, "goal") {
		t.Errorf("no goal reason should mean no goal segment: %q", line)
	}
}

func TestStatusLineNoBudgetOmitsSuffix(t *testing.T) {
	line := statusLine("main", "sonnet", 1, 2, nil, "")
	if strings.Contains(line, "/$") {
		t.Errorf("no budget configured should mean no suffix: %q", line)
	}
}
