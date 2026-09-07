// Package budget enforces spend caps before requests are forwarded.
package budget

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
)

// Gate holds in-memory spend counters (rebuilt from SQLite on start and on
// window rollover) so budget checks don't hit disk per request.
type Gate struct {
	cfg atomic.Pointer[config.Config]
	st  *store.Store
	log *slog.Logger

	mu      sync.Mutex
	buckets map[string]*bucket // key "" = global, otherwise profile name
	warned  map[string]bool    // scope+window keys that already logged a warning
}

type bucket struct {
	day, week, month                float64
	dayStart, weekStart, monthStart time.Time
}

func NewGate(cfg *config.Config, st *store.Store, log *slog.Logger) *Gate {
	g := &Gate{st: st, log: log, buckets: map[string]*bucket{}, warned: map[string]bool{}}
	g.cfg.Store(cfg)
	return g
}

func (g *Gate) SetConfig(cfg *config.Config) { g.cfg.Store(cfg) }

// Check returns a user-facing refusal message when any applicable cap is
// exhausted (and hard_stop is on), or "" to allow the request. In-flight
// streams are never cut — only the next request is blocked.
func (g *Gate) Check(profile string) string {
	cfg := g.cfg.Load()
	g.mu.Lock()
	defer g.mu.Unlock()

	scopes := []struct {
		key    string
		budget *config.Budget
		label  string
	}{
		{"", cfg.Budgets, "global"},
	}
	if profile != "" {
		if p, ok := cfg.Profiles[profile]; ok && p.Budget != nil {
			scopes = append(scopes, struct {
				key    string
				budget *config.Budget
				label  string
			}{profile, p.Budget, "profile '" + profile + "'"})
		}
	}

	for _, s := range scopes {
		if s.budget == nil {
			continue
		}
		b := g.freshBucket(s.key)
		for _, win := range []struct {
			name  string
			cap   float64
			spent float64
		}{
			{"daily", s.budget.Daily, b.day},
			{"weekly", s.budget.Weekly, b.week},
			{"monthly", s.budget.Monthly, b.month},
		} {
			if win.cap <= 0 {
				continue
			}
			if win.spent >= win.cap && hardStop(s.budget) {
				return fmt.Sprintf("[gremlord] %s budget exceeded ($%.2f of $%.2f for %s). Raise it in ~/.gremlord/config.yaml or run: gremlord cost",
					win.name, win.spent, win.cap, s.label)
			}
			warnAt := s.budget.WarnAt
			if warnAt == 0 {
				warnAt = 0.8
			}
			warnKey := fmt.Sprintf("%s/%s/%s", s.key, win.name, windowStart(win.name, b).Format("2006-01-02"))
			if win.spent >= win.cap*warnAt && !g.warned[warnKey] {
				g.warned[warnKey] = true
				g.log.Warn("budget warning",
					"scope", s.label, "window", win.name,
					"spent", fmt.Sprintf("%.2f", win.spent), "cap", win.cap)
			}
		}
	}
	return ""
}

// Add records spend into the counters after a request completes.
func (g *Gate) Add(profile string, cost float64) {
	if cost == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	keys := []string{""}
	if profile != "" {
		keys = append(keys, profile)
	}
	for _, key := range keys {
		b := g.freshBucket(key)
		b.day += cost
		b.week += cost
		b.month += cost
	}
}

// freshBucket returns the scope's counters, rebuilding from SQLite when
// first seen or when a window boundary has passed.
func (g *Gate) freshBucket(profile string) *bucket {
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekStart := dayStart.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7)) // Monday
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	b, ok := g.buckets[profile]
	if ok && b.dayStart.Equal(dayStart) && b.weekStart.Equal(weekStart) && b.monthStart.Equal(monthStart) {
		return b
	}
	b = &bucket{dayStart: dayStart, weekStart: weekStart, monthStart: monthStart}
	if g.st != nil {
		b.day, _ = g.st.TotalSince(dayStart, profile, "")
		b.week, _ = g.st.TotalSince(weekStart, profile, "")
		b.month, _ = g.st.TotalSince(monthStart, profile, "")
	}
	g.buckets[profile] = b
	return b
}

func windowStart(name string, b *bucket) time.Time {
	switch name {
	case "weekly":
		return b.weekStart
	case "monthly":
		return b.monthStart
	default:
		return b.dayStart
	}
}

func hardStop(b *config.Budget) bool {
	return b.HardStop == nil || *b.HardStop
}
