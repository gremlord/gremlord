package cmd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/store"
)

func seed(t *testing.T, path string, at ...time.Time) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, ts := range at {
		if err := st.RecordUsage(store.UsageEvent{
			TS: ts, Model: "m", CostUSD: 0.01, Priced: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// The case that matters: an agentic router kept logging after the copy, so the
// old database holds events gremlord cannot see.
func TestLegacySpendLagDetectsDivergence(t *testing.T) {
	dir := t.TempDir()
	oldDB := filepath.Join(dir, "old.db")
	newDB := filepath.Join(dir, "new.db")

	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	seed(t, newDB, base)
	seed(t, oldDB, base, base.Add(30*time.Minute), base.Add(time.Hour))

	lag, rows, diverged := legacySpendLag(oldDB, newDB)
	if !diverged {
		t.Fatal("want divergence reported")
	}
	if rows != 2 {
		t.Errorf("rows = %d, want 2", rows)
	}
	if lag < 55*time.Minute || lag > 65*time.Minute {
		t.Errorf("lag = %v, want about an hour", lag)
	}
}

// A finished migration must stay silent, including when the two databases are
// identical -- otherwise every run nags about nothing.
func TestLegacySpendLagQuietWhenInSync(t *testing.T) {
	dir := t.TempDir()
	oldDB := filepath.Join(dir, "old.db")
	newDB := filepath.Join(dir, "new.db")
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	seed(t, oldDB, at)
	seed(t, newDB, at)

	if _, _, diverged := legacySpendLag(oldDB, newDB); diverged {
		t.Error("identical databases should not report divergence")
	}
}

func TestLegacySpendLagQuietWhenLegacyMissing(t *testing.T) {
	dir := t.TempDir()
	newDB := filepath.Join(dir, "new.db")
	seed(t, newDB, time.Now())
	if _, _, diverged := legacySpendLag(filepath.Join(dir, "absent.db"), newDB); diverged {
		t.Error("a missing legacy database is the normal finished state")
	}
}
