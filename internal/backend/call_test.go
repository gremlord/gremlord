package backend

import (
	"testing"

	"github.com/gremlord/gremlord/internal/config"
)

func TestScaleBudgetPrefersGauge(t *testing.T) {
	c := &Call{Route: config.Resolved{Model: config.Model{ContextWindow: 32_768}}}
	if got := c.ScaleBudget(); got != 32_768 {
		t.Errorf("ScaleBudget() = %d, want the model's own budget", got)
	}
	c.GaugeBudget = 1_000_000
	if got := c.ScaleBudget(); got != 1_000_000 {
		t.Errorf("ScaleBudget() = %d, want the rule's anchor", got)
	}
}
