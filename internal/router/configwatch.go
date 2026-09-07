package router

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/gremlord/gremlord/internal/config"
)

// watchConfig polls the config file's mtime and reloads the server when it
// changes. This closes the gap for direct edits to ~/.gremlord/config.yaml:
// `gremlord config set` already hot-reloads via the reload path, but an
// editor save does not, so the running leader would serve a stale config
// until restarted.
//
// It runs only in the leader process. Followers proxy to the leader, so a
// leader reload covers them. Reload is debounced: a burst of writes (save +
// formatter + linter) collapses to one reload. A reload failure (e.g. a
// half-typed file) is logged and retried on the next change — the last known
// good config keeps serving, so a bad edit never takes the router down.
func watchConfig(ctx context.Context, srv *Server, logger *slog.Logger) {
	path, err := config.Path()
	if err != nil {
		return // no usable config path; nothing to watch
	}
	pollReload(ctx, logger, path, 2*time.Second, srv.Reload)
}

// pollReload is the testable core of watchConfig. It polls path every
// interval and calls reload when the file's mtime advances.
func pollReload(ctx context.Context, logger *slog.Logger, path string, interval time.Duration, reload func() error) {
	lastMod := fileModTime(path)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mod := fileModTime(path)
			if mod.Equal(lastMod) {
				continue
			}
			lastMod = mod
			if err := reload(); err != nil {
				// Keep the prior config; retry on the next mtime change.
				logger.Warn("config file changed but failed to reload; keeping last config",
					"err", err)
				continue
			}
			logger.Info("config reloaded from file change")
		}
	}
}

// fileModTime returns the file's mtime, or the zero time if it is missing —
// a missing file is treated as unchanged so a transient delete (atomic save
// via rename) doesn't trigger a doomed reload.
func fileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
