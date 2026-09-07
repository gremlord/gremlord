// Package cmd is the gremlord CLI.
package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/launch"
	"github.com/gremlord/gremlord/internal/logrotate"
	"github.com/gremlord/gremlord/internal/router"
)

var (
	flagProfile     string
	flagModel       string
	flagName        string
	flagNoClauder   bool
	flagPassthrough bool
)

var rootCmd = &cobra.Command{
	Use:   "gremlord [flags] [-- claude args...]",
	Short: "Multi-model, cost-controlled harness wrapping Claude Code",
	Long: `gremlord launches Claude Code through a local router that can serve
Anthropic, OpenAI, xAI, and open-weight models, with budgets and spend
tracking. Everything after -- is passed to claude verbatim.`,
	Version:       router.Version,
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		claudeArgs := args
		if at := cmd.ArgsLenAtDash(); at >= 0 {
			claudeArgs = args[at:]
		}
		return launch.Run(cmd.Context(), cfg, dataDir, launch.Options{
			Profile:      flagProfile,
			ModelFlag:    flagModel,
			InstanceName: flagName,
			Passthrough:  flagPassthrough,
			ClaudeArgs:   claudeArgs,
		}, logger())
	},
}

func init() {
	rootCmd.Flags().StringVarP(&flagProfile, "profile", "p", "", "profile from ~/.gremlord/config.yaml")
	rootCmd.Flags().StringVar(&flagModel, "model", "", "one-shot main-model alias override")
	rootCmd.Flags().StringVar(&flagName, "name", "", "session name (forwarded to claude)")
	rootCmd.Flags().BoolVar(&flagNoClauder, "no-clauder", false, "")
	// Every session is a bare claude now, so there is no wrap layer to opt out
	// of. Kept as an accepted no-op so existing aliases and scripts still run.
	rootCmd.Flags().MarkDeprecated("no-clauder", "sessions always launch claude directly; this flag does nothing")
	rootCmd.Flags().BoolVar(&flagPassthrough, "passthrough", false, "skip the router (subscription billing, no tracking)")
	rootCmd.AddCommand(routerCmd, costCmd, modelsCmd, setupCmd, contextCmd, evalCmd)
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "gremlord:", err)
		os.Exit(1)
	}
}

func loadConfig() (*config.Config, string, error) {
	dataDir, err := config.DataDir()
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load()
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("no config found — run `gremlord setup` first")
	}
	if err != nil {
		return nil, "", err
	}
	return cfg, dataDir, nil
}

// Router log ceiling: the current file plus logMaxGenerations archives, so
// the log costs at most logMaxBytes*(1+logMaxGenerations) on disk. It used to
// have no ceiling at all and grew past the usage database beside it.
const (
	logMaxBytes       = 8 << 20 // 8 MiB
	logMaxGenerations = 3
)

func logger() *slog.Logger {
	dataDir, err := config.DataDir()
	if err == nil {
		if w, err := logrotate.Open(filepath.Join(dataDir, "router.log"),
			logMaxBytes, logMaxGenerations); err == nil {
			return slog.New(slog.NewTextHandler(w, nil))
		}
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
