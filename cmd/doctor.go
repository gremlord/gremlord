package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/pricing"
	"github.com/gremlord/gremlord/internal/router"
	"github.com/gremlord/gremlord/internal/store"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose the gremlord installation",
	RunE: func(cmd *cobra.Command, args []string) error {
		fail := 0
		check := func(ok bool, good, bad string) {
			if ok {
				fmt.Println("✓", good)
			} else {
				fail++
				fmt.Println("✗", bad)
			}
		}

		if !ensureClaude() {
			fail++
		}

		cfg, cfgErr := config.Load()
		if errors.Is(cfgErr, os.ErrNotExist) {
			check(false, "", "no config — run `gremlord setup`")
			return fmt.Errorf("%d problem(s)", fail)
		}
		check(cfgErr == nil, "config parses", fmt.Sprintf("config invalid: %v", cfgErr))
		if cfgErr != nil {
			return fmt.Errorf("%d problem(s)", fail)
		}
		dataDir, _ := config.DataDir()

		for name, p := range cfg.Providers {
			if p.APIKeyEnv == "" || p.APIKey != "" {
				continue
			}
			check(p.Key() != "",
				fmt.Sprintf("provider %s: %s available", name, p.APIKeyEnv),
				fmt.Sprintf("provider %s: %s not in the environment or ~/.gremlord/env", name, p.APIKeyEnv))
		}

		st, err := store.Open(filepath.Join(dataDir, config.DBName))
		check(err == nil, "spend database writable", fmt.Sprintf("spend database: %v", err))
		if err == nil {
			st.Close()
		}

		prices := pricing.Load(dataDir, cfg)
		for alias, m := range cfg.Models {
			check(prices.Has(m.ID),
				fmt.Sprintf("model %s (%s) priced", alias, m.ID),
				fmt.Sprintf("model %s (%s) has no pricing — spend will be untracked; add `pricing:` in config or run `gremlord models update-prices`", alias, m.ID))
		}

		if d, err := router.ReadDiscovery(dataDir); err == nil {
			fmt.Printf("· router leader: pid %d on port %d (version %s)\n", d.PID, d.Port, d.Version)
		} else {
			fmt.Println("· no router running (starts automatically with the next session)")
		}

		ensureClauder()

		// Divergence between the two databases is only a fault while a
		// pre-rename router is actually alive and still writing. Once it is
		// gone, whether to merge those events or abandon them is the user's
		// call, so this drops to a note rather than nagging as a failure on
		// every run.
		if legacyDB, ok := config.LegacyDBPath(); ok {
			if lag, rows, diverged := legacySpendLag(legacyDB, filepath.Join(dataDir, config.DBName)); diverged {
				legacyDir := filepath.Dir(legacyDB)
				if alive, pid := legacyRouterAlive(legacyDir); alive {
					fail++
					fmt.Printf("✗ an agentic router (pid %d) is still logging spend to %s\n", pid, legacyDB)
					fmt.Printf("  %d event(s) are there and not in %s, the newest %s ahead.\n",
						rows, config.DBName, lag.Round(time.Minute))
					fmt.Println("  It holds the router port, so new sessions follow it and their spend")
					fmt.Println("  goes there too. Exit those sessions, then merge with:")
					fmt.Printf("    cp %s* %s/\n", legacyDB, dataDir)
				} else {
					fmt.Printf("· %d event(s) exist only in the pre-rename %s (newest %s ahead)\n",
						rows, legacyDB, lag.Round(time.Minute))
					fmt.Printf("  No agentic router is running, so nothing is still being written there.\n")
					fmt.Printf("  Merge them with `cp %s* %s/`, or drop them with `rm -rf %s`.\n",
						legacyDB, dataDir, legacyDir)
				}
			}
		}

		if dir, notCarried := config.LegacyLeftBehind(); dir != "" {
			fmt.Printf("· %s is still on disk; gremlord reads %s now and left the original alone\n",
				dir, dataDir)
			if len(notCarried) > 0 {
				fmt.Printf("  not carried over: %s\n", strings.Join(notCarried, ", "))
				fmt.Println("  evals/ and swebench-venv/ embed absolute paths, so recreate the venv" +
					" if you run SWE-bench; the logs regenerate. Delete the old directory when you are done with it.")
			}
		}

		home, _ := os.UserHomeDir()
		if data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
			var s struct {
				StatusLine struct {
					Command string `json:"command"`
				} `json:"statusLine"`
			}
			json.Unmarshal(data, &s)
			check(s.StatusLine.Command == statuslineCommand,
				"statusline registered",
				"statusline not registered — run `gremlord setup`")
		}

		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			hasPassthrough := false
			for _, p := range cfg.Profiles {
				if p.Passthrough {
					hasPassthrough = true
				}
			}
			if hasPassthrough {
				fmt.Println("· note: ANTHROPIC_API_KEY is set globally — passthrough profiles will bill the key, not your subscription")
			}
		}

		if fail > 0 {
			return fmt.Errorf("%d problem(s) found", fail)
		}
		fmt.Println("\nAll checks passed.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

// legacySpendLag compares the newest usage event in the pre-rename database
// against the current one, and counts how many events only the old one holds.
// Returns diverged=false on any read error: a missing or unreadable legacy
// database is the normal, finished state, not something to report.
func legacySpendLag(legacyPath, currentPath string) (lag time.Duration, rows int, diverged bool) {
	oldSt, err := store.OpenReadOnly(legacyPath)
	if err != nil {
		return 0, 0, false
	}
	defer oldSt.Close()
	newSt, err := store.OpenReadOnly(currentPath)
	if err != nil {
		return 0, 0, false
	}
	defer newSt.Close()

	oldAt, oldOK, err := oldSt.LatestEventAt()
	if err != nil || !oldOK {
		return 0, 0, false
	}
	newAt, _, err := newSt.LatestEventAt()
	if err != nil {
		return 0, 0, false
	}
	if !oldAt.After(newAt.Add(time.Minute)) {
		return 0, 0, false
	}
	rows, err = oldSt.EventsNewerThan(newAt)
	if err != nil {
		rows = 0
	}
	return oldAt.Sub(newAt), rows, true
}

// legacyRouterAlive reports whether the router recorded in a pre-rename data
// directory is still running. That is what separates an active divergence,
// which the user has to act on, from leftover history they may simply drop.
func legacyRouterAlive(legacyDir string) (bool, int) {
	d, err := router.ReadDiscovery(legacyDir)
	if err != nil {
		return false, 0
	}
	return pidAlive(d.PID), d.PID
}
