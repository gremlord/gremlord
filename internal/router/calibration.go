package router

import (
	"log/slog"
	"sync"
	"time"

	"github.com/gremlord/gremlord/internal/store"
	"github.com/gremlord/gremlord/internal/tokens"
)

const (
	// calibrationTTL is how long a measured correction is reused before
	// it is re-derived. Long, because the ratio it measures is a property
	// of a tokenizer, not of the current conversation.
	calibrationTTL = 10 * time.Minute
	// calibrationWindow bounds the history a correction is measured over.
	// Recent enough to follow a model swap behind an alias, long enough
	// that a quiet day does not drop a model below MinSamples.
	calibrationWindow = 14 * 24 * time.Hour
)

// calibrator caches the per-model estimator correction derived from the
// local usage log. Nil-safe: a router without a store simply runs
// uncalibrated, which is the pre-existing behavior.
type calibrator struct {
	store *store.Store
	log   *slog.Logger

	mu  sync.Mutex
	val tokens.Calibration
	at  time.Time
}

func newCalibrator(st *store.Store, log *slog.Logger) *calibrator {
	return &calibrator{store: st, log: log}
}

// get returns the current correction, re-deriving it when stale. A failed
// or empty measurement leaves the estimate uncorrected rather than
// guessing — the raw estimate is deliberately safe, just wasteful.
func (c *calibrator) get() tokens.Calibration {
	if c == nil || c.store == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < calibrationTTL {
		return c.val
	}
	// Stamp before querying so a failure backs off for a full TTL instead
	// of re-querying on every request.
	c.at = time.Now()
	rows, err := c.store.EstimateCalibration(time.Now().Add(-calibrationWindow))
	if err != nil {
		if c.log != nil {
			c.log.Warn("estimate calibration failed", "err", err)
		}
		return c.val
	}
	next := tokens.Calibration{}
	for _, row := range rows {
		// Too few samples is not a measurement — leave the model on the
		// raw estimate, which is safe, just wasteful.
		if row.Requests < tokens.MinSamples || row.Ratio() <= 0 {
			continue
		}
		next[row.Model] = tokens.Clamp(row.Ratio())
	}
	if len(next) == 0 {
		c.val = nil
		return nil
	}
	if c.log != nil {
		c.log.Debug("estimate_calibration", "models", len(next))
	}
	c.val = next
	return next
}

// set overrides the cached correction. Tests only.
func (c *calibrator) set(v tokens.Calibration) {
	c.mu.Lock()
	c.val, c.at = v, time.Now()
	c.mu.Unlock()
}
