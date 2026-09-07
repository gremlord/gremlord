// Package launch assembles the session environment and spawns Claude Code.
package launch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/router"
	"github.com/gremlord/gremlord/internal/store"
	"github.com/gremlord/gremlord/internal/wire"
)

type Options struct {
	Profile      string
	ModelFlag    string // one-shot main-model override (alias)
	InstanceName string // forwarded to claude as --name
	Passthrough  bool
	ClaudeArgs   []string
}

// Token returns the per-install router token, creating it on first use.
func Token(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "token")
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		return strings.TrimSpace(string(data)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func newSessionID() string {
	buf := make([]byte, 8)
	rand.Read(buf)
	return fmt.Sprintf("sess-%d-%s", time.Now().Unix(), hex.EncodeToString(buf))
}

// Run launches Claude Code for one session and blocks until it exits.
func Run(ctx context.Context, cfg *config.Config, dataDir string, opts Options, logger *slog.Logger) error {
	profName := opts.Profile
	if profName == "" {
		profName = cfg.DefaultProfile
	}
	var prof config.Profile
	if profName != "" {
		p, ok := cfg.Profiles[profName]
		if !ok {
			return fmt.Errorf("profile %q not found in ~/.gremlord/config.yaml", profName)
		}
		prof = p
	}

	env := os.Environ()
	sessionID := newSessionID()

	if prof.Passthrough || opts.Passthrough {
		fmt.Fprintf(os.Stderr, "gremlord: passthrough profile — subscription billing, cost tracking unavailable\n")
	} else {
		token, err := Token(dataDir)
		if err != nil {
			return err
		}
		mgr := &router.Manager{Port: cfg.Router.Port, Token: token, DataDir: dataDir, Log: logger}
		routerCtx, cancelRouter := context.WithCancel(context.Background())
		defer cancelRouter()
		go func() {
			if err := mgr.Run(routerCtx); err != nil {
				logger.Error("router", "err", err)
			}
		}()
		if err := mgr.Ensure(ctx); err != nil {
			return err
		}

		model := prof.Model
		if opts.ModelFlag != "" {
			if cfg.IsCLIAlias(opts.ModelFlag) {
				return fmt.Errorf("cli alias %q is only available as an explicit subagent (gremlord-%s), not a session model", opts.ModelFlag, opts.ModelFlag)
			}
			model = opts.ModelFlag
		}
		cwd, _ := os.Getwd()
		env = sessionEnv(env, mgr.BaseURL(), token, sessionID, profName, prof, model, cwd)

		recordSession(dataDir, sessionID, profName, true)
		defer func() {
			recordSession(dataDir, sessionID, profName, false)
			printSummary(dataDir, cfg, sessionID, profName)
		}()

		// Notice (non-blocking) when the per-alias subagents have drifted from
		// config. Router-backed sessions only — a passthrough profile doesn't
		// resolve gremlord aliases, so the generated agents wouldn't work there.
		noticeAgentDrift(cfg, dataDir)
	}

	child := buildChild(opts)
	cmd := exec.Command(child[0], child[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching %s: %w (is Claude Code installed?)", child[0], err)
	}
	go func() {
		for sig := range sigs {
			cmd.Process.Signal(sig)
		}
	}()
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Claude Code's own exit code (Ctrl-C etc.) — not our error.
		return nil
	}
	return err
}

// sessionEnv assembles the child process environment for a non-passthrough
// profile: router creds, model selection, and the X-Gremlord-* headers Claude
// Code carries transparently on every request so the router can attribute
// spend to this session and (when pinned) enforce the pin. cwd rides along as
// X-Gremlord-Cwd so cli-delegation backends run the peer CLI in the session's
// directory (the router leader is a shared long-lived process whose own cwd
// is meaningless). Extracted from Run so PinTiers behavior is directly
// testable without spawning a router or a claude process.
func sessionEnv(env []string, baseURL, token, sessionID, profName string, prof config.Profile, model, cwd string) []string {
	env = setEnv(env, "ANTHROPIC_BASE_URL", baseURL)
	env = setEnv(env, "ANTHROPIC_AUTH_TOKEN", token)
	env = unsetEnv(env, "ANTHROPIC_API_KEY")
	if model != "" {
		env = setEnv(env, "ANTHROPIC_MODEL", model)
	}
	// Header values must be single-line; a pathological cwd must not let one
	// header smuggle another.
	cwd = strings.NewReplacer("\n", "", "\r", "").Replace(cwd)
	cwdHeader := ""
	if cwd != "" {
		cwdHeader = "\n" + wire.HeaderCwd + ": " + cwd +
			"\n" + wire.LegacyHeaderCwd + ": " + cwd
	}
	if prof.PinTiers && model != "" {
		// Pin every tier fallback — including Claude Code's own subagent
		// spawns — to the main model, overriding small_fast/tiers config
		// entirely. Mirrors internal/eval's evalEnv() candidate pinning,
		// for interactive sessions that want one model end-to-end rather
		// than per-tier routing.
		env = setEnv(env, "ANTHROPIC_SMALL_FAST_MODEL", model)
		env = setEnv(env, "ANTHROPIC_DEFAULT_OPUS_MODEL", model)
		env = setEnv(env, "ANTHROPIC_DEFAULT_SONNET_MODEL", model)
		env = setEnv(env, "ANTHROPIC_DEFAULT_HAIKU_MODEL", model)
		env = setEnv(env, "CLAUDE_CODE_SUBAGENT_MODEL", model)
		env = setEnv(env, "ANTHROPIC_CUSTOM_HEADERS",
			sessionHeaders(sessionID, profName, model, cwdHeader))
	} else {
		if prof.SmallFast != "" {
			env = setEnv(env, "ANTHROPIC_SMALL_FAST_MODEL", prof.SmallFast)
		}
		for tier, alias := range prof.Tiers {
			env = setEnv(env, "ANTHROPIC_DEFAULT_"+strings.ToUpper(tier)+"_MODEL", alias)
		}
		env = setEnv(env, "ANTHROPIC_CUSTOM_HEADERS",
			sessionHeaders(sessionID, profName, "", cwdHeader))
	}
	// Both spellings for one release: a ~/.claude/settings.json statusLine
	// still pointing at the old agentic executable reads only AGENTIC_*.
	env = setEnv(env, config.EnvName("SESSION_ID"), sessionID)
	env = setEnv(env, config.EnvName("PROFILE"), profName)
	env = setEnv(env, "AGENTIC_SESSION_ID", sessionID)
	env = setEnv(env, "AGENTIC_PROFILE", profName)
	env = enableToolSearch(env, prof)
	if prof.TimeoutMS > 0 {
		env = setEnv(env, "API_TIMEOUT_MS", fmt.Sprint(prof.TimeoutMS))
	}
	return env
}

// autoApprovedTools is the tool allowlist every gremlord session runs with.
// It is the set `clauder wrap --slave` used to pass before gremlord spawned
// claude itself, kept identical so the launch path change is invisible.
// Allowlisting mcp__clauder__* is inert when clauder is not installed.
var autoApprovedTools = []string{
	"Read", "Write", "Edit", "Glob", "Grep", "Bash(*)",
	"WebFetch", "WebSearch", "mcp__clauder__*",
}

// buildChild spawns claude directly. Claude Code registers each session in
// ~/.claude/sessions and opens a peer socket, so cross-instance messaging is
// native and no longer needs `clauder wrap` in front of us; clauder's memory
// tools arrive over its own MCP registration regardless of how we launch.
//
// Caller args go first because --allowedTools is variadic: it consumes args
// until the next flag, so a trailing bare prompt lands after it as a tool
// name and claude exits with "Input must be provided". Repeating the flag
// per tool (rather than one comma-joined value) keeps a tool spec that
// contains a comma from being split.
func buildChild(opts Options) []string {
	args := append([]string{"claude"}, opts.ClaudeArgs...)
	if opts.InstanceName != "" {
		args = append(args, "--name", opts.InstanceName)
	}
	for _, tool := range autoApprovedTools {
		args = append(args, "--allowedTools", tool)
	}
	return args
}

func recordSession(dataDir, id, profile string, start bool) {
	st, err := store.Open(filepath.Join(dataDir, config.DBName))
	if err != nil {
		return
	}
	defer st.Close()
	wd, _ := os.Getwd()
	if start {
		st.StartSession(id, profile, wd, time.Now())
	} else {
		st.EndSession(id, time.Now())
	}
}

func printSummary(dataDir string, cfg *config.Config, sessionID, profile string) {
	st, err := store.OpenReadOnly(filepath.Join(dataDir, config.DBName))
	if err != nil {
		return
	}
	defer st.Close()
	dayStart := time.Now().Truncate(24 * time.Hour)
	sess, _ := st.TotalSince(time.Time{}, "", sessionID)
	day, _ := st.TotalSince(dayStart, "", "")
	line := fmt.Sprintf("gremlord: session cost $%.2f (profile: %s) — today $%.2f", sess, profile, day)
	if cfg.Budgets != nil && cfg.Budgets.Daily > 0 {
		line += fmt.Sprintf(" / $%.2f daily budget", cfg.Budgets.Daily)
	}
	fmt.Fprintln(os.Stderr, line)
}

// enableToolSearch asks Claude Code to defer tool schemas: send the deferred
// tools' names up front and the full schema only once the model pulls one in
// with ToolSearch. The client turns that off on its own when
// ANTHROPIC_BASE_URL is not a first-party Anthropic host — which the router
// never is — and then every builtin and MCP schema rides on every request:
// on a session with 165 MCP tools that measured 186K of tool schemas inside
// a 200K frame, over the limit before the first user turn.
//
// The deferral is the client's own work, not the API's: it answers its own
// ToolSearch call with tool_reference blocks and adds the bound tool's real
// schema to the next request's tools array. So the router only has to carry
// those blocks through, which passthrough does natively and translation
// renders as text (see anthropic.ContentBlock.FlatText).
//
// Precedence, most immediate first: an explicit value in the caller's
// environment (so a shell can say "false" for the old eager behavior, or
// "auto:N" to sample it), then the profile's tool_search, then on. A
// profile that opts out gets an explicit "false" rather than an unset
// variable, so the choice does not depend on how the client's own gate
// happens to treat an unrecognized host.
func enableToolSearch(env []string, prof config.Profile) []string {
	if hasEnv(env, "ENABLE_TOOL_SEARCH") {
		return env
	}
	if prof.ToolSearch != nil && !*prof.ToolSearch {
		return setEnv(env, "ENABLE_TOOL_SEARCH", "false")
	}
	return setEnv(env, "ENABLE_TOOL_SEARCH", "true")
}

func setEnv(env []string, key, value string) []string {
	return append(unsetEnv(env, key), key+"="+value)
}

func hasEnv(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func unsetEnv(env []string, key string) []string {
	out := env[:0]
	prefix := key + "="
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}

// sessionHeaders builds the ANTHROPIC_CUSTOM_HEADERS value carrying session
// identity to the router. Both header spellings go out for one release: the
// router is whichever binary won the port, so a new launcher may be talking to
// an agentic router that only reads X-Agentic-*. Dropping the old spelling
// there would lose spend attribution silently.
func sessionHeaders(sessionID, profName, pinModel, cwdHeader string) string {
	pairs := [][2]string{
		{wire.HeaderSession, sessionID},
		{wire.LegacyHeaderSession, sessionID},
		{wire.HeaderProfile, profName},
		{wire.LegacyHeaderProfile, profName},
	}
	if pinModel != "" {
		pairs = append(pairs,
			[2]string{wire.HeaderPinModel, pinModel},
			[2]string{wire.LegacyHeaderPinModel, pinModel})
	}
	lines := make([]string, 0, len(pairs))
	for _, p := range pairs {
		lines = append(lines, p[0]+": "+p[1])
	}
	return strings.Join(lines, "\n") + cwdHeader
}
