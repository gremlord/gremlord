package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
	"gopkg.in/yaml.v3"
)

var profilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "Inspect launch profiles",
}

var profilesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List profiles",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tMODEL\tSMALL/FAST\tBUDGET\tMODE")
		names := make([]string, 0, len(cfg.Profiles))
		for n := range cfg.Profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := cfg.Profiles[n]
			mode := "router"
			if p.Passthrough {
				mode = "passthrough (subscription)"
			}
			budget := "—"
			if p.Budget != nil && p.Budget.Daily > 0 {
				budget = fmt.Sprintf("$%.2f/day", p.Budget.Daily)
			}
			def := ""
			if n == cfg.DefaultProfile {
				def = " (default)"
			}
			fmt.Fprintf(tw, "%s%s\t%s\t%s\t%s\t%s\n", n, def, orDefault(p.Model, "—"), orDefault(p.SmallFast, "—"), budget, mode)
		}
		return tw.Flush()
	},
}

var profilesShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show one profile as YAML",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}
		p, ok := cfg.Profiles[args[0]]
		if !ok {
			return fmt.Errorf("profile %q not found", args[0])
		}
		out, err := yaml.Marshal(map[string]config.Profile{args[0]: p})
		if err != nil {
			return err
		}
		fmt.Print(string(out))
		return nil
	},
}

var budgetProfile string

var budgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Manage spend budgets",
}

var budgetShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show budget caps and spend against them",
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

		printBudget := func(label string, b *config.Budget, profile string) {
			if b == nil {
				return
			}
			now := time.Now()
			dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			weekStart := dayStart.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
			monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
			stop := "warn only"
			if b.HardStop == nil || *b.HardStop {
				stop = "hard stop"
			}
			warnAt := b.WarnAt
			if warnAt == 0 {
				warnAt = 0.8
			}
			fmt.Printf("%s  (%s, warn at %.0f%%)\n", label, stop, warnAt*100)
			for _, w := range []struct {
				name  string
				cap   float64
				since time.Time
			}{
				{"daily", b.Daily, dayStart},
				{"weekly", b.Weekly, weekStart},
				{"monthly", b.Monthly, monthStart},
			} {
				if w.cap <= 0 {
					continue
				}
				spent, _ := st.TotalSince(w.since, profile, "")
				frac := spent / w.cap
				fmt.Printf("  %-8s $%.2f / $%.2f  [%s] %.0f%%\n",
					w.name, spent, w.cap, bar(frac, 12), frac*100)
			}
		}

		if cfg.Budgets == nil {
			fmt.Println("No global budget set. Set one with: gremlord budget set --daily 25 --monthly 400")
		} else {
			printBudget("Global", cfg.Budgets, "")
		}
		for _, name := range sortedProfileNames(cfg) {
			if p := cfg.Profiles[name]; p.Budget != nil {
				fmt.Println()
				printBudget("Profile "+name, p.Budget, name)
			}
		}
		return nil
	},
}

var budgetSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set budget caps (global, or per profile with --profile)",
	Example: `  gremlord budget set --daily 25 --monthly 400
  gremlord budget set --profile cheap --daily 5`,
	RunE: func(cmd *cobra.Command, args []string) error {
		base := "budgets"
		if budgetProfile != "" {
			base = "profiles." + budgetProfile + ".budget"
		}
		changed := false
		return editConfig(func(doc *config.Doc) error {
			for _, f := range []string{"daily", "weekly", "monthly", "warn_at"} {
				if cmd.Flags().Changed(f) {
					v, _ := cmd.Flags().GetFloat64(f)
					if err := doc.Set(base+"."+f, fmt.Sprintf("%g", v)); err != nil {
						return err
					}
					changed = true
				}
			}
			if cmd.Flags().Changed("hard-stop") {
				v, _ := cmd.Flags().GetBool("hard-stop")
				if err := doc.Set(base+".hard_stop", fmt.Sprint(v)); err != nil {
					return err
				}
				changed = true
			}
			if !changed {
				return fmt.Errorf("nothing to set — pass --daily / --weekly / --monthly / --warn-at / --hard-stop")
			}
			return nil
		}, "budget updated ("+base+")")
	},
}

func init() {
	budgetSetCmd.Flags().StringVar(&budgetProfile, "profile", "", "set the budget on a profile instead of globally")
	budgetSetCmd.Flags().Float64("daily", 0, "daily cap in USD")
	budgetSetCmd.Flags().Float64("weekly", 0, "weekly cap in USD")
	budgetSetCmd.Flags().Float64("monthly", 0, "monthly cap in USD")
	budgetSetCmd.Flags().Float64("warn_at", 0, "warn threshold as a fraction (default 0.8)")
	budgetSetCmd.Flags().Bool("hard-stop", true, "block requests when over cap (false = warn only)")
	budgetCmd.AddCommand(budgetSetCmd, budgetShowCmd)
	profilesCmd.AddCommand(profilesListCmd, profilesShowCmd)
}

// sortedProfileNames returns profile names in stable order for display.
func sortedProfileNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
