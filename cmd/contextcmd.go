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
	"github.com/gremlord/gremlord/internal/tokens"
)

var contextJSON bool

var contextCmd = &cobra.Command{
	Use:   "context [session-id]",
	Short: "Context-fullness trajectory of a session (research view for context scaling)",
	Long: `context shows, request by request, how full the routed model's real
context window was versus what Claude Code's gauge saw. Use it to verify
scaling behavior and to tune context_window / effective_context: sessions
that error or degrade at high fullness argue for a lower effective_context.
Defaults to the most recent session; find ids with 'gremlord cost --by session'.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		st, err := store.OpenReadOnly(filepath.Join(dataDir, config.DBName))
		if err != nil {
			return fmt.Errorf("no usage recorded yet (%v)", err)
		}
		defer st.Close()

		sessionID := ""
		if len(args) == 1 {
			sessionID = args[0]
		} else {
			if sessionID, err = st.LatestSessionID(); err != nil {
				return err
			}
			if sessionID == "" {
				return fmt.Errorf("no attributed sessions recorded yet")
			}
		}
		traj, err := st.ContextTrajectory(sessionID)
		if err != nil {
			return err
		}
		if len(traj) == 0 {
			return fmt.Errorf("no usage recorded for session %q", sessionID)
		}
		if contextJSON {
			return json.NewEncoder(os.Stdout).Encode(traj)
		}

		fmt.Printf("Session %s — %d requests\n", sessionID, len(traj))
		fmt.Printf("%-6s %-24s %9s %9s %9s  %s\n", "time", "model", "true", "reported", "budget", "fullness")
		var prevTrue int64
		for _, e := range traj {
			marker := ""
			// A drop between successful requests is a compaction. The system
			// prompt is a large constant floor, so even a full compact only
			// drops the total by a modest fraction — flag any >10% dip.
			if e.TrueInput > 0 && prevTrue > 0 && float64(e.TrueInput) < 0.9*float64(prevTrue) {
				marker = "  ← compacted"
			}
			if e.ErrType != "" {
				marker += fmt.Sprintf("  ✗ %s", e.ErrType)
			}
			fullness := "unscaled"
			if e.CtxBudget > 0 {
				frac := float64(e.TrueInput) / float64(e.CtxBudget)
				fullness = fmt.Sprintf("[%s] %3.0f%%", bar(frac, 12), frac*100)
			}
			fmt.Printf("%-6s %-24s %9s %9s %9s  %s%s\n",
				e.TS.Format("15:04"), e.Model,
				humanTokens(e.TrueInput), humanTokens(e.ReportedInput), humanTokens(int64(e.CtxBudget)),
				fullness, marker)
			if e.TrueInput > 0 {
				prevTrue = e.TrueInput // zero-usage error rows aren't a baseline
			}
		}
		fmt.Printf("\nassumed window %s: the client compacts against that; 'true' is what the model really held.\n",
			humanTokens(tokens.AssumedWindow))
		printComposition(st, sessionID)
		printCalibration(st)
		return nil
	},
}

// printComposition shows where a session's context actually goes. The
// system prompt and tool schemas are re-sent in full on every request, so
// a high fixed share means most of the window is being spent before the
// turn's own work is considered.
func printComposition(st *store.Store, sessionID string) {
	rows, err := st.Composition(sessionID, time.Time{})
	if err != nil || len(rows) == 0 {
		return
	}
	fmt.Printf("\nAverage request composition (estimated)\n")
	fmt.Printf("%-24s %9s %9s %9s  %s\n", "model", "system", "tools", "messages", "fixed")
	for _, r := range rows {
		fmt.Printf("%-24s %9s %9s %9s  %3.0f%%\n", r.Model,
			humanTokens(r.System), humanTokens(r.Tools), humanTokens(r.Messages),
			r.FixedFraction()*100)
	}
}

// printCalibration reports how close the router's own estimator runs to
// real billed usage. Under 1.00 it over-counts, and every over-counted
// token is context budget the router refuses to use.
func printCalibration(st *store.Store) {
	rows, err := st.EstimateCalibration(time.Now().AddDate(0, 0, -14))
	if err != nil || len(rows) == 0 {
		return
	}
	fmt.Printf("\nEstimator accuracy, last 14 days (true / estimated)\n")
	for _, r := range rows {
		note := ""
		if r.Requests < tokens.MinSamples {
			note = fmt.Sprintf("  (uncorrected: %d/%d samples)", r.Requests, tokens.MinSamples)
		}
		fmt.Printf("  %-24s ×%.2f  over %d requests%s\n", r.Model, r.Ratio(), r.Requests, note)
	}
}

func init() {
	contextCmd.Flags().BoolVar(&contextJSON, "json", false, "machine-readable output")
}
