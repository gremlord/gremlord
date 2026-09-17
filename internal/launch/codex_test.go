package launch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/config"
)

func TestCodexChildNativeModelAndGateway(t *testing.T) {
	route := config.Resolved{Alias: "astra", Model: config.Model{ID: "gpt-6-astra", ContextWindow: 1000000, EffectiveContext: 600000, ReasoningEffort: "high"}}
	args, env := codexChild(Options{ClaudeArgs: []string{"exec", "resume", "thread-1", "prompt"}}, route, []string{"CODEX_HOME=/custom", "OTHER=keep"}, "http://127.0.0.1:1234", "secret-token", "session", "profile")
	joined := strings.Join(args, "\n")
	for _, want := range []string{`model="gpt-6-astra"`, `model_provider="gremlord"`, `model_providers.gremlord.name="OpenAI"`, `model_context_window=600000`, `model_auto_compact_token_limit=540000`, `model_reasoning_effort="high"`, `"X-Gremlord-Pin-Model"="astra"`, `"X-Gremlord-Session"="session"`, `"X-Gremlord-Profile"="profile"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"secret-token", "--allowedTools", "dangerously-bypass", "sandbox_mode", "approval_policy"} {
		if strings.Contains(joined, bad) {
			t.Errorf("unexpected %s", bad)
		}
	}
	if !slices.Equal(args[:3], []string{"codex", "exec", "resume"}) || !slices.Equal(args[len(args)-2:], []string{"thread-1", "prompt"}) {
		t.Fatalf("args=%v", args)
	}
	if envVal(env, "GREMLORD_CODEX_TOKEN") != "secret-token" || envVal(env, "CODEX_HOME") != "/custom" || envVal(env, "ANTHROPIC_BASE_URL") != "" {
		t.Fatalf("env=%v", env)
	}
}

func TestValidateCodex(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.Provider{"openai": {Type: config.ProviderOpenAI, API: config.APIResponses}}, Models: map[string]config.Model{"astra": {Provider: "openai", ID: "gpt-6-astra"}}, Routing: map[string]config.RouteRule{"auto": {}}}
	if _, err := validateCodex(cfg, config.Profile{}, Options{}, "astra"); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []Options{
		{Passthrough: true}, {ClaudeArgs: []string{"--model", "other"}}, {ClaudeArgs: []string{"-mother"}},
		{ClaudeArgs: []string{"exec", "-c", `model_provider="openai"`}}, {ClaudeArgs: []string{`--config=model_providers.gremlord.base_url="https://other"`}},
		{ClaudeArgs: []string{"--profile=other"}}, {ClaudeArgs: []string{"--oss"}},
	} {
		if _, err := validateCodex(cfg, config.Profile{}, opts, "astra"); err == nil {
			t.Errorf("accepted %v", opts)
		}
	}
	for _, model := range []string{"", "missing", "auto"} {
		if _, err := validateCodex(cfg, config.Profile{}, Options{}, model); err == nil {
			t.Errorf("accepted %q", model)
		}
	}
}

func TestCodexCommandScope(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		prefix int
	}{
		{[]string{"exec", "prompt"}, 1},
		{[]string{"-C", "repo", "exec", "resume", "thread", "prompt"}, 4},
		{[]string{"--config", `personality="friendly"`, "app-server"}, 3},
		{[]string{"-C", "exec", "prompt"}, 0},
		{[]string{"--", "exec"}, 0},
		{[]string{"resume", "--last"}, 1},
	} {
		if got := codexCommandPrefix(tc.args); got != tc.prefix {
			t.Errorf("args=%v prefix=%d want %d", tc.args, got, tc.prefix)
		}
	}
}

func TestRequireNativeResponsesDetectsOldRouter(t *testing.T) {
	for _, supported := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"ok":true,"native_responses":%t}`, supported)
		}))
		err := requireNativeResponses(context.Background(), srv.URL)
		srv.Close()
		if (err == nil) != supported {
			t.Errorf("supported=%t err=%v", supported, err)
		}
	}
}
