package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	// DirName is where gremlord keeps config, keys, and the cost log.
	DirName = ".gremlord"

	// LegacyDirName is the pre-rename directory. Read once, to carry a v0.1.x
	// gremlord install forward; never written to.
	LegacyDirName = ".agentic"

	// DBName deliberately keeps its pre-rename name. SQLite keeps -wal and
	// -shm sidecars beside the main file, and renaming only the main file
	// orphans the write-ahead log — which is exactly the cost history the
	// migration exists to preserve.
	DBName = "agentic.db"
)

// carried is what moves from an agentic install to a gremlord one.
//
// Deliberately not carried:
//   - router.json — leader discovery for a router that is gone by now; a stale
//     copy would point the new binary at a dead port.
//   - router.log* — a rotating log that regenerates on the next request.
//   - evals/, swebench-venv/ — hundreds of megabytes between them, and both
//     embed absolute paths (a Python venv does not survive being moved), so
//     copying would duplicate a lot of disk to produce something broken.
var carried = []string{
	"config.yaml",
	"env",
	"token",
	"prices.json",
	DBName,
	DBName + "-wal",
	DBName + "-shm",
}

// migrateLegacy copies an agentic data directory into its gremlord equivalent
// the first time the renamed binary runs. The original is left untouched, so a
// user can go back to the old binary with their history intact.
//
// The copy lands in a temp directory and is renamed into place, so two
// processes starting at once cannot interleave a half-copied directory: one
// rename wins and the loser sees a directory that already exists.
func migrateLegacy(home, dir string) (bool, error) {
	if _, err := os.Stat(dir); err == nil {
		return false, nil // already on gremlord
	} else if !os.IsNotExist(err) {
		return false, err
	}

	legacy := filepath.Join(home, LegacyDirName)
	if info, err := os.Stat(legacy); err != nil || !info.IsDir() {
		return false, nil // nothing to carry over
	}

	staging, err := os.MkdirTemp(home, ".gremlord-migrating-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return false, err
	}

	copied := 0
	for _, name := range carried {
		src := filepath.Join(legacy, name)
		if _, err := os.Stat(src); err != nil {
			continue // optional file this install never created
		}
		if err := copyFile(src, filepath.Join(staging, name)); err != nil {
			return false, fmt.Errorf("copy %s: %w", name, err)
		}
		copied++
	}
	if copied == 0 {
		return false, nil // an empty or unrelated ~/.agentic
	}

	if err := os.Rename(staging, dir); err != nil {
		// Lost the race with another process, which is a success for us.
		if _, statErr := os.Stat(dir); statErr == nil {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// copyFile copies one file, preserving its mode so key material stays 0600.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// readPath resolves a file for reading, preferring the gremlord directory and
// falling back to the same filename under ~/.agentic.
//
// migrateLegacy normally copies these on first run, so this only matters when
// the copy could not complete — an unwritable home, a full disk, a permissions
// problem. Without the fallback such a user silently loses their config and
// provider keys and gets defaults instead, which for a tool holding API keys
// and budgets is a much worse failure than reading the old location.
//
// Reads only. Writes always go to the gremlord directory, so the old files are
// never modified and the tool converges on the new location.
func readPath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	legacy := filepath.Join(home, LegacyDirName, filepath.Base(path))
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return path
}

// LegacyLeftBehind describes an agentic directory still on disk and what the
// migration deliberately did not carry across. Empty when there is nothing to
// say. The first-run notice points users at `gremlord doctor` for this, so the
// two have to stay in step.
func LegacyLeftBehind() (dir string, notCarried []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	dir = filepath.Join(home, LegacyDirName)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", nil
	}
	for _, name := range []string{"evals", "swebench-venv", "router.log", "router.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			notCarried = append(notCarried, name)
		}
	}
	return dir, notCarried
}

// LegacyDBIsNewer reports whether the pre-rename spend database has been
// written more recently than the current one, and by how long.
//
// This is the one migration hazard the copy cannot prevent. The router leader
// is whichever binary won the port, and it can be an agentic process that has
// been up for days. New sessions health-check it, find it alive, and follow it
// as leader -- so their spend keeps landing in ~/.agentic while `gremlord cost`
// reads ~/.gremlord and shows nothing new. Nothing is lost, but the two
// diverge until that old leader exits, and the copy already happened so it will
// not run again.
func LegacyDBIsNewer() (legacy string, behind time.Duration, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", 0, false
	}
	legacy = filepath.Join(home, LegacyDirName, DBName)
	old, err := os.Stat(legacy)
	if err != nil {
		return "", 0, false
	}
	cur, err := os.Stat(filepath.Join(home, DirName, DBName))
	if err != nil {
		return "", 0, false
	}
	if d := old.ModTime().Sub(cur.ModTime()); d > time.Minute {
		return legacy, d, true
	}
	return "", 0, false
}
