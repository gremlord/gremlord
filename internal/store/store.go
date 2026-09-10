// Package store persists sessions and usage events in SQLite.
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the gremlord database. modernc.org/sqlite
// uses _pragma=name(value) DSN syntax; WAL lets CLI readers run while the
// router leader holds the single write connection.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens the database for CLI readers (cost, statusline).
func OpenReadOnly(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  profile    TEXT,
  work_dir   TEXT,
  started_at INTEGER,
  ended_at   INTEGER
);
CREATE TABLE IF NOT EXISTS usage_events (
  id                INTEGER PRIMARY KEY,
  ts                INTEGER NOT NULL,
  session_id        TEXT,
  profile           TEXT,
  provider          TEXT,
  model             TEXT,
  model_alias       TEXT,
  input_tokens      INTEGER,
  output_tokens     INTEGER,
  cache_read_tokens INTEGER,
  cache_write_tokens INTEGER,
  cost_usd          REAL,
  priced            INTEGER,
  request_id        TEXT,
  status            INTEGER,
  err_type          TEXT
);
CREATE TABLE IF NOT EXISTS route_decisions (
  session_id TEXT PRIMARY KEY,
  alias      TEXT,
  tier       TEXT,
  model      TEXT,
  at         INTEGER
);
CREATE TABLE IF NOT EXISTS route_events (
  id         INTEGER PRIMARY KEY,
  ts         INTEGER NOT NULL,
  session_id TEXT,
  alias      TEXT,
  tier       TEXT,
  model      TEXT,
  reason     TEXT
);
CREATE TABLE IF NOT EXISTS goal_decisions (
  session_id TEXT PRIMARY KEY,
  goal       INTEGER,
  reason     TEXT,
  at         INTEGER
);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_events(ts);
CREATE INDEX IF NOT EXISTS idx_usage_session ON usage_events(session_id);
CREATE INDEX IF NOT EXISTS idx_route_events_session ON route_events(session_id, ts, id);
`)
	if err != nil {
		return err
	}
	// Additive columns for existing databases. ctx_budget is the routed
	// model's context budget at request time; reported_input is the
	// (possibly scaled) input-side total sent to the client — together they
	// make context-scaling behavior queryable per session.
	for _, col := range []string{
		"ALTER TABLE usage_events ADD COLUMN ctx_budget INTEGER DEFAULT 0",
		"ALTER TABLE usage_events ADD COLUMN reported_input INTEGER DEFAULT 0",
		"ALTER TABLE usage_events ADD COLUMN duration_ms INTEGER DEFAULT 0",
		"ALTER TABLE route_decisions ADD COLUMN reason TEXT DEFAULT ''",
		// est_* are the router's own bias-high estimate of the request,
		// split by section, recorded UNCALIBRATED. Comparing est_input to
		// the billed input is what calibration is derived from, so storing
		// a corrected number here would feed the correction back into
		// itself; est_system/est_tools make the fixed per-request tax
		// (system prompt + tool schemas) queryable against the part that
		// actually accumulates.
		"ALTER TABLE usage_events ADD COLUMN est_input INTEGER DEFAULT 0",
		"ALTER TABLE usage_events ADD COLUMN est_system INTEGER DEFAULT 0",
		"ALTER TABLE usage_events ADD COLUMN est_tools INTEGER DEFAULT 0",
		// The budget the client-facing gauge was scaled against, which
		// under a session-stable anchor is no longer the same thing as the
		// serving model's own ctx_budget. Both are needed to read a
		// trajectory: ctx_budget says how full the model was, gauge_budget
		// says what the client was told.
		"ALTER TABLE usage_events ADD COLUMN gauge_budget INTEGER DEFAULT 0",
	} {
		if _, err := s.db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

type UsageEvent struct {
	TS               time.Time
	SessionID        string
	Profile          string
	Provider         string
	Model            string
	Alias            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
	Priced           bool
	RequestID        string
	Status           int
	ErrType          string
	CtxBudget        int   // model's context budget (0 = unknown/unscaled)
	ReportedInput    int64 // input-side tokens as reported to the client
	DurationMS       int64 // end-to-end router request duration
	EstInput         int64 // raw (uncalibrated) input estimate for this request
	EstSystem        int64 // portion of EstInput attributed to the system prompt
	EstTools         int64 // portion of EstInput attributed to tool schemas
	GaugeBudget      int   // budget the client gauge was scaled against (0 = the model's own)
}

func (s *Store) RecordUsage(e UsageEvent) error {
	_, err := s.db.Exec(`INSERT INTO usage_events
(ts, session_id, profile, provider, model, model_alias,
 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
 cost_usd, priced, request_id, status, err_type, ctx_budget, reported_input, duration_ms,
 est_input, est_system, est_tools, gauge_budget)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.TS.Unix(), e.SessionID, e.Profile, e.Provider, e.Model, e.Alias,
		e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens,
		e.CostUSD, boolToInt(e.Priced), e.RequestID, e.Status, e.ErrType,
		e.CtxBudget, e.ReportedInput, e.DurationMS,
		e.EstInput, e.EstSystem, e.EstTools, e.GaugeBudget)
	return err
}

func (s *Store) StartSession(id, profile, workDir string, at time.Time) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO sessions (id, profile, work_dir, started_at) VALUES (?,?,?,?)`,
		id, profile, workDir, at.Unix())
	return err
}

func (s *Store) EndSession(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE sessions SET ended_at = ? WHERE id = ?`, at.Unix(), id)
	return err
}

// ActiveSession is a launched session that has not recorded an end. A
// session that died without cleanup (kill -9, crash) lingers here — LastSeen
// (the newest usage event) lets callers flag those instead of trusting the
// row blindly.
type ActiveSession struct {
	ID        string
	Profile   string
	WorkDir   string
	StartedAt time.Time
	LastSeen  time.Time // zero when no usage was ever attributed
}

// ActiveSessions returns open sessions, most recently started first.
func (s *Store) ActiveSessions() ([]ActiveSession, error) {
	rows, err := s.db.Query(`SELECT id, COALESCE(profile,''), COALESCE(work_dir,''), started_at,
  COALESCE((SELECT MAX(ts) FROM usage_events u WHERE u.session_id = sessions.id), 0)
FROM sessions WHERE ended_at IS NULL ORDER BY started_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveSession
	for rows.Next() {
		var a ActiveSession
		var started, seen int64
		if err := rows.Scan(&a.ID, &a.Profile, &a.WorkDir, &started, &seen); err != nil {
			return nil, err
		}
		a.StartedAt = time.Unix(started, 0)
		if seen > 0 {
			a.LastSeen = time.Unix(seen, 0)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordRouteDecision persists the auto-router's tier/model choice for a
// session's current turn, overwriting any prior decision for that session
// (a session re-classifies as new user turns arrive). reason is a free-text
// note — e.g. "size:light→standard" when size-aware routing remapped a tier
// — empty for a plain classifier decision.
func (s *Store) RecordRouteDecision(sessionID, alias, tier, model, reason string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO route_decisions
(session_id, alias, tier, model, reason, at) VALUES (?,?,?,?,?,?)`,
		sessionID, alias, tier, model, reason, at.Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO route_events
(ts, session_id, alias, tier, model, reason) VALUES (?,?,?,?,?,?)`,
		at.Unix(), sessionID, alias, tier, model, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// LatestRouteDecision returns the most recent auto-router decision recorded
// for a session, if any. ok is false when no decision has been recorded yet
// (e.g. the profile isn't using a routing alias).
func (s *Store) LatestRouteDecision(sessionID string) (alias, tier, model, reason string, ok bool, err error) {
	row := s.db.QueryRow(`SELECT alias, tier, model, reason FROM route_decisions WHERE session_id = ?`, sessionID)
	err = row.Scan(&alias, &tier, &model, &reason)
	if err != nil && strings.Contains(err.Error(), "no such column: reason") {
		// Read-only clients may briefly attach to an older router leader that
		// has not restarted to run the additive reason-column migration yet.
		// Preserve the decision itself and leave reason empty.
		reason = ""
		err = s.db.QueryRow(`SELECT alias, tier, model FROM route_decisions WHERE session_id = ?`, sessionID).Scan(&alias, &tier, &model)
	}
	if err == sql.ErrNoRows {
		return "", "", "", "", false, nil
	}
	return alias, tier, model, reason, err == nil, err
}

// RouteEvent is one append-only routing decision. Unlike route_decisions,
// these rows preserve every turn for evaluation and post-hoc analysis.
type RouteEvent struct {
	TS        time.Time
	SessionID string
	Alias     string
	Tier      string
	Model     string
	Reason    string
}

func (s *Store) RouteEvents(sessionID string) ([]RouteEvent, error) {
	rows, err := s.db.Query(`SELECT ts, session_id, alias, tier, model, reason
FROM route_events WHERE session_id = ? ORDER BY ts, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RouteEvent
	for rows.Next() {
		var e RouteEvent
		var ts int64
		if err := rows.Scan(&ts, &e.SessionID, &e.Alias, &e.Tier, &e.Model, &e.Reason); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordGoalDecision persists the auto-goal classifier's verdict for a
// session's current turn, overwriting any prior decision for that session
// (a session re-classifies as new user turns arrive).
func (s *Store) RecordGoalDecision(sessionID string, goal bool, reason string, at time.Time) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO goal_decisions
(session_id, goal, reason, at) VALUES (?,?,?,?)`,
		sessionID, boolToInt(goal), reason, at.Unix())
	return err
}

// LatestGoalDecision returns the most recent auto-goal verdict recorded for
// a session, if any. ok is false when no decision has been recorded yet.
func (s *Store) LatestGoalDecision(sessionID string) (goal bool, reason string, ok bool, err error) {
	row := s.db.QueryRow(`SELECT goal, reason FROM goal_decisions WHERE session_id = ?`, sessionID)
	var g int
	err = row.Scan(&g, &reason)
	if err == sql.ErrNoRows {
		return false, "", false, nil
	}
	return g != 0, reason, err == nil, err
}

// ContextEvent is one request's context-fullness datapoint: how full the
// routed model really was vs what the client was told. The research surface
// for tuning context_window/effective_context.
type ContextEvent struct {
	TS            time.Time
	Model         string
	TrueInput     int64 // input + cache read + cache write, as billed
	ReportedInput int64 // what the client's context gauge saw
	CtxBudget     int   // model budget at request time (0 = unscaled)
	Status        int
	ErrType       string
}

// ContextTrajectory returns a session's per-request context datapoints in
// time order. A drop in TrueInput between consecutive rows is a compaction.
func (s *Store) ContextTrajectory(sessionID string) ([]ContextEvent, error) {
	rows, err := s.db.Query(`SELECT ts, model,
  COALESCE(input_tokens,0)+COALESCE(cache_read_tokens,0)+COALESCE(cache_write_tokens,0),
  COALESCE(reported_input,0), COALESCE(ctx_budget,0), status, COALESCE(err_type,'')
FROM usage_events WHERE session_id = ? ORDER BY ts, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContextEvent
	for rows.Next() {
		var e ContextEvent
		var ts int64
		if err := rows.Scan(&ts, &e.Model, &e.TrueInput, &e.ReportedInput, &e.CtxBudget, &e.Status, &e.ErrType); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SessionUsage returns raw request records for an attributed session. These
// rows let evaluation artifacts report exact token, cost, and latency totals.
func (s *Store) SessionUsage(sessionID string) ([]UsageEvent, error) {
	rows, err := s.db.Query(`SELECT ts, session_id, profile, provider, model, model_alias,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd,
  priced, request_id, status, err_type, ctx_budget, reported_input, duration_ms,
  COALESCE(est_input,0), COALESCE(est_system,0), COALESCE(est_tools,0), COALESCE(gauge_budget,0)
FROM usage_events WHERE session_id = ? ORDER BY ts, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageEvent
	for rows.Next() {
		var e UsageEvent
		var ts int64
		var priced int
		if err := rows.Scan(&ts, &e.SessionID, &e.Profile, &e.Provider, &e.Model, &e.Alias,
			&e.InputTokens, &e.OutputTokens, &e.CacheReadTokens, &e.CacheWriteTokens, &e.CostUSD,
			&priced, &e.RequestID, &e.Status, &e.ErrType, &e.CtxBudget, &e.ReportedInput, &e.DurationMS,
			&e.EstInput, &e.EstSystem, &e.EstTools, &e.GaugeBudget); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0)
		e.Priced = priced != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestSessionID returns the session with the most recent usage event, or
// "" when nothing attributed has been recorded.
func (s *Store) LatestSessionID() (string, error) {
	row := s.db.QueryRow(`SELECT session_id FROM usage_events
WHERE session_id != '' ORDER BY ts DESC, id DESC LIMIT 1`)
	var id string
	err := row.Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// SpendRow is one line of a cost report. InputTokens is the whole input
// side (fresh + cache read + cache write); CacheReadTokens is the part of
// it that was served from an upstream prefix cache, which is the number
// that says whether prompt caching is actually working.
type SpendRow struct {
	Key             string // model, profile, or session id depending on grouping
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	CostUSD         float64
	Unpriced        int64 // count of events with priced=0
}

// CacheHitRate is the fraction of input tokens served from cache.
func (r SpendRow) CacheHitRate() float64 {
	if r.InputTokens <= 0 {
		return 0
	}
	return float64(r.CacheReadTokens) / float64(r.InputTokens)
}

// SpendSince aggregates usage from `since`, grouped by "model", "profile",
// or "session".
func (s *Store) SpendSince(since time.Time, groupBy string) ([]SpendRow, error) {
	return s.SpendSinceFiltered(since, groupBy, "")
}

// SpendSinceFiltered is SpendSince restricted to one session_id. An empty
// sessionID is the unfiltered report.
func (s *Store) SpendSinceFiltered(since time.Time, groupBy, sessionID string) ([]SpendRow, error) {
	col := map[string]string{"model": "model", "profile": "profile", "session": "session_id"}[groupBy]
	if col == "" {
		return nil, fmt.Errorf("unknown grouping %q", groupBy)
	}
	q := `SELECT ` + col + `,
  COALESCE(SUM(input_tokens+cache_read_tokens+cache_write_tokens),0),
  COALESCE(SUM(output_tokens),0),
  COALESCE(SUM(cache_read_tokens),0),
  COALESCE(SUM(cost_usd),0),
  COALESCE(SUM(1-priced),0)
FROM usage_events WHERE ts >= ?`
	args := []any{since.Unix()}
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	q += ` GROUP BY ` + col + ` ORDER BY SUM(cost_usd) DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpendRow
	for rows.Next() {
		var r SpendRow
		if err := rows.Scan(&r.Key, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CostUSD, &r.Unpriced); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionBounds is the first and last usage timestamp for a session, plus
// the profile that session ran under (empty if unattributed).
type SessionBounds struct {
	First    time.Time
	Last     time.Time
	Profile  string
	Requests int64
}

// SessionBounds returns the time span of a session's usage. ok is false
// when no events exist for that id.
func (s *Store) SessionBounds(sessionID string) (SessionBounds, bool, error) {
	var first, last, n int64
	var profile string
	err := s.db.QueryRow(`SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0),
  COALESCE(MAX(profile),''), COUNT(*)
FROM usage_events WHERE session_id = ?`, sessionID).Scan(&first, &last, &profile, &n)
	if err != nil {
		return SessionBounds{}, false, err
	}
	if n == 0 {
		return SessionBounds{}, false, nil
	}
	return SessionBounds{
		First:    time.Unix(first, 0),
		Last:     time.Unix(last, 0),
		Profile:  profile,
		Requests: n,
	}, true, nil
}

// TotalSince returns total spend from `since`, optionally filtered by
// profile ("" = all) or session ("" = all).
func (s *Store) TotalSince(since time.Time, profile, session string) (float64, error) {
	q := `SELECT COALESCE(SUM(cost_usd),0) FROM usage_events WHERE ts >= ?`
	args := []any{since.Unix()}
	if profile != "" {
		q += ` AND profile = ?`
		args = append(args, profile)
	}
	if session != "" {
		q += ` AND session_id = ?`
		args = append(args, session)
	}
	var total float64
	err := s.db.QueryRow(q, args...).Scan(&total)
	return total, err
}

// CalibrationRow is one model's measured estimator accuracy: what the
// router guessed the input would be versus what upstream actually billed.
type CalibrationRow struct {
	Model     string
	Requests  int64
	TrueInput int64 // input + cache read + cache write, as billed
	EstInput  int64 // the router's raw, uncalibrated estimate
}

// Ratio is the correction factor: true input / estimated input. Below 1
// the estimator runs high and is stranding usable context window; above 1
// it under-counts and its safety bias has been eaten.
func (r CalibrationRow) Ratio() float64 {
	if r.EstInput <= 0 {
		return 0
	}
	return float64(r.TrueInput) / float64(r.EstInput)
}

// EstimateCalibration measures the router's own token estimator against
// what upstream actually billed, per upstream model, over successful
// requests since `since`.
//
// Ratio of sums, not mean of ratios: a handful of tiny requests should
// not outvote the large ones, and it is the large ones whose fit against
// a context budget actually matters. Callers decide how many samples they
// need before trusting a row.
func (s *Store) EstimateCalibration(since time.Time) ([]CalibrationRow, error) {
	rows, err := s.db.Query(`SELECT model, COUNT(*),
  COALESCE(SUM(input_tokens+cache_read_tokens+cache_write_tokens),0),
  COALESCE(SUM(est_input),0)
FROM usage_events
WHERE ts >= ? AND status = 200 AND COALESCE(est_input,0) > 0
GROUP BY model ORDER BY COUNT(*) DESC`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CalibrationRow
	for rows.Next() {
		var r CalibrationRow
		if err := rows.Scan(&r.Model, &r.Requests, &r.TrueInput, &r.EstInput); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CompositionRow is the average makeup of a model's requests: how much of
// the estimated input is system prompt and tool schemas — paid on every
// request whatever the turn is about — versus conversation.
type CompositionRow struct {
	Model    string
	Requests int64
	System   int64
	Tools    int64
	Messages int64
}

func (r CompositionRow) Total() int64 { return r.System + r.Tools + r.Messages }

// FixedFraction is the share of an average request that is system prompt
// plus tool schemas.
func (r CompositionRow) FixedFraction() float64 {
	if r.Total() <= 0 {
		return 0
	}
	return float64(r.System+r.Tools) / float64(r.Total())
}

// Composition returns average per-request composition by model, for one
// session (sessionID != "") or across everything since `since`.
func (s *Store) Composition(sessionID string, since time.Time) ([]CompositionRow, error) {
	q := `SELECT model, COUNT(*),
  CAST(AVG(est_system) AS INTEGER),
  CAST(AVG(est_tools) AS INTEGER),
  CAST(AVG(est_input - est_system - est_tools) AS INTEGER)
FROM usage_events WHERE COALESCE(est_input,0) > 0`
	args := []any{}
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, sessionID)
	} else {
		q += ` AND ts >= ?`
		args = append(args, since.Unix())
	}
	q += ` GROUP BY model ORDER BY COUNT(*) DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompositionRow
	for rows.Next() {
		var r CompositionRow
		if err := rows.Scan(&r.Model, &r.Requests, &r.System, &r.Tools, &r.Messages); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// LatestEventAt returns the timestamp of the most recent usage event, and
// whether there is one. Used to compare two databases during the agentic
// migration, where file modification times are useless: any reader that opens
// a database read-write touches it, so the file mtimes converge even while the
// contents diverge.
func (s *Store) LatestEventAt() (time.Time, bool, error) {
	var ts sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(ts) FROM usage_events`).Scan(&ts); err != nil {
		return time.Time{}, false, err
	}
	if !ts.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(ts.Int64, 0), true, nil
}

// EventsNewerThan counts usage events recorded after t.
func (s *Store) EventsNewerThan(t time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE ts > ?`, t.Unix()).Scan(&n)
	return n, err
}
