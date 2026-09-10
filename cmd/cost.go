package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/router"
	"github.com/gremlord/gremlord/internal/store"
)

var (
	costWeek    bool
	costMonth   bool
	costSince   string
	costBy      string
	costJSON    bool
	costSession string
	costReceipt bool
)

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Spend report from the local usage log",
	Long: `Spend report from the local usage log.

Default is today's spend, grouped by model. --receipt prints a
self-contained pasteable log for one session (defaults to the most
recent) — the format the $25 Challenge asks for as a receipt.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		st, err := store.OpenReadOnly(filepath.Join(dataDir, config.DBName))
		if err != nil {
			return fmt.Errorf("no usage recorded yet (%v)", err)
		}
		defer st.Close()

		now := time.Now()
		since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		label := "Today"
		switch {
		case costSince != "":
			t, err := time.ParseInLocation("2006-01-02", costSince, now.Location())
			if err != nil {
				return fmt.Errorf("--since wants YYYY-MM-DD: %w", err)
			}
			since, label = t, "Since "+costSince
		case costMonth:
			since, label = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()), "This month"
		case costWeek:
			weekday := (int(now.Weekday()) + 6) % 7 // Monday = 0
			since, label = since.AddDate(0, 0, -weekday), "This week"
		}

		sessionID := costSession
		if costReceipt && sessionID == "" {
			sessionID, err = st.LatestSessionID()
			if err != nil {
				return err
			}
			if sessionID == "" {
				return fmt.Errorf("no attributed sessions recorded yet")
			}
		}

		if costReceipt {
			// A receipt covers the whole session, not "today".
			since = time.Unix(0, 0)
			label = "Session"
		}

		rows, err := st.SpendSinceFiltered(since, costBy, sessionID)
		if err != nil {
			return err
		}
		if costJSON {
			return json.NewEncoder(os.Stdout).Encode(rows)
		}
		if costReceipt {
			return printReceipt(st, sessionID, rows)
		}

		var total float64
		var unpriced int64
		for _, r := range rows {
			total += r.CostUSD
			unpriced += r.Unpriced
		}
		fmt.Printf("%s (%s)%36s$%.2f\n", label, since.Format("2006-01-02"), "", total)
		for _, r := range rows {
			key := r.Key
			if key == "" {
				key = "(unattributed)"
			}
			// Cache hit rate is the headline number for prompt caching:
			// a session that re-primes its prefix every turn shows near 0
			// here while burning full-price input on tokens it already sent.
			cache := "   —"
			if r.InputTokens > 0 {
				cache = fmt.Sprintf("%3.0f%%", r.CacheHitRate()*100)
			}
			fmt.Printf("  %-28s %8s in / %-8s out  %s cached  $%.2f\n",
				key, humanTokens(r.InputTokens), humanTokens(r.OutputTokens), cache, r.CostUSD)
		}
		if unpriced > 0 {
			fmt.Printf("  ⚠ %d requests on unpriced models (untracked spend) — add pricing in config\n", unpriced)
		}
		if cfg.Budgets != nil && cfg.Budgets.Daily > 0 && label == "Today" && sessionID == "" {
			frac := total / cfg.Budgets.Daily
			fmt.Printf("Daily budget: $%.2f / $%.2f  [%s] %.0f%%\n",
				total, cfg.Budgets.Daily, bar(frac, 12), frac*100)
		}
		return nil
	},
}

func printReceipt(st *store.Store, sessionID string, rows []store.SpendRow) error {
	bounds, ok, err := st.SessionBounds(sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no usage recorded for session %q", sessionID)
	}

	var total float64
	var in, out, cached, unpriced int64
	for _, r := range rows {
		total += r.CostUSD
		in += r.InputTokens
		out += r.OutputTokens
		cached += r.CacheReadTokens
		unpriced += r.Unpriced
	}

	fmt.Println("gremlord cost receipt")
	fmt.Printf("gremlord %s\n", router.Version)
	fmt.Printf("session  %s\n", sessionID)
	if bounds.Profile != "" {
		fmt.Printf("profile  %s\n", bounds.Profile)
	}
	fmt.Printf("from     %s\n", bounds.First.Local().Format(time.RFC3339))
	fmt.Printf("to       %s\n", bounds.Last.Local().Format(time.RFC3339))
	fmt.Printf("requests %d\n", bounds.Requests)
	fmt.Println()
	fmt.Printf("%-28s %8s %8s %7s %10s\n", "model", "in", "out", "cached", "usd")
	for _, r := range rows {
		key := r.Key
		if key == "" {
			key = "(unattributed)"
		}
		cache := "—"
		if r.InputTokens > 0 {
			cache = fmt.Sprintf("%.0f%%", r.CacheHitRate()*100)
		}
		fmt.Printf("%-28s %8s %8s %7s %10.2f\n",
			key, humanTokens(r.InputTokens), humanTokens(r.OutputTokens), cache, r.CostUSD)
	}
	fmt.Println()
	cache := "—"
	if in > 0 {
		cache = fmt.Sprintf("%.0f%%", float64(cached)/float64(in)*100)
	}
	fmt.Printf("total    $%.2f   %s in / %s out   %s cached\n",
		total, humanTokens(in), humanTokens(out), cache)
	if unpriced > 0 {
		fmt.Printf("warning  %d unpriced requests (untracked spend)\n", unpriced)
	}
	return nil
}

func init() {
	costCmd.Flags().BoolVar(&costWeek, "week", false, "this ISO week")
	costCmd.Flags().BoolVar(&costMonth, "month", false, "this calendar month")
	costCmd.Flags().StringVar(&costSince, "since", "", "start date (YYYY-MM-DD)")
	costCmd.Flags().StringVar(&costBy, "by", "model", "group by: model | profile | session")
	costCmd.Flags().StringVar(&costSession, "session", "", "restrict to one session id")
	costCmd.Flags().BoolVar(&costReceipt, "receipt", false, "pasteable one-session receipt (defaults to latest)")
	costCmd.Flags().BoolVar(&costJSON, "json", false, "machine-readable output")
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fK", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}

func bar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	out := make([]rune, width)
	for i := range out {
		if i < filled {
			out[i] = '█'
		} else {
			out[i] = '░'
		}
	}
	return string(out)
}
