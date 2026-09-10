package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRouteDecisionRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// No decision recorded yet.
	if _, _, _, _, ok, err := st.LatestRouteDecision("sess-1"); ok || err != nil {
		t.Errorf("empty lookup: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	if err := st.RecordRouteDecision("sess-1", "auto", "deep", "opus", "size:light→deep", time.Now()); err != nil {
		t.Fatal(err)
	}
	alias, tier, model, reason, ok, err := st.LatestRouteDecision("sess-1")
	if err != nil || !ok || alias != "auto" || tier != "deep" || model != "opus" || reason != "size:light→deep" {
		t.Errorf("got alias=%s tier=%s model=%s reason=%q ok=%v err=%v, want auto/deep/opus/\"size:light→deep\"/true/nil",
			alias, tier, model, reason, ok, err)
	}

	// A later decision for the same session overwrites, not duplicates.
	if err := st.RecordRouteDecision("sess-1", "auto", "light", "qwen", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	alias, tier, model, reason, ok, err = st.LatestRouteDecision("sess-1")
	if err != nil || !ok || alias != "auto" || tier != "light" || model != "qwen" || reason != "" {
		t.Errorf("after overwrite: alias=%s tier=%s model=%s reason=%q ok=%v err=%v, want auto/light/qwen/\"\"/true/nil",
			alias, tier, model, reason, ok, err)
	}

	// A different session is unaffected.
	if _, _, _, _, ok, err := st.LatestRouteDecision("sess-2"); ok || err != nil {
		t.Errorf("other session: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestRouteEventsAreAppendOnly(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	at := time.Now().Truncate(time.Second)
	if err := st.RecordRouteDecision("sess-1", "auto", "deep", "opus", "first", at); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRouteDecision("sess-1", "auto", "light", "qwen", "second", at); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRouteDecision("sess-2", "auto", "standard", "sonnet", "other", at); err != nil {
		t.Fatal(err)
	}

	events, err := st.RouteEvents("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want two", events)
	}
	if events[0].Model != "opus" || events[0].Reason != "first" || events[1].Model != "qwen" || events[1].Reason != "second" {
		t.Errorf("events = %+v, want insertion order", events)
	}
	if other, err := st.RouteEvents("sess-2"); err != nil || len(other) != 1 || other[0].Model != "sonnet" {
		t.Errorf("other events = %+v, err=%v", other, err)
	}
}

func TestSessionUsageIncludesDuration(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	at := time.Now().Truncate(time.Second)
	want := UsageEvent{
		TS: at, SessionID: "sess-1", Profile: "main", Provider: "anthropic",
		Model: "claude", Alias: "opus", InputTokens: 10, OutputTokens: 20,
		CacheReadTokens: 3, CacheWriteTokens: 4, CostUSD: 0.5, Priced: true,
		RequestID: "req-1", Status: 200, CtxBudget: 1000, ReportedInput: 17,
		DurationMS: 1234,
	}
	if err := st.RecordUsage(want); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordUsage(UsageEvent{TS: at, SessionID: "sess-2", DurationMS: 99}); err != nil {
		t.Fatal(err)
	}

	got, err := st.SessionUsage("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("usage = %+v, want one", got)
	}
	if got[0].DurationMS != 1234 || got[0].InputTokens != 10 || got[0].ReportedInput != 17 || !got[0].Priced {
		t.Errorf("usage = %+v", got[0])
	}
}

func TestSpendSinceFilteredAndSessionBounds(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	at := time.Now().Truncate(time.Second)
	for _, e := range []UsageEvent{
		{TS: at, SessionID: "sess-a", Profile: "main", Model: "opus", InputTokens: 10, CacheReadTokens: 5, OutputTokens: 2, CostUSD: 1.25, Priced: true},
		{TS: at.Add(time.Minute), SessionID: "sess-a", Profile: "main", Model: "haiku", InputTokens: 3, OutputTokens: 1, CostUSD: 0.10, Priced: true},
		{TS: at, SessionID: "sess-b", Profile: "cheap", Model: "opus", InputTokens: 99, CostUSD: 9.00, Priced: true},
	} {
		if err := st.RecordUsage(e); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.SpendSince(at.Add(-time.Hour), "model")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered models = %+v, want opus+haiku", all)
	}

	filtered, err := st.SpendSinceFiltered(at.Add(-time.Hour), "model", "sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered = %+v, want two models", filtered)
	}
	var total float64
	for _, r := range filtered {
		total += r.CostUSD
		if r.Key == "opus" && (r.CostUSD != 1.25 || r.InputTokens != 15) {
			t.Errorf("opus row = %+v, want cost 1.25 input 15", r)
		}
	}
	if total != 1.35 {
		t.Errorf("sess-a total = %v, want 1.35", total)
	}

	bounds, ok, err := st.SessionBounds("sess-a")
	if err != nil || !ok {
		t.Fatalf("bounds ok=%v err=%v", ok, err)
	}
	if bounds.Profile != "main" || bounds.Requests != 2 || !bounds.First.Equal(at) || !bounds.Last.Equal(at.Add(time.Minute)) {
		t.Errorf("bounds = %+v", bounds)
	}
	if _, ok, err := st.SessionBounds("sess-missing"); err != nil || ok {
		t.Errorf("missing: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestGoalDecisionRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// No decision recorded yet.
	if _, _, ok, err := st.LatestGoalDecision("sess-1"); ok || err != nil {
		t.Errorf("empty lookup: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	if err := st.RecordGoalDecision("sess-1", true, "polling a long build", time.Now()); err != nil {
		t.Fatal(err)
	}
	goal, reason, ok, err := st.LatestGoalDecision("sess-1")
	if err != nil || !ok || !goal || reason != "polling a long build" {
		t.Errorf("got goal=%v reason=%q ok=%v err=%v, want true/\"polling a long build\"/true/nil",
			goal, reason, ok, err)
	}

	// A later decision for the same session overwrites, not duplicates.
	if err := st.RecordGoalDecision("sess-1", false, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	goal, reason, ok, err = st.LatestGoalDecision("sess-1")
	if err != nil || !ok || goal || reason != "" {
		t.Errorf("after overwrite: goal=%v reason=%q ok=%v err=%v, want false/\"\"/true/nil",
			goal, reason, ok, err)
	}

	// A different session is unaffected.
	if _, _, ok, err := st.LatestGoalDecision("sess-2"); ok || err != nil {
		t.Errorf("other session: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestActiveSessions(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().Truncate(time.Second)
	st.StartSession("sess-live", "main", "/tmp/a", now.Add(-time.Hour))
	st.StartSession("sess-done", "main", "/tmp/b", now.Add(-2*time.Hour))
	st.EndSession("sess-done", now)
	st.RecordUsage(UsageEvent{TS: now.Add(-time.Minute), SessionID: "sess-live", InputTokens: 10})

	active, err := st.ActiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != "sess-live" {
		t.Fatalf("active = %+v, want just sess-live", active)
	}
	a := active[0]
	if a.Profile != "main" || a.WorkDir != "/tmp/a" {
		t.Errorf("session fields: %+v", a)
	}
	if !a.StartedAt.Equal(now.Add(-time.Hour)) {
		t.Errorf("StartedAt = %v", a.StartedAt)
	}
	if !a.LastSeen.Equal(now.Add(-time.Minute)) {
		t.Errorf("LastSeen = %v, want usage event time", a.LastSeen)
	}

	// A session with no usage has zero LastSeen.
	st.StartSession("sess-quiet", "cheap", "/tmp/c", now)
	active, _ = st.ActiveSessions()
	if len(active) != 2 || active[0].ID != "sess-quiet" || !active[0].LastSeen.IsZero() {
		t.Errorf("active = %+v, want sess-quiet first with zero LastSeen", active)
	}
}
