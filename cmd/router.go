package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/launch"
	"github.com/gremlord/gremlord/internal/router"
)

var routerCmd = &cobra.Command{
	Use:   "router",
	Short: "Inspect or run the shared router",
}

var routerRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the router in the foreground (headless leader, for debugging/servers)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		token, err := launch.Token(dataDir)
		if err != nil {
			return err
		}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		mgr := &router.Manager{Port: cfg.Router.Port, Token: token, DataDir: dataDir, Log: log}
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		fmt.Fprintf(os.Stderr, "gremlord router on %s (ctrl-c to stop)\n", mgr.BaseURL())
		return mgr.Run(ctx)
	},
}

var routerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the current router leader",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		d, err := router.ReadDiscovery(dataDir)
		if err != nil {
			fmt.Println("no router running")
			return nil
		}
		fmt.Printf("leader pid %d on 127.0.0.1:%d (version %s, since %s)\n",
			d.PID, d.Port, d.Version, d.StartedAt)
		return nil
	},
}

func init() {
	routerCmd.AddCommand(routerRunCmd, routerStatusCmd)
}
