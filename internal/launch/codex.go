package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/wire"
)

func validateCodex(cfg *config.Config, prof config.Profile, opts Options, model string) (config.Resolved, error) {
	var empty config.Resolved
	if opts.Passthrough || prof.Passthrough {
		return empty, fmt.Errorf("Codex PoC requires a routed profile; passthrough would disable metering")
	}
	if _, ok := cfg.Routing[model]; ok {
		return empty, fmt.Errorf("Codex PoC requires --model <fixed GPT alias>; automatic routing is not supported")
	}
	route, err := cfg.Resolve(model)
	if err != nil {
		return empty, err
	}
	if route.Provider.Type != config.ProviderOpenAI || route.APIFlavor() != config.APIResponses || !strings.HasPrefix(route.Model.ID, "gpt-") {
		return empty, fmt.Errorf("Codex PoC requires a GPT alias configured with api: responses")
	}
	// Avoid accidentally replacing the gateway with native auth or a different
	// provider. Other native flags, including permissions and -C, remain native.
	for i, arg := range opts.ClaudeArgs {
		for _, flag := range []string{"--model", "-m", "--profile", "-p", "--oss", "--local-provider", "--remote"} {
			if arg == flag || strings.HasPrefix(arg, flag+"=") || ((flag == "-m" || flag == "-p") && strings.HasPrefix(arg, flag) && len(arg) > 2) {
				return empty, fmt.Errorf("use Gremlord's --model/--profile before --; Codex %s is unsupported in the PoC", flag)
			}
		}
		value := ""
		if (arg == "-c" || arg == "--config") && i+1 < len(opts.ClaudeArgs) {
			value = opts.ClaudeArgs[i+1]
		}
		if strings.HasPrefix(arg, "--config=") {
			value = strings.TrimPrefix(arg, "--config=")
		}
		if strings.HasPrefix(arg, "-c") && len(arg) > 2 {
			value = strings.TrimPrefix(arg[2:], "=")
		}
		key, _, _ := strings.Cut(value, "=")
		key = strings.TrimSpace(key)
		if key == "model" || key == "model_provider" || strings.HasPrefix(key, "model_providers") || key == "openai_base_url" || key == "profile" || key == "features.request_compression" || strings.HasPrefix(key, "features.responses_websockets") {
			return empty, fmt.Errorf("Codex config %q is owned by Gremlord in the PoC", key)
		}
	}
	return route, nil
}

func codexChild(opts Options, route config.Resolved, env []string, baseURL, token, session, profile string) ([]string, []string) {
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	// The real model ID lets Codex select its native model instructions and
	// tool protocol. The router resolves the alias from our pin header.
	overrides := []string{
		"model=" + quote(route.Model.ID),
		`model_provider="gremlord"`,
		// Codex uses this identity for OpenAI-specific protocol capabilities.
		`model_providers.gremlord.name="OpenAI"`,
		"model_providers.gremlord.base_url=" + quote(strings.TrimRight(baseURL, "/")+"/v1"),
		`model_providers.gremlord.env_key="GREMLORD_CODEX_TOKEN"`,
		`model_providers.gremlord.requires_openai_auth=false`,
		`model_providers.gremlord.wire_api="responses"`,
		`model_providers.gremlord.supports_websockets=false`,
		`model_providers.gremlord.supports_standalone_web_search=false`,
		`model_providers.gremlord.request_max_retries=1`,
		`model_providers.gremlord.stream_max_retries=1`,
		`features.responses_websockets=false`,
		`features.responses_websockets_v2=false`,
		`features.request_compression=false`,
		`model_providers.gremlord.http_headers={` + quote(wire.HeaderSession) + "=" + quote(session) + "," + quote(wire.HeaderProfile) + "=" + quote(profile) + "," + quote(wire.HeaderPinModel) + "=" + quote(route.Alias) + "," + quote(wire.HeaderHarness) + `="codex"}`,
	}
	if effort := route.Model.ReasoningEffort; effort != "" {
		overrides = append(overrides, "model_reasoning_effort="+quote(effort))
	}
	if budget := route.Model.ContextBudget(); budget > 0 {
		overrides = append(overrides, fmt.Sprintf("model_context_window=%d", budget), fmt.Sprintf("model_auto_compact_token_limit=%d", budget*9/10))
	}
	// Keep overrides in the selected command's scope. Some Codex versions
	// discard root -c values when exec/resume has its own config overrides.
	// The benchmark's --ignore-user-config exposed this as a default-provider
	// fallback, so exercise the real CLI against a fake gateway as well.
	child := []string{"codex"}
	args := opts.ClaudeArgs
	prefix := codexCommandPrefix(args)
	child = append(child, args[:prefix]...)
	args = args[prefix:]
	for _, override := range overrides {
		child = append(child, "-c", override)
	}
	if opts.InstanceName != "" {
		child = append(child, "--name", opts.InstanceName)
	}
	child = append(child, args...)
	env = setEnv(env, "GREMLORD_CODEX_TOKEN", token)
	env = setEnv(env, config.EnvName("SESSION_ID"), session)
	env = setEnv(env, config.EnvName("PROFILE"), profile)
	// Keep the user's Codex home, approvals, sandbox, rules and tools. No
	// Anthropic environment or Claude tool allowlist is injected here.
	return child, env
}

// Locate the command after optional root flags (e.g. -C dir exec resume).
// Stop at a prompt; words in a prompt or option value are not subcommands.
func codexCommandPrefix(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return 0
		}
		if strings.HasPrefix(arg, "-") {
			switch arg {
			case "-c", "--config", "-C", "--cd", "-s", "--sandbox", "-a", "--ask-for-approval", "-i", "--image", "--add-dir", "--enable", "--disable", "--name":
				i++
			}
			continue
		}
		switch arg {
		case "exec", "e", "resume", "review", "app-server":
			end := i + 1
			if (arg == "exec" || arg == "e") && end < len(args) && (args[end] == "resume" || args[end] == "review") {
				end++
			}
			return end
		default:
			return 0
		}
	}
	return 0
}

func requireNativeResponses(ctx context.Context, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+wire.PathHealth, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("checking native Responses support: %w", err)
	}
	defer resp.Body.Close()
	var health struct {
		Native bool `json:"native_responses"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&health) != nil || !health.Native {
		return fmt.Errorf("running router lacks native Responses support; restart it with this PoC build before launching Codex")
	}
	return nil
}
