package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/launch"
	"github.com/gremlord/gremlord/internal/pricing"
	"github.com/gremlord/gremlord/internal/router"

	"github.com/gremlord/gremlord/internal/wire"
)

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "Model and provider inventory",
}

var modelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured model aliases",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		prices := pricing.Load(dataDir, cfg)
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ALIAS\tPROVIDER\tUPSTREAM MODEL\tCTX\t$IN/MTok\t$OUT/MTok\tKEY")
		aliases := make([]string, 0, len(cfg.Models))
		for a := range cfg.Models {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
		for _, a := range aliases {
			m := cfg.Models[a]
			p := cfg.Providers[m.Provider]
			priceIn, priceOut := "?", "?"
			if pr, ok := prices.Get(m.ID); ok {
				priceIn, priceOut = fmt.Sprintf("%.2f", pr.Input), fmt.Sprintf("%.2f", pr.Output)
			}
			key := "✓"
			if p.APIKeyEnv != "" && p.Key() == "" {
				key = "✗ (" + p.APIKeyEnv + " unavailable)"
			}
			ctx := "-"
			if b := m.ContextBudget(); b > 0 {
				ctx = humanTokens(int64(b))
				if m.EffectiveContext > 0 && m.EffectiveContext < m.ContextWindow {
					ctx += " (of " + humanTokens(int64(m.ContextWindow)) + ")"
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a, m.Provider, m.ID, ctx, priceIn, priceOut, key)
		}
		return tw.Flush()
	},
}

var (
	modelProvider  string
	modelID        string
	modelReasoning string
	modelMaxOutput int
	modelCtxWindow int
	modelEffective int
)

var modelsAddCmd = &cobra.Command{
	Use:   "add <alias>",
	Short: "Add or update a model alias",
	Example: `  gremlord models add gpt  --provider openai --id gpt-5.2 --reasoning effort
  gremlord models add grok --provider xai --id grok-4
  gremlord models add qwen --provider local --id qwen3-coder-30b
  gremlord models add codex --provider codex`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if modelProvider == "" {
			return fmt.Errorf("--provider is required")
		}
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}
		p, ok := cfg.Providers[modelProvider]
		if !ok {
			return fmt.Errorf("unknown provider %q", modelProvider)
		}
		if p.Type != config.ProviderCLI && modelID == "" {
			return fmt.Errorf("--id is required for %s providers", p.Type)
		}
		snippet := fmt.Sprintf("provider: %s\n", modelProvider)
		if modelID != "" {
			snippet += fmt.Sprintf("id: %s\n", yamlQuote(modelID))
		}
		if modelReasoning != "" {
			snippet += "reasoning: " + modelReasoning + "\n"
		}
		if modelMaxOutput > 0 {
			snippet += fmt.Sprintf("max_output: %d\n", modelMaxOutput)
		}
		if modelCtxWindow > 0 {
			snippet += fmt.Sprintf("context_window: %d\n", modelCtxWindow)
		}
		if modelEffective > 0 {
			snippet += fmt.Sprintf("effective_context: %d\n", modelEffective)
		}
		return editConfig(func(doc *config.Doc) error {
			return doc.SetSubtree("models", args[0], snippet)
		}, "model "+args[0]+" -> "+modelProvider+"/"+modelID)
	},
}

var modelsRemoveCmd = &cobra.Command{
	Use:   "remove <alias>",
	Short: "Remove a model alias",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return editConfig(func(doc *config.Doc) error {
			return doc.Delete("models", args[0])
		}, "removed model "+args[0])
	},
}

var modelsTestCmd = &cobra.Command{
	Use:   "test [alias]",
	Short: "Send a 1-token request per model through the router",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		baseURL, token, stop, err := ensureRouter(cmd.Context(), cfg, dataDir)
		if err != nil {
			return err
		}
		defer stop()

		aliases := make([]string, 0, len(cfg.Models))
		if len(args) == 1 {
			aliases = args
		} else {
			for a := range cfg.Models {
				aliases = append(aliases, a)
			}
			sort.Strings(aliases)
		}
		failed := 0
		explicit := len(args) == 1
		for _, alias := range aliases {
			model, ok := cfg.Models[alias]
			if !ok {
				failed++
				fmt.Printf("✗ %-12s model alias not found\n", alias)
				continue
			}
			if !explicit && cfg.Providers[model.Provider].Type == config.ProviderCLI {
				fmt.Printf("· %-12s (cli) — skipped; run %q to invoke it for real\n", alias, "gremlord models test "+alias)
				continue
			}
			start := time.Now()
			timeout := 60 * time.Second
			if cfg.Providers[model.Provider].Type == config.ProviderCLI {
				timeout = 20 * time.Minute
				if cfg.Providers[model.Provider].TimeoutMS > 0 {
					timeout = time.Duration(cfg.Providers[model.Provider].TimeoutMS)*time.Millisecond + time.Minute
				}
			}
			err := probeModel(baseURL, token, alias, timeout)
			if err != nil {
				failed++
				fmt.Printf("✗ %-12s %v\n", alias, err)
			} else {
				fmt.Printf("✓ %-12s ok (%dms)\n", alias, time.Since(start).Milliseconds())
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d model(s) failed", failed)
		}
		return nil
	},
}

func probeModel(baseURL, token, alias string, timeout time.Duration) error {
	body := fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, alias)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", token)
	req.Header.Set("content-type", "application/json")
	if cwd, err := os.Getwd(); err == nil {
		req.Header.Set(wire.HeaderCwd, cwd)
		req.Header.Set(wire.LegacyHeaderCwd, cwd)
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var apiErr anthropic.APIError
		json.NewDecoder(resp.Body).Decode(&apiErr)
		return fmt.Errorf("%d %s: %s", resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message)
	}
	return nil
}

// ensureRouter joins the existing leader or runs one in-process for the
// duration of the command.
func portAvailable(port int) (bool, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false, err
	}
	return true, ln.Close()
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, ln.Close()
}

func ensureRouter(ctx context.Context, cfg *config.Config, dataDir string) (string, string, func(), error) {
	token, err := launch.Token(dataDir)
	if err != nil {
		return "", "", nil, err
	}
	port := cfg.Router.Port
	// A router leader belongs to one gremlord data directory/token. If another
	// install (most commonly a test with a temporary HOME) already owns the
	// configured port, pick another local port rather than joining it and
	// sending that unrelated leader our token/config.
	if discovery, err := router.ReadDiscovery(dataDir); err != nil || discovery.Port != port {
		if available, err := portAvailable(port); err != nil || !available {
			port, err = freePort()
			if err != nil {
				return "", "", nil, fmt.Errorf("selecting temporary router port: %w", err)
			}
		}
	}
	mgr := &router.Manager{Port: port, Token: token, DataDir: dataDir, Log: logger()}
	runCtx, cancel := context.WithCancel(context.Background())
	go mgr.Run(runCtx)
	if err := mgr.Ensure(ctx); err != nil {
		cancel()
		return "", "", nil, err
	}
	return mgr.BaseURL(), token, cancel, nil
}

const pricesURL = "https://raw.githubusercontent.com/gremlord/gremlord/main/internal/pricing/prices.json"

var modelsUpdatePricesCmd = &cobra.Command{
	Use:   "update-prices",
	Short: "Fetch the latest pricing table (no binary update needed)",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		resp, err := http.Get(pricesURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("fetching %s: HTTP %d", pricesURL, resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return err
		}
		var check map[string]json.RawMessage
		if err := json.Unmarshal(data, &check); err != nil {
			return fmt.Errorf("fetched prices are not valid JSON: %w", err)
		}
		path := filepath.Join(dataDir, "prices.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
		fmt.Printf("✓ wrote %s (%d models)\n", path, len(check))
		reloadRouter()
		return nil
	},
}

func init() {
	modelsAddCmd.Flags().StringVar(&modelProvider, "provider", "", "provider name from config")
	modelsAddCmd.Flags().StringVar(&modelID, "id", "", "upstream model id (optional for cli providers)")
	modelsAddCmd.Flags().StringVar(&modelReasoning, "reasoning", "", "none | effort | passive")
	modelsAddCmd.Flags().IntVar(&modelMaxOutput, "max-output", 0, "clamp max_tokens to this output cap")
	modelsAddCmd.Flags().IntVar(&modelCtxWindow, "context-window", 0, "model's real input context window in tokens")
	modelsAddCmd.Flags().IntVar(&modelEffective, "effective-context", 0, "usable context before quality degrades (attention budget)")
	modelsCmd.AddCommand(modelsListCmd, modelsAddCmd, modelsRemoveCmd, modelsTestCmd, modelsUpdatePricesCmd)
}
