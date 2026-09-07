package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/router"
	"github.com/gremlord/gremlord/internal/store"
)

// printRestartReport lists what is still running an old binary after an
// update: the router leader (which serves every session until it exits)
// and open sessions (each hosts the old launcher, and one of them hosts
// the leader). Best-effort — a missing store or discovery file just means
// nothing to report.
func printRestartReport(newTag string) {
	dataDir, err := config.DataDir()
	if err != nil {
		return
	}

	var lines []string
	if d, err := router.ReadDiscovery(dataDir); err == nil {
		line := fmt.Sprintf("  router leader   pid %d on :%d (%s)", d.PID, d.Port, d.Version)
		switch {
		case !pidAlive(d.PID):
			line += "  — not running (stale discovery file, nothing to do)"
		case d.Version == newTag:
			line += "  — already on the new version"
		default:
			line += "  — exits with its host session, or restart `gremlord router run`"
		}
		lines = append(lines, line)
	}

	if st, err := store.OpenReadOnly(filepath.Join(dataDir, config.DBName)); err == nil {
		defer st.Close()
		sessions, _ := st.ActiveSessions()
		const maxListed = 20
		for i, s := range sessions {
			if i == maxListed {
				lines = append(lines, fmt.Sprintf("  … and %d more (see `gremlord cost --by session`)", len(sessions)-maxListed))
				break
			}
			line := fmt.Sprintf("  session %-28s profile %-10s %s  started %s",
				s.ID, s.Profile, s.WorkDir, humanAge(s.StartedAt))
			switch {
			case s.LastSeen.IsZero():
				line += ", no activity recorded"
			case time.Since(s.LastSeen) > 24*time.Hour:
				line += fmt.Sprintf(", idle %s (may have exited uncleanly)", humanAge(s.LastSeen))
			default:
				line += fmt.Sprintf(", active %s", humanAge(s.LastSeen))
			}
			lines = append(lines, line)
		}
	}

	if len(lines) == 0 {
		fmt.Println("Nothing is running the old version — new sessions pick up the update automatically.")
		return
	}
	fmt.Printf("Still on the old binary until restarted (exit and re-run `gremlord`):\n")
	for _, l := range lines {
		fmt.Println(l)
	}
}

// pidAlive reports whether a process exists. On Windows signal 0 isn't
// supported, so err on the side of "alive".
func pidAlive(pid int) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
