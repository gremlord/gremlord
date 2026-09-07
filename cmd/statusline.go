package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
)

// statuslineCmd is invoked by Claude Code (registered as its statusLine
// command) with session JSON on stdin. It inherits the session's env, so
// GREMLORD_SESSION_ID / GREMLORD_PROFILE identify the session (the
// pre-rename AGENTIC_* spellings are still accepted).
var statuslineCmd = &cobra.Command{
	Use:    "statusline",
	Short:  "Claude Code statusLine hook (spend bar)",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		var input struct {
			Model struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		}
		json.NewDecoder(os.Stdin).Decode(&input)

		profile := config.Getenv("PROFILE")
		sessionID := config.Getenv("SESSION_ID")
		if sessionID == "" {
			fmt.Printf("%s · sub · no tracking\n", orDefault(input.Model.DisplayName, "claude"))
			return nil
		}

		cfg, dataDir, err := loadConfig()
		if err != nil {
			return nil // never break the statusline
		}
		st, err := store.OpenReadOnly(filepath.Join(dataDir, config.DBName))
		if err != nil {
			return nil
		}
		defer st.Close()

		now := time.Now()
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		sess, _ := st.TotalSince(time.Time{}, "", sessionID)
		day, _ := st.TotalSince(dayStart, "", "")

		modelPart := orDefault(input.Model.DisplayName, "?")
		if alias, tier, model, _, ok, _ := st.LatestRouteDecision(sessionID); ok && alias == input.Model.DisplayName {
			tierColor := map[string]string{
				"deep":     "\033[35m", // magenta
				"standard": "\033[36m", // cyan
				"light":    "\033[32m", // green
			}[tier]
			if tierColor != "" {
				modelPart = fmt.Sprintf("%s→%s%s\033[0m (%s)", alias, tierColor, model, tier)
			}
		}

		var goalReason string
		goal, reason, ok, _ := st.LatestGoalDecision(sessionID)
		if ok && goal {
			goalReason = reason
		}

		fmt.Println(statusLine(orDefault(profile, "gremlord"), modelPart, sess, day, cfg.Budgets, goalReason))
		return nil
	},
}

// statusLine builds the printed status line. Segment order matters: the
// budget suffix ("/$600 [bar]") describes the day-spend figure and must sit
// immediately after it — appending it after the goal segment instead makes
// it read as though it belongs to the goal reason. goalReason == "" omits
// the goal segment entirely.
func statusLine(profile, modelPart string, sess, day float64, budgets *config.Budget, goalReason string) string {
	line := fmt.Sprintf("%s · %s · sess $%.2f · day $%.2f", profile, modelPart, sess, day)

	if budgets != nil && budgets.Daily > 0 {
		frac := day / budgets.Daily
		color := "\033[32m" // green
		warnAt := budgets.WarnAt
		if warnAt == 0 {
			warnAt = 0.8
		}
		switch {
		case frac >= 1:
			color = "\033[31m" // red
		case frac >= warnAt:
			color = "\033[33m" // yellow
		}
		line += fmt.Sprintf("/$%.0f %s[%s]\033[0m", budgets.Daily, color, bar(frac, 6))
	}

	if goalReason != "" {
		line += fmt.Sprintf(" · \033[33m⟳ goal\033[0m (%s)", truncateForStatus(goalReason))
	}

	return line
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// truncateForStatus caps a classifier reason so a verbose answer can't blow
// out the status line.
func truncateForStatus(s string) string {
	const max = 24
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func init() {
	rootCmd.AddCommand(statuslineCmd)
}
