package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/peers"
)

var peersCmd = &cobra.Command{
	Use:   "peers [approximate name]",
	Short: "Find another Claude Code session to message",
	Long: `Resolve the approximate name people use for a session ("the labs-service
one") to a specific session, matching on both session name and working
directory. With no argument, lists every session that can receive a message.

Sending is done from inside a session: call ListAgents for the [ref], then
SendMessage to "<name> [ref]".`,
	Args:          cobra.ArbitraryArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		all, err := peers.Load(filepath.Join(home, ".claude", "sessions"))
		if err != nil {
			return err
		}

		selfID := os.Getenv("CLAUDE_CODE_SESSION_ID")
		var live []peers.Session
		var self *peers.Session
		var stale int
		for _, s := range all {
			if !pidAlive(s.PID) {
				continue
			}
			if s.SessionID == selfID {
				self = &s
				continue
			}
			if !s.Reachable() {
				stale++
				continue
			}
			live = append(live, s)
		}

		// Printed last on every path, including the no-match error: unreachable
		// sessions are usually the reason a lookup came up empty.
		defer printStale(stale)

		if len(live) == 0 {
			fmt.Println("No other session can receive a message right now.")
			return nil
		}

		query := strings.Join(args, " ")
		ranked := peers.Match(live, query)
		if len(ranked) == 0 {
			// Being asked to message the session you already are is a plausible
			// mix-up; saying so beats "no match" and a retry loop.
			if self != nil && len(peers.Match([]peers.Session{*self}, query)) > 0 {
				return fmt.Errorf("%q is this session (%s) — nothing to message", query, self.Name)
			}
			return fmt.Errorf("no reachable session matches %q — run `gremlord peers` to see them all", query)
		}

		if query == "" {
			fmt.Printf("Sessions you can message (%d):\n", len(ranked))
			for _, r := range ranked {
				fmt.Println(line(r.Session, home))
			}
			return nil
		}

		if peers.Ambiguous(ranked) {
			fmt.Printf("%q matches these equally well — ask which one is meant, don't guess:\n", query)
			for _, r := range ranked {
				if r.Score < ranked[0].Score {
					break
				}
				fmt.Println(line(r.Session, home))
			}
			return nil
		}

		fmt.Printf("Best match for %q:\n%s\n", query, line(ranked[0].Session, home))
		if len(ranked) > 1 {
			fmt.Println("\nAlso matched, less closely:")
			for _, r := range ranked[1:] {
				fmt.Println(line(r.Session, home))
			}
		}
		fmt.Printf("\nTo message it: call ListAgents for its [ref], then SendMessage to\n"+
			"%q — the bare name works after first contact.\n", ranked[0].Name+" [ref]")
		return nil
	},
}

func line(s peers.Session, home string) string {
	return fmt.Sprintf("  %-30s %-5s %-38s started %s",
		s.Name, s.Status, tildePath(s.Dir, home), humanAge(s.Started))
}

func printStale(n int) {
	if n == 0 {
		return
	}
	fmt.Printf("\n· %d other session(s) predate native messaging and cannot be reached —\n"+
		"  restart them (exit and relaunch) to make them addressable.\n", n)
}

func tildePath(dir, home string) string {
	if home != "" && strings.HasPrefix(dir, home) {
		return "~" + dir[len(home):]
	}
	return dir
}

func init() {
	rootCmd.AddCommand(peersCmd)
}
