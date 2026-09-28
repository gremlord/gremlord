package router

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// touch writes content to path with a strictly-increasing mtime, so two
// rapid touches can't collapse to the same second on filesystems with coarse
// (1s) mtime resolution (CI tmpfs, HFS+). Each call advances mtime by one
// full second past a per-test base.
var touchSeq atomic.Int64

func touch(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Distinct, far-apart mtimes — a base far in the future plus a
	// monotonically increasing second so no two touches can collide.
	mtime := time.Unix(2_000_000_000+touchSeq.Add(1), 0)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// TestPollReloadFiresOnChange verifies a write to the watched file triggers
// exactly one reload, and a second write triggers a second.
func TestPollReloadFiresOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	touch(t, path, "v1")

	var calls atomic.Int32
	reload := func() error { calls.Add(1); return nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pollReload(ctx, quietLogger(), path, 10*time.Millisecond, reload)

	// Let the poller capture the baseline mtime.
	time.Sleep(60 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("spurious reload before any change: %d", got)
	}

	touch(t, path, "v2")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })

	touch(t, path, "v3")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 2 })
}

// TestPollReloadSurvivesBadEdit verifies a reload error is logged and skipped
// without stopping the watcher — the next good edit reloads again.
func TestPollReloadSurvivesBadEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	touch(t, path, "v1")

	var calls atomic.Int32
	var fail atomic.Bool
	reload := func() error {
		calls.Add(1)
		if fail.Load() {
			return errReloadSimulated
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pollReload(ctx, quietLogger(), path, 10*time.Millisecond, reload)

	// Let the poller capture the baseline mtime before we start changing it.
	time.Sleep(60 * time.Millisecond)

	// First change: reload fails. Watcher must survive.
	fail.Store(true)
	touch(t, path, "broken")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 1 })

	// Second change: reload succeeds. Watcher picked it back up. The poller
	// detects change by mtime equality, and CI filesystems round timestamps
	// to milliseconds, so back-to-back writes can share an mtime and the
	// second change is invisible. Wait out that granularity first.
	time.Sleep(20 * time.Millisecond)
	fail.Store(false)
	touch(t, path, "fixed")
	waitFor(t, 3*time.Second, func() bool { return calls.Load() == 2 })
}

// TestPollReloadStopsOnContext verifies canceling ctx unblocks pollReload.
func TestPollReloadStopsOnContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	touch(t, path, "v1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pollReload(ctx, quietLogger(), path, 10*time.Millisecond, func() error { return nil })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pollReload did not return after ctx cancel")
	}
}

var errReloadSimulated = errSentinel("simulated reload failure")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", max)
}
