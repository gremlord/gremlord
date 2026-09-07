package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/launch"
	"github.com/gremlord/gremlord/internal/peers"
)

// registerStatusline merges a statusLine entry into ~/.claude/settings.json
// (read-merge-write; an existing different statusline is left alone).
func registerStatusline() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "settings.json")
	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("existing %s is not valid JSON: %w", path, err)
		}
	}
	if existing, ok := settings["statusLine"]; ok {
		if m, ok := existing.(map[string]any); ok && m["command"] == "gremlord statusline" {
			fmt.Println("✓ statusline already registered")
			return nil
		}
		fmt.Println("· a statusline is already configured in ~/.claude/settings.json — leaving it alone")
		fmt.Println(`  (to use gremlord's: set "statusLine": {"type":"command","command":"gremlord statusline"})`)
		return nil
	}
	settings["statusLine"] = map[string]any{"type": "command", "command": "gremlord statusline"}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Println("✓ registered gremlord statusline in ~/.claude/settings.json")
	return nil
}

// registerPeerGuidance teaches every session — gremlord-launched or not — how
// to resolve an approximate session name via `gremlord peers`. It lives in
// ~/.claude/CLAUDE.md between markers, so re-running setup refreshes the block
// without touching anything else in the file.
func registerPeerGuidance() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude", "CLAUDE.md")
	action, err := peers.InstallGuidance(path)
	if err != nil {
		return err
	}
	switch action {
	case peers.Created:
		fmt.Println("✓ wrote peer-resolution guidance to ~/.claude/CLAUDE.md")
	case peers.Updated:
		fmt.Println("✓ refreshed peer-resolution guidance in ~/.claude/CLAUDE.md")
	default:
		fmt.Println("✓ peer-resolution guidance already current in ~/.claude/CLAUDE.md")
	}
	return nil
}

const defaultConfig = `# ~/.gremlord/config.yaml — edit directly or via the gremlord CLI.
version: 1
default_profile: main

router:
  port: 41100          # fixed — leader election binds this port

providers:
  anthropic:
    type: anthropic                      # native passthrough, no translation
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY
  # openai:
  #   type: openai                       # OpenAI-dialect translation (phase 2)
  #   base_url: https://api.openai.com/v1
  #   api_key_env: OPENAI_API_KEY
  #   max_tokens_param: max_completion_tokens
  # xai:
  #   type: openai
  #   base_url: https://api.x.ai/v1
  #   api_key_env: XAI_API_KEY
  # local:
  #   type: openai                       # Ollama / vLLM / any OpenAI-compatible server
  #   base_url: http://localhost:11434/v1
  #   api_key_env: ""
  # codex:
  #   type: cli                          # whole-task delegation to the official CLI
  #   dialect: codex                     # codex | grok
  #   sandbox: workspace-write           # codex only

models:                # alias -> upstream; aliases flow into ANTHROPIC_MODEL
  opus:   {provider: anthropic, id: claude-opus-4-8}
  sonnet: {provider: anthropic, id: claude-sonnet-5}
  haiku:  {provider: anthropic, id: claude-haiku-4-5}
  # gpt:   {provider: openai, id: gpt-5.2, reasoning: effort, max_output: 16384}
  # grok:  {provider: xai, id: grok-4}
  # codex: {provider: codex}             # id optional; explicit subagent use only

profiles:
  main:
    model: sonnet
    small_fast: haiku
    tiers: {opus: opus, sonnet: sonnet, haiku: haiku}
  subscription:
    passthrough: true  # normal claude login, no router, no cost tracking

budgets:
  daily: 50.00
  warn_at: 0.8
  hard_stop: true
`

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "First-run configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		dataDir, err := config.DataDir()
		if err != nil {
			return err
		}
		ensureClaude()

		path, _ := config.Path()
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("✓ config already exists at %s (edit it directly)\n", path)
		} else {
			if _, err := config.Parse([]byte(defaultConfig)); err != nil {
				return fmt.Errorf("internal: default config invalid: %w", err)
			}
			if err := os.WriteFile(path, []byte(defaultConfig), 0o600); err != nil {
				return err
			}
			fmt.Printf("✓ wrote %s\n", path)
		}

		if _, err := launch.Token(dataDir); err != nil {
			return err
		}
		fmt.Println("✓ router token ready")

		envPath := filepath.Join(dataDir, "env")
		if _, err := os.Stat(envPath); os.IsNotExist(err) {
			template := "# gremlord provider keys (0600). The router reads this file directly,\n" +
				"# so keys work no matter which shell launches a session.\n" +
				"# Process environment variables take precedence when set.\n" +
				"ANTHROPIC_API_KEY=\n" +
				"OPENAI_API_KEY=\n" +
				"XAI_API_KEY=\n"
			if err := os.WriteFile(envPath, []byte(template), 0o600); err == nil {
				fmt.Printf("✓ created %s — put provider keys there\n", envPath)
			}
		}
		anthCfg := config.Provider{APIKeyEnv: "ANTHROPIC_API_KEY"}
		if anthCfg.Key() == "" {
			fmt.Println("⚠ ANTHROPIC_API_KEY not found — the router bills via API keys, not your Claude subscription.")
			fmt.Printf("  Put it in %s (or export it), or use `gremlord -p subscription` for normal subscription claude.\n", envPath)
		} else {
			fmt.Println("✓ ANTHROPIC_API_KEY available")
		}

		if err := registerStatusline(); err != nil {
			fmt.Printf("⚠ could not register statusline: %v\n", err)
		}

		if err := registerPeerGuidance(); err != nil {
			fmt.Printf("⚠ could not write peer guidance: %v\n", err)
		}

		if ensureClauder() {
			fmt.Println("  clauder's persistent memory is available to sessions over its own MCP server")
			fmt.Println("  (cross-instance messaging is native to Claude Code — nothing to configure)")
		}

		fmt.Println("\nDone. Start a session with: gremlord")
		return nil
	},
}
