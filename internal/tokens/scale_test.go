package tokens

import (
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
)

func TestScaleFactor(t *testing.T) {
	cases := []struct {
		budget int
		want   float64
	}{
		{0, 1},           // unknown → no scaling
		{-5, 1},          // defensive
		{200_000, 1},     // matches the assumed window
		{100_000, 2},     // small model → inflate
		{1_000_000, 0.2}, // big model → deflate so compaction happens later
	}
	for _, tc := range cases {
		if got := ScaleFactor(tc.budget); got != tc.want {
			t.Errorf("ScaleFactor(%d) = %v, want %v", tc.budget, got, tc.want)
		}
	}
}

func TestScaleCountRoundsUp(t *testing.T) {
	if got := ScaleCount(100, 1.001); got != 101 {
		t.Errorf("ScaleCount(100, 1.001) = %d, want 101 (must round up)", got)
	}
	if got := ScaleCount(100, 1); got != 100 {
		t.Errorf("factor 1 must be identity, got %d", got)
	}
	if got := ScaleCount(0, 5); got != 0 {
		t.Errorf("zero stays zero, got %d", got)
	}
}

func TestScaleUsageInputSideOnly(t *testing.T) {
	u := anthropic.Usage{
		InputTokens:              1000,
		OutputTokens:             500,
		CacheReadInputTokens:     2000,
		CacheCreationInputTokens: 300,
	}
	got := ScaleUsage(u, 2)
	want := anthropic.Usage{
		InputTokens:              2000,
		OutputTokens:             500, // output stays true
		CacheReadInputTokens:     4000,
		CacheCreationInputTokens: 600,
	}
	if got != want {
		t.Errorf("ScaleUsage = %+v, want %+v", got, want)
	}
	if u.InputTokens != 1000 {
		t.Error("ScaleUsage must not mutate its argument")
	}
}

// The invariant the whole feature rests on: for any budget, a conversation
// at fraction f of the REAL budget reports fraction ~f of the ASSUMED
// window, never under-reporting.
func TestScaleMapsBudgetOntoAssumedWindow(t *testing.T) {
	for _, budget := range []int{8_000, 32_000, 64_000, 128_000, 200_000, 400_000, 1_000_000} {
		factor := ScaleFactor(budget)
		for _, frac := range []float64{0.1, 0.5, 0.9, 1.0} {
			real := int64(float64(budget) * frac)
			reported := ScaleCount(real, factor)
			gotFrac := float64(reported) / float64(AssumedWindow)
			if gotFrac < frac || gotFrac > frac+0.001 {
				t.Errorf("budget %d at %.0f%% real: reported fraction %.4f", budget, frac*100, gotFrac)
			}
		}
	}
}
