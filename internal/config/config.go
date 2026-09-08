// Package config loads and validates ~/.gremlord/config.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	// ProviderCLI delegates to a locally installed coding-agent CLI (Codex,
	// Grok Build) running under the user's own subscription login, instead of
	// an HTTP endpoint. gremlord never touches the CLI's credentials — the
	// binary authenticates itself from its own cached login.
	ProviderCLI = "cli"

	// CLI dialects — a closed set because the dialect determines argv shape,
	// how the task text is passed, and output extraction.
	CLIDialectCodex = "codex" // OpenAI Codex CLI: `codex exec`
	CLIDialectGrok  = "grok"  // xAI Grok Build CLI: `grok -p`

	// OpenAI-dialect API flavors. chat_completions is the default and
	// what every OpenAI-compatible upstream (xAI, vLLM, Ollama, …) speaks.
	// responses is OpenAI's /v1/responses — required for models that
	// reject function tools combined with reasoning_effort on
	// /v1/chat/completions (gpt-6-astra and similar).
	APIChatCompletions = "chat_completions"
	APIResponses       = "responses"

	DefaultPort = 41100
)

type Config struct {
	Version        int                  `yaml:"version"`
	DefaultProfile string               `yaml:"default_profile"`
	Router         Router               `yaml:"router"`
	Providers      map[string]Provider  `yaml:"providers"`
	Models         map[string]Model     `yaml:"models"`
	Routing        map[string]RouteRule `yaml:"routing"`
	Profiles       map[string]Profile   `yaml:"profiles"`
	Budgets        *Budget              `yaml:"budgets"`
	Pricing        map[string]Price     `yaml:"pricing"`
	// SplashSeconds holds the launch banner for this many seconds before
	// Claude Code takes the terminal. nil = default, 0 = off.
	SplashSeconds *int `yaml:"splash_seconds"`
}

// RouteRule is a dynamic alias: a cheap classifier model assesses each new
// user turn and picks a tier, so e.g. `model: auto` plans on a frontier
// model and executes on open weights without manual switching.
type RouteRule struct {
	Classifier string            `yaml:"classifier"` // model alias used to classify
	Default    string            `yaml:"default"`    // tier when classification fails ("" = standard)
	Tiers      map[string]string `yaml:"tiers"`      // deep/standard/light -> model alias
	// Tasks optionally maps a fixed set of task labels (see TaskLabels) to a
	// model alias that overrides the tier pick for that kind of work — e.g.
	// routing security_review to a specific reviewer model regardless of
	// which tier the request would otherwise land on. Absent/empty means
	// exact tier-only behavior: no combined task classification happens, and
	// routing is byte-for-byte identical to a rule with no Tasks at all.
	Tasks map[string]string `yaml:"tasks"`
	// ContextGauge picks the budget the client's context gauge is scaled
	// against for this rule (see internal/router/gauge.go):
	//
	//   "max"   (default) the largest budget the rule can route to. The
	//           session keeps growing until the biggest tier is full;
	//           turns that outgrow a smaller tier are remapped up to one
	//           that fits by the existing size-aware routing.
	//   "min"   the smallest budget. Every tier stays reachable at any
	//           conversation length, at the cost of compacting as early
	//           as the smallest window demands.
	//   "model" legacy per-request scaling: the gauge tracks whichever
	//           model served the last turn, and jumps when the tier
	//           changes.
	ContextGauge string `yaml:"context_gauge"`
}

// Context gauge policies for RouteRule.ContextGauge.
const (
	GaugeMax   = "max"
	GaugeMin   = "min"
	GaugeModel = "model"
)

// TaskLabels is the fixed, closed set of task labels a task-aware routing
// rule may map. Fixed (not user-extensible) because the classifier prompt
// enumerates them by name and the allow-list parser validates classifier
// output against exactly this set — fail-open (treat as "no task") for
// anything else, never a dynamically-extended list.
var TaskLabels = []string{
	"implementation",
	"sql_data",
	"debugging",
	"code_review",
	"architecture",
	"security_review",
	"critical_review",
}

// IsTaskLabel reports whether s (expected already lowercased/trimmed) is one
// of the fixed TaskLabels.
func IsTaskLabel(s string) bool {
	for _, l := range TaskLabels {
		if l == s {
			return true
		}
	}
	return false
}

type Router struct {
	Port int `yaml:"port"`
}

type Provider struct {
	Type      string `yaml:"type"` // "anthropic" | "openai" | "cli"
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"api_key"`
	// MaxTokensParam is the OpenAI-dialect parameter name for the output
	// limit: "max_tokens" (default) or "max_completion_tokens". Ignored
	// when the resolved API flavor is responses (which uses
	// max_output_tokens).
	MaxTokensParam string `yaml:"max_tokens_param"`
	// API selects the OpenAI-dialect wire format: chat_completions
	// (default) or responses. openai providers only; a model's api
	// overrides the provider's.
	API string `yaml:"api"`
	// MaxRequestBytes is the upstream's request body size cap (e.g. an nginx
	// client_max_body_size). A request larger than this is refused before
	// dispatch with a clean 400 instead of a mangled upstream 413 retry loop.
	// 0 means unknown/no cap. Sits on the provider because the cap belongs to
	// the upstream edge, not the model.
	MaxRequestBytes int `yaml:"max_request_bytes"`

	// Dialect selects which coding-agent CLI a "cli" provider shells out to:
	// CLIDialectCodex or CLIDialectGrok. cli providers only.
	Dialect string `yaml:"dialect"`
	// Command overrides the CLI binary name or path. Empty defaults to the
	// dialect name ("codex"/"grok"). cli providers only.
	Command string `yaml:"command"`
	// Sandbox is passed through to Codex as --sandbox (read-only |
	// workspace-write | danger-full-access). codex dialect only; empty keeps
	// the CLI's own default.
	Sandbox string `yaml:"sandbox"`
	// TimeoutMS bounds a single delegated CLI run. 0 means the built-in
	// default (20 minutes). cli providers only.
	TimeoutMS int `yaml:"timeout_ms"`
}

// Bin returns the CLI binary to invoke for a cli provider: the configured
// Command, else the dialect name.
func (p Provider) Bin() string {
	if p.Command != "" {
		return p.Command
	}
	return p.Dialect
}

// Key resolves the provider's API key: config literal, then process
// environment, then ~/.gremlord/env (so keys don't depend on which shell
// launched the router leader). Empty is valid for unauthenticated local
// endpoints.
func (p Provider) Key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv == "" {
		return ""
	}
	if v := os.Getenv(p.APIKeyEnv); v != "" {
		return v
	}
	return EnvFileLookup(p.APIKeyEnv)
}

type Model struct {
	Provider string `yaml:"provider"`
	ID       string `yaml:"id"`
	// Reasoning: "" (no reasoning param, sampling kept), "none" (like ""
	// but also explicitly sends reasoning_effort=none — required by
	// GPT-5-class models to accept function tools on
	// /v1/chat/completions), "effort" (map budget_tokens to
	// reasoning_effort, sampling dropped), "passive" (model always
	// reasons; parse reasoning_content, sampling kept). On the
	// responses flavor, effort+tools is valid — that combination is
	// why the flavor exists.
	Reasoning string `yaml:"reasoning"`
	// API overrides the provider's OpenAI-dialect wire format for this
	// model: chat_completions or responses. Empty inherits the provider
	// (itself defaulting to chat_completions). openai-backed models only.
	API string `yaml:"api"`
	// MaxOutput clamps requested max_tokens to the model's output cap
	// (Claude Code asks for 32K+; many models cap lower).
	MaxOutput int    `yaml:"max_output"`
	Pricing   *Price `yaml:"pricing"`
	// ContextWindow is the model's nominal input context window in tokens.
	// Claude Code assumes ~200K; when the real window differs, the router
	// scales reported token counts so auto-compact fires at the right
	// relative fullness (see internal/tokens/scale.go). Declaring it on an
	// anthropic model is meaningful too: it anchors the model in a routing
	// rule's shared context gauge, and lets effective_context force
	// compaction before a real Claude window is full.
	ContextWindow int `yaml:"context_window"`
	// EffectiveContext caps the usable context below the nominal window —
	// an attention budget for models that degrade well before their
	// advertised limit. Unset means the full window is usable.
	EffectiveContext int `yaml:"effective_context"`
}

// ContextBudget is the number of input tokens the model can usefully hold:
// the smaller of ContextWindow and EffectiveContext, considering only set
// fields. 0 means unknown (no scaling applied).
func (m Model) ContextBudget() int {
	switch {
	case m.ContextWindow > 0 && m.EffectiveContext > 0:
		return min(m.ContextWindow, m.EffectiveContext)
	case m.EffectiveContext > 0:
		return m.EffectiveContext
	default:
		return m.ContextWindow
	}
}

type Profile struct {
	Model     string            `yaml:"model"`
	SmallFast string            `yaml:"small_fast"`
	Tiers     map[string]string `yaml:"tiers"` // opus/sonnet/haiku -> alias
	// PinTiers forces every tier fallback (small_fast, opus/sonnet/haiku,
	// and Claude Code's own subagent spawns) onto Model instead of the
	// aliases above. Off by default so a profile like `tiers: {opus: opus,
	// sonnet: sonnet, haiku: haiku}` keeps working as a deliberate per-tier
	// routing choice; turn this on when you want one model end-to-end
	// (e.g. testing a non-Anthropic model's true harness overhead, or
	// keeping a whole session pinned to a specific cost/speed tradeoff).
	// Mirrors internal/eval's evalEnv() candidate pinning.
	PinTiers    bool    `yaml:"pin_tiers"`
	Budget      *Budget `yaml:"budget"`
	Passthrough bool    `yaml:"passthrough"`
	TimeoutMS   int     `yaml:"timeout_ms"`
	// ToolSearch opts a profile out of deferred tool schemas. Deferral is
	// on by default because that is where it was measured — it took a
	// 186K-of-200K frame down to 12 schemas — but it asks the model to
	// notice a withheld-tool announcement and call ToolSearch before using
	// one. Claude models are trained on that protocol; a model that is not
	// simply never loads the withheld tools and quietly runs without them,
	// so a profile pinned to such a model can set `tool_search: false`.
	// ENABLE_TOOL_SEARCH in the launching shell overrides this either way.
	ToolSearch *bool `yaml:"tool_search"`
}

type Budget struct {
	Daily    float64 `yaml:"daily"`
	Weekly   float64 `yaml:"weekly"`
	Monthly  float64 `yaml:"monthly"`
	WarnAt   float64 `yaml:"warn_at"`
	HardStop *bool   `yaml:"hard_stop"`
}

// Price is USD per million tokens.
type Price struct {
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheRead  float64 `yaml:"cache_read" json:"cache_read"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write"`
}

// DataDir returns ~/.gremlord, creating it if needed. On the first run after
// the gremlord rename it carries the old directory's contents across.
func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, DirName)

	// A failed migration must not make the tool unusable. Warn and continue
	// with an empty directory; ~/.agentic is untouched, so a retry is possible.
	if migrated, err := migrateLegacy(home, dir); err != nil {
		fmt.Fprintf(os.Stderr, "gremlord: could not carry ~/%s forward: %v\n", LegacyDirName, err)
	} else if migrated {
		fmt.Fprintf(os.Stderr, "gremlord: carried your config and cost history over from ~/%s (originals untouched; `gremlord doctor` lists what was not copied)\n", LegacyDirName)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func Path() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load reads and validates the config file. A missing file returns
// os.ErrNotExist so callers can suggest `gremlord setup`.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(readPath(path))
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Router.Port == 0 {
		c.Router.Port = DefaultPort
	}
	for name, p := range c.Providers {
		switch p.Type {
		case ProviderAnthropic, ProviderOpenAI:
			if p.BaseURL == "" {
				return fmt.Errorf("config: provider %q missing base_url", name)
			}
			// Catch cli-only fields here instead of silently ignoring them.
			if p.Dialect != "" || p.Command != "" || p.Sandbox != "" || p.TimeoutMS != 0 {
				return fmt.Errorf("config: provider %q: dialect/command/sandbox/timeout_ms only apply to cli providers — remove them", name)
			}
			if p.Type == ProviderAnthropic && p.API != "" {
				return fmt.Errorf("config: provider %q: api only applies to openai providers — remove it", name)
			}
			if p.Type == ProviderOpenAI {
				switch p.API {
				case "", APIChatCompletions, APIResponses:
				default:
					return fmt.Errorf("config: provider %q has unknown api %q (want %q or %q)",
						name, p.API, APIChatCompletions, APIResponses)
				}
			}
		case ProviderCLI:
			switch p.Dialect {
			case CLIDialectCodex, CLIDialectGrok:
			default:
				return fmt.Errorf("config: provider %q has unknown dialect %q (want %q or %q)",
					name, p.Dialect, CLIDialectCodex, CLIDialectGrok)
			}
			// A cli provider has no HTTP upstream; these would be silently
			// ignored, which is the failure mode this switch guards against.
			if p.BaseURL != "" || p.APIKey != "" || p.APIKeyEnv != "" || p.MaxTokensParam != "" || p.MaxRequestBytes != 0 || p.API != "" {
				return fmt.Errorf("config: provider %q: base_url/api_key/api_key_env/max_tokens_param/max_request_bytes/api have no effect on cli providers — remove them", name)
			}
			if p.Sandbox != "" {
				if p.Dialect != CLIDialectCodex {
					return fmt.Errorf("config: provider %q: sandbox only applies to the %q dialect", name, CLIDialectCodex)
				}
				switch p.Sandbox {
				case "read-only", "workspace-write", "danger-full-access":
				default:
					return fmt.Errorf("config: provider %q has unknown sandbox %q (want read-only, workspace-write, or danger-full-access)", name, p.Sandbox)
				}
			}
			if p.TimeoutMS < 0 {
				return fmt.Errorf("config: provider %q has negative timeout_ms", name)
			}
		default:
			return fmt.Errorf("config: provider %q has unknown type %q (want %q, %q, or %q)",
				name, p.Type, ProviderAnthropic, ProviderOpenAI, ProviderCLI)
		}
		if p.MaxRequestBytes < 0 {
			return fmt.Errorf("config: provider %q has negative max_request_bytes", name)
		}
	}
	for alias, m := range c.Models {
		if _, ok := c.Providers[m.Provider]; !ok {
			return fmt.Errorf("config: model %q references unknown provider %q", alias, m.Provider)
		}
		if c.Providers[m.Provider].Type == ProviderCLI {
			// A delegated CLI run is a whole agent loop, not a completion —
			// none of the HTTP/completion knobs apply. ID stays optional and,
			// when set, is passed as the CLI's model-selection flag.
			if m.Reasoning != "" || m.MaxOutput != 0 || m.Pricing != nil || m.ContextWindow != 0 || m.EffectiveContext != 0 || m.API != "" {
				return fmt.Errorf("config: model %q: reasoning/max_output/pricing/context_window/effective_context/api have no effect on cli providers — remove them", alias)
			}
			continue
		}
		if m.ID == "" {
			return fmt.Errorf("config: model %q missing id", alias)
		}
		switch m.Reasoning {
		case "", "none", "effort", "passive":
		default:
			return fmt.Errorf("config: model %q has unknown reasoning %q", alias, m.Reasoning)
		}
		if m.API != "" {
			if c.Providers[m.Provider].Type != ProviderOpenAI {
				return fmt.Errorf("config: model %q: api only applies to openai providers — remove it", alias)
			}
			switch m.API {
			case APIChatCompletions, APIResponses:
			default:
				return fmt.Errorf("config: model %q has unknown api %q (want %q or %q)",
					alias, m.API, APIChatCompletions, APIResponses)
			}
		}
		if m.ContextWindow < 0 || m.EffectiveContext < 0 {
			return fmt.Errorf("config: model %q has negative context size", alias)
		}
		if m.ContextWindow > 0 && m.EffectiveContext > m.ContextWindow {
			return fmt.Errorf("config: model %q effective_context %d exceeds context_window %d",
				alias, m.EffectiveContext, m.ContextWindow)
		}
	}
	for name, prof := range c.Profiles {
		for what, alias := range map[string]string{"model": prof.Model, "small_fast": prof.SmallFast} {
			if alias != "" && c.IsCLIAlias(alias) {
				return fmt.Errorf("config: profile %q %s references cli alias %q — cli delegation is only available as an explicit subagent (gremlord agents sync), not a session model", name, what, alias)
			}
		}
		for tier, alias := range prof.Tiers {
			if c.IsCLIAlias(alias) {
				return fmt.Errorf("config: profile %q tier %q references cli alias %q — cli delegation is only available as an explicit subagent, not a tier fallback", name, tier, alias)
			}
		}
		if prof.Passthrough {
			continue
		}
		for what, alias := range map[string]string{"model": prof.Model, "small_fast": prof.SmallFast} {
			if alias == "" {
				continue
			}
			if !c.isModelRef(alias) {
				return fmt.Errorf("config: profile %q %s references unknown model alias %q", name, what, alias)
			}
		}
		for tier, alias := range prof.Tiers {
			if !c.isModelRef(alias) {
				return fmt.Errorf("config: profile %q tier %q references unknown model alias %q", name, tier, alias)
			}
		}
		if prof.PinTiers && prof.Model == "" {
			return fmt.Errorf("config: profile %q sets pin_tiers but has no model to pin to", name)
		}
	}
	if c.DefaultProfile != "" {
		if _, ok := c.Profiles[c.DefaultProfile]; !ok {
			return fmt.Errorf("config: default_profile %q not defined", c.DefaultProfile)
		}
	}
	for name, r := range c.Routing {
		if _, clash := c.Models[name]; clash {
			return fmt.Errorf("config: routing %q collides with a model alias", name)
		}
		if _, ok := c.Models[r.Classifier]; !ok {
			return fmt.Errorf("config: routing %q classifier references unknown model alias %q", name, r.Classifier)
		}
		if c.IsCLIAlias(r.Classifier) {
			return fmt.Errorf("config: routing %q classifier %q is a cli alias — classification needs a completion model, not a delegated agent", name, r.Classifier)
		}
		if len(r.Tiers) == 0 {
			return fmt.Errorf("config: routing %q has no tiers", name)
		}
		for tier, alias := range r.Tiers {
			if _, ok := c.Models[alias]; !ok {
				return fmt.Errorf("config: routing %q tier %q references unknown model alias %q", name, tier, alias)
			}
			if c.IsCLIAlias(alias) {
				return fmt.Errorf("config: routing %q tier %q references cli alias %q — auto-routing must never silently start a delegated agent run; invoke it as an explicit subagent instead", name, tier, alias)
			}
		}
		switch r.ContextGauge {
		case "", GaugeMax, GaugeMin, GaugeModel:
		default:
			return fmt.Errorf("config: routing %q has unknown context_gauge %q (want %q, %q, or %q)",
				name, r.ContextGauge, GaugeMax, GaugeMin, GaugeModel)
		}
		if r.Default != "" {
			if _, ok := r.Tiers[r.Default]; !ok {
				return fmt.Errorf("config: routing %q default %q is not a tier", name, r.Default)
			}
		}
		for label, alias := range r.Tasks {
			if !IsTaskLabel(label) {
				return fmt.Errorf("config: routing %q task %q is not a recognized label (want one of: %s)",
					name, label, strings.Join(TaskLabels, ", "))
			}
			if _, ok := c.Models[alias]; !ok {
				return fmt.Errorf("config: routing %q task %q references unknown model alias %q", name, label, alias)
			}
			if c.IsCLIAlias(alias) {
				return fmt.Errorf("config: routing %q task %q references cli alias %q — auto-routing must never silently start a delegated agent run; invoke it as an explicit subagent instead", name, label, alias)
			}
		}
	}
	return nil
}

// isModelRef reports whether name is a usable main-model reference: either a
// concrete model alias or a dynamic routing rule (e.g. "auto"). Profiles and
// tiers accept both so `model: auto` is valid config, not a validation error.
func (c *Config) isModelRef(name string) bool {
	if _, ok := c.Models[name]; ok {
		return true
	}
	_, ok := c.Routing[name]
	return ok
}

// IsCLIAlias reports whether name is a model alias backed by a cli provider —
// i.e. one that delegates a whole task to a local coding-agent CLI instead of
// answering a single completion.
func (c *Config) IsCLIAlias(name string) bool {
	m, ok := c.Models[name]
	if !ok {
		return false
	}
	return c.Providers[m.Provider].Type == ProviderCLI
}

// Resolved is a model alias resolved to its provider.
type Resolved struct {
	Alias        string
	ProviderName string
	Provider     Provider
	Model        Model
}

// APIFlavor is the OpenAI-dialect wire format for this route: the model's
// api, else the provider's, else chat_completions. Non-openai routes never
// consult it.
func (r Resolved) APIFlavor() string {
	if r.Model.API != "" {
		return r.Model.API
	}
	if r.Provider.API != "" {
		return r.Provider.API
	}
	return APIChatCompletions
}

// Resolve maps a model id from a request to a provider + upstream model.
// Resolution order: exact alias -> built-in default (claude-* passes
// through to the "anthropic" provider unchanged).
func (c *Config) Resolve(alias string) (Resolved, error) {
	if m, ok := c.Models[alias]; ok {
		return Resolved{Alias: alias, ProviderName: m.Provider, Provider: c.Providers[m.Provider], Model: m}, nil
	}
	if strings.HasPrefix(alias, "claude-") {
		if p, ok := c.Providers[ProviderAnthropic]; ok {
			return Resolved{
				Alias:        alias,
				ProviderName: ProviderAnthropic,
				Provider:     p,
				Model:        Model{Provider: ProviderAnthropic, ID: alias},
			}, nil
		}
		return Resolved{}, fmt.Errorf("model %q needs an %q provider in config", alias, ProviderAnthropic)
	}
	return Resolved{}, fmt.Errorf("unknown model alias %q", alias)
}
