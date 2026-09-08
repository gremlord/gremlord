package cmd

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
)

var (
	provType    string
	provBase    string
	provKeyEnv  string
	provMaxTok  string
	provMaxReq  int64
	provDialect string
	provCommand string
	provSandbox string
	provTimeout int
	provAPI     string
)

var providersCmd = &cobra.Command{
	Use:   "providers",
	Short: "Manage upstream providers",
}

var providersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured providers",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tTYPE\tBASE URL\tKEY")
		names := make([]string, 0, len(cfg.Providers))
		for n := range cfg.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := cfg.Providers[n]
			key := "✓"
			switch {
			case p.Type == config.ProviderCLI:
				key = "· (subscription login)"
			case p.APIKey != "":
				key = "✓ (literal)"
			case p.APIKeyEnv == "":
				key = "· (no auth)"
			case p.Key() == "":
				key = "✗ (" + p.APIKeyEnv + " unavailable)"
			}
			base := p.BaseURL
			if p.Type == config.ProviderCLI {
				base = "(" + p.Bin() + " CLI, " + p.Dialect + " dialect)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, p.Type, base, key)
		}
		return tw.Flush()
	},
}

var providersAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add or update a provider",
	Long: `Add an upstream provider. Examples:

  gremlord providers add openai --type openai --base-url https://api.openai.com/v1 \
      --key-env OPENAI_API_KEY --max-tokens-param max_completion_tokens
  gremlord providers add xai   --type openai --base-url https://api.x.ai/v1 --key-env XAI_API_KEY
  gremlord providers add local --type openai --base-url http://localhost:11434/v1 --key-env ""

A "cli" provider delegates whole tasks to a locally installed coding-agent
CLI running under your own subscription login (codex login / grok login):

  gremlord providers add codex --type cli --dialect codex --sandbox workspace-write
  gremlord providers add grokcli --type cli --dialect grok`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if provType == config.ProviderCLI {
			if provDialect == "" {
				return fmt.Errorf("--dialect is required for cli providers (%s | %s)", config.CLIDialectCodex, config.CLIDialectGrok)
			}
			if provBase != "" || provKeyEnv != "" || provMaxTok != "" || provMaxReq != 0 {
				return fmt.Errorf("--base-url/--key-env/--max-tokens-param/--max-request-bytes do not apply to cli providers")
			}
			if provTimeout < 0 {
				return fmt.Errorf("--timeout-ms must be >= 0")
			}
			snippet := fmt.Sprintf("type: %s\ndialect: %s\n", provType, provDialect)
			if provCommand != "" {
				snippet += fmt.Sprintf("command: %s\n", yamlQuote(provCommand))
			}
			if provSandbox != "" {
				snippet += fmt.Sprintf("sandbox: %s\n", provSandbox)
			}
			if provTimeout > 0 {
				snippet += fmt.Sprintf("timeout_ms: %d\n", provTimeout)
			}
			return editConfig(func(doc *config.Doc) error {
				return doc.SetSubtree("providers", args[0], snippet)
			}, "provider "+args[0])
		}
		if provType != config.ProviderAnthropic && provType != config.ProviderOpenAI {
			return fmt.Errorf("--type must be %q, %q, or %q", config.ProviderAnthropic, config.ProviderOpenAI, config.ProviderCLI)
		}
		if provBase == "" {
			return fmt.Errorf("--base-url is required")
		}
		snippet := fmt.Sprintf("type: %s\nbase_url: %s\napi_key_env: %s\n",
			provType, yamlQuote(provBase), yamlQuote(provKeyEnv))
		if provMaxTok != "" {
			snippet += fmt.Sprintf("max_tokens_param: %s\n", provMaxTok)
		}
		if provMaxReq > 0 {
			snippet += fmt.Sprintf("max_request_bytes: %d\n", provMaxReq)
		}
		if provAPI != "" {
			snippet += fmt.Sprintf("api: %s\n", provAPI)
		}
		return editConfig(func(doc *config.Doc) error {
			return doc.SetSubtree("providers", args[0], snippet)
		}, "provider "+args[0])
	},
}

var providersRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return editConfig(func(doc *config.Doc) error {
			return doc.Delete("providers", args[0])
		}, "removed provider "+args[0])
	},
}

func init() {
	providersAddCmd.Flags().StringVar(&provType, "type", "openai", "provider dialect: anthropic | openai | cli")
	providersAddCmd.Flags().StringVar(&provBase, "base-url", "", "API base URL")
	providersAddCmd.Flags().StringVar(&provKeyEnv, "key-env", "", "env var holding the API key (empty = no auth)")
	providersAddCmd.Flags().StringVar(&provMaxTok, "max-tokens-param", "", "max_tokens | max_completion_tokens")
	providersAddCmd.Flags().Int64Var(&provMaxReq, "max-request-bytes", 0, "upstream request body cap in bytes (refuses oversized requests pre-dispatch; 0 = none)")
	providersAddCmd.Flags().StringVar(&provDialect, "dialect", "", "cli providers: codex | grok")
	providersAddCmd.Flags().StringVar(&provCommand, "command", "", "cli providers: binary name or path (default: the dialect name)")
	providersAddCmd.Flags().StringVar(&provSandbox, "sandbox", "", "cli providers (codex only): read-only | workspace-write | danger-full-access")
	providersAddCmd.Flags().IntVar(&provTimeout, "timeout-ms", 0, "cli providers: per-delegation deadline (0 = 20m default)")
	providersAddCmd.Flags().StringVar(&provAPI, "api", "", "openai providers: chat_completions (default) | responses")
	providersCmd.AddCommand(providersListCmd, providersAddCmd, providersRemoveCmd)
}
