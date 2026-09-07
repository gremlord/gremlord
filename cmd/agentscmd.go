package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/agents"
)

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Claude Code subagents for your model aliases",
	Long: `Claude Code's built-in Agent tool only accepts a fixed model parameter
(sonnet | opus | haiku | fable), so a routed alias like "qwen" can't be
selected through it. A subagent definition's model frontmatter has no such
limit, so gremlord generates one subagent per configured model alias —
making every model selectable by name (subagent_type: "gremlord-qwen").

Definitions live in ~/.claude/agents/gremlord-<alias>.md. Only files with the
"gremlord-" prefix are ever written or removed; your own agents are untouched.`,
}

var agentsListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show the subagents implied by your model aliases, and their state on disk",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}
		dir, err := agents.Dir()
		if err != nil {
			return err
		}
		want := agents.Desired(cfg)
		if len(want) == 0 {
			fmt.Println("no model aliases configured — add one with `gremlord models add`")
			return nil
		}
		changes, err := agents.Diff(cfg, dir)
		if err != nil {
			return err
		}
		pending := map[string]string{}
		for _, c := range changes {
			pending[c.Name] = c.Kind
		}

		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "SUBAGENT\tALIAS\tSTATE")
		for _, d := range want {
			state := "in sync"
			if k, ok := pending[d.Name]; ok {
				state = k + " pending"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", d.Name, d.Alias, state)
		}
		for _, c := range changes {
			if c.Kind == "remove" {
				fmt.Fprintf(tw, "%s\t(alias gone)\tremove pending\n", c.Name)
			}
		}
		tw.Flush()
		fmt.Printf("\n%s\n", dir)
		if len(changes) > 0 {
			fmt.Println("run `gremlord agents sync` to apply")
		}
		return nil
	},
}

var agentsSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Write subagent definitions for your model aliases",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		dir, err := agents.Dir()
		if err != nil {
			return err
		}
		changes, err := agents.Sync(cfg, dir)
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			fmt.Println("✓ subagents already in sync with your model aliases")
			return nil
		}
		for _, c := range changes {
			fmt.Printf("  %-8s %s\n", c.Kind, c.Name)
		}
		fmt.Printf("✓ synced %d subagent(s) in %s\n", len(changes), dir)
		fmt.Println("  restart Claude Code sessions to pick them up")
		agents.ClearDeclined(dataDir)
		return nil
	},
}

func init() {
	agentsCmd.AddCommand(agentsListCmd, agentsSyncCmd)
	rootCmd.AddCommand(agentsCmd)
}
