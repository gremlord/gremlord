package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/clauder"
	"github.com/gremlord/gremlord/internal/router"
	"github.com/gremlord/gremlord/internal/selfupdate"
)

var flagUpdateCheck bool

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update gremlord itself to the latest release",
	Long: `Checks GitHub for the latest gremlord release and, if newer than the
running binary, downloads it and replaces the current executable in place.

This updates gremlord only — Claude Code updates itself independently.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		rel, err := selfupdate.Latest(ctx)
		if err != nil {
			return err
		}

		current := strings.TrimPrefix(router.Version, "v")
		latest := strings.TrimPrefix(rel.TagName, "v")

		if router.Version == "dev" {
			fmt.Fprintln(os.Stderr, "gremlord: running a dev build, version comparison skipped")
		} else if current == latest {
			fmt.Printf("gremlord %s is already the latest version.\n", router.Version)
			return nil
		}

		fmt.Printf("gremlord %s -> %s\n", router.Version, rel.TagName)
		if flagUpdateCheck {
			fmt.Println("Run `gremlord update` (without --check) to install.")
			printRestartReport(rel.TagName)
			return nil
		}

		exePath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locating running binary: %w", err)
		}

		tmp := exePath + ".download"
		defer os.Remove(tmp)

		fmt.Println("Downloading...")
		if err := selfupdate.Download(ctx, rel.TagName, tmp); err != nil {
			return err
		}
		if err := selfupdate.Apply(exePath, tmp); err != nil {
			return err
		}

		fmt.Printf("Updated to %s.\n", rel.TagName)
		printRestartReport(rel.TagName)

		// Running sessions host the router in-process; the new binary only
		// takes over when they restart. Claude Code's native peer messaging
		// is session-to-session only, with no CLI to reach it from out here,
		// so we still notify through clauder — which registers instances from
		// its MCP server and so sees sessions we no longer wrap.
		msg := fmt.Sprintf("[gremlord] gremlord was updated to %s. If this session was launched via `gremlord`, "+
			"its router is still running the old version — please tell the user this session should be "+
			"restarted (exit and re-run `gremlord`) at a convenient moment to pick up the update.", rel.TagName)
		if n := clauder.Broadcast(msg); n > 0 {
			fmt.Printf("Notified %d running instance(s) via clauder to restart when convenient.\n", n)
		}
		return nil
	},
}

func init() {
	updateCmd.Flags().BoolVar(&flagUpdateCheck, "check", false, "check for an update without installing it")
	rootCmd.AddCommand(updateCmd)
}
