package cmd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/launch"
	"github.com/gremlord/gremlord/internal/router"

	"github.com/gremlord/gremlord/internal/wire"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Read or edit ~/.gremlord/config.yaml from the terminal",
}

var configGetCmd = &cobra.Command{
	Use:   "get <dot.path>",
	Short: "Print a config value (e.g. `gremlord config get budgets.daily`)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		doc, err := config.LoadDoc()
		if err != nil {
			return err
		}
		v, err := doc.Get(args[0])
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <dot.path> <value>",
	Short: "Set a config value (e.g. `gremlord config set budgets.daily 25`)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return editConfig(func(doc *config.Doc) error { return doc.Set(args[0], args[1]) },
			fmt.Sprintf("set %s = %s", args[0], args[1]))
	},
}

// editConfig applies one mutation, saves (with validation), and hot-reloads
// the live router leader so changes apply to running sessions.
func editConfig(mutate func(*config.Doc) error, what string) error {
	doc, err := config.LoadDoc()
	if err != nil {
		return err
	}
	if err := mutate(doc); err != nil {
		return err
	}
	if err := doc.Save(); err != nil {
		return err
	}
	fmt.Println("✓", what)
	reloadRouter()
	return nil
}

// reloadRouter best-effort POSTs the reload path to the current leader,
// falling back to the pre-rename path when an agentic router holds the port.
func reloadRouter() {
	cfg, dataDir, err := loadConfig()
	if err != nil {
		return
	}
	token, err := launch.Token(dataDir)
	if err != nil {
		return
	}
	if _, err := router.ReadDiscovery(dataDir); err != nil {
		return // no leader running
	}
	for _, path := range []string{wire.PathReload, wire.LegacyPathReload} {
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Router.Port, path), nil)
		if err != nil {
			return
		}
		req.Header.Set("x-api-key", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			fmt.Println("✓ live router reloaded")
			return
		}
		if resp.StatusCode != http.StatusNotFound {
			return
		}
	}
}

// yamlQuote quotes a string for embedding in a YAML snippet.
func yamlQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func init() {
	configCmd.AddCommand(configGetCmd, configSetCmd)
	rootCmd.AddCommand(configCmd, providersCmd, profilesCmd, budgetCmd)
}
