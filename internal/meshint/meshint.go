// Package meshint wires Gremlord Mesh into gremlord: it starts the mesh
// leader inside whichever process holds the router port, gives it the
// router's classifier call, and spawns headless runs through the router so
// budgets and spend tracking apply to them like any other session.
package meshint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	meshleader "github.com/gremlord/gremlord-mesh/leader"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/wire"
)

// Completer runs a one-shot prompt on a model alias through the router's
// own backends (router.Server.Complete).
type Completer func(ctx context.Context, alias, prompt string, maxTokens int) (string, error)

// Start runs the mesh leader for this router leader when mesh is enabled
// and the host has joined. A disabled or unjoined mesh returns (nil, nil):
// the router works exactly as before.
func Start(ctx context.Context, cfg *config.Config, dataDir, token, baseURL, version string, complete Completer, log *slog.Logger) (*meshleader.Leader, error) {
	if cfg.Mesh == nil || !cfg.Mesh.Enabled {
		return nil, nil
	}
	if _, err := meshleader.LoadCred(dataDir); err != nil {
		log.Warn("mesh enabled but this host has not joined; run `gremlord mesh join <hub-url> <token>`")
		return nil, nil
	}
	mc := *cfg.Mesh
	opt := meshleader.Options{
		DataDir: dataDir, Token: token, Config: mc, Log: log, Version: version,
		Spawner: spawner(cfg, dataDir, token, baseURL, log),
	}
	if mc.Classifier != "" && complete != nil {
		alias := mc.Classifier
		opt.Complete = func(ctx context.Context, prompt string, maxTokens int) (string, error) {
			return complete(ctx, alias, prompt, maxTokens)
		}
	}
	l, err := meshleader.Start(ctx, opt)
	if errors.Is(err, meshleader.ErrNotLeader) {
		log.Warn("mesh leader lock held by another process on this host")
		return nil, nil
	}
	return l, err
}

// spawner starts `claude -p` pointed at the router, attributed to its own
// gremlord session, with only the configured tools and a per-run budget.
func spawner(cfg *config.Config, dataDir, token, baseURL string, log *slog.Logger) meshleader.Spawner {
	return func(ctx context.Context, req meshleader.SpawnRequest) error {
		prof := req.Profile
		if prof == "" {
			prof = cfg.DefaultProfile
		}
		p, ok := cfg.Profiles[prof]
		if !ok {
			return fmt.Errorf("spawn profile %q not found", prof)
		}
		if p.Passthrough {
			return fmt.Errorf("spawn profile %q is passthrough; spawned runs must go through the router", prof)
		}
		buf := make([]byte, 8)
		rand.Read(buf)
		sessionID := fmt.Sprintf("sess-mesh-%d-%s", time.Now().Unix(), hex.EncodeToString(buf))
		env := os.Environ()
		env = setEnv(env, "ANTHROPIC_BASE_URL", baseURL)
		env = setEnv(env, "ANTHROPIC_AUTH_TOKEN", token)
		env = unsetEnv(env, "ANTHROPIC_API_KEY")
		if p.Model != "" {
			env = setEnv(env, "ANTHROPIC_MODEL", p.Model)
		}
		if p.SmallFast != "" {
			env = setEnv(env, "ANTHROPIC_SMALL_FAST_MODEL", p.SmallFast)
		}
		headers := []string{
			wire.HeaderSession + ": " + sessionID,
			wire.HeaderProfile + ": " + prof,
			wire.HeaderCwd + ": " + strings.NewReplacer("\n", "", "\r", "").Replace(req.Dir),
		}
		env = setEnv(env, "ANTHROPIC_CUSTOM_HEADERS", strings.Join(headers, "\n"))
		env = setEnv(env, config.EnvName("SESSION_ID"), sessionID)
		env = setEnv(env, config.EnvName("PROFILE"), prof)
		env = setEnv(env, "GREMLORD_MESH_SPAWN", req.Ref)

		args := []string{"-p", req.Prompt, "--max-budget-usd", fmt.Sprintf("%.2f", req.BudgetUSD)}
		for _, t := range req.Tools {
			args = append(args, "--allowedTools", t)
		}
		cmd := exec.Command("claude", args...)
		cmd.Dir, cmd.Env = req.Dir, env
		if f, err := os.Create(filepath.Join(req.Dir, req.Ref+".log")); err == nil {
			cmd.Stdout, cmd.Stderr = f, f
			defer func() { go func() { cmd.Wait(); f.Close() }() }()
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("starting claude: %w", err)
		}
		log.Info("mesh spawn", "ref", req.Ref, "session", sessionID, "profile", prof, "dir", req.Dir)
		return nil
	}
}

func setEnv(env []string, key, value string) []string {
	return append(unsetEnv(env, key), key+"="+value)
}

func unsetEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}
