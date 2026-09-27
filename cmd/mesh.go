package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gremlord/gremlord-mesh/leader"
	"github.com/gremlord/gremlord-mesh/meshcli"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/router"
)

// meshDeps wires the gremlord mesh commands to gremlord's data dir. Sessions
// run `gremlord mesh-mcp` / `gremlord mesh-hook`, so the binary is the
// gremlord on PATH when it is this executable, else this executable's path.
func meshDeps() *meshcli.Deps {
	return &meshcli.Deps{DataDir: config.DataDir, Binary: meshBinary(), Version: router.Version}
}

func meshBinary() string {
	self, err := os.Executable()
	if err != nil {
		return "gremlord"
	}
	self, _ = filepath.EvalSymlinks(self)
	if onPath, err := exec.LookPath("gremlord"); err == nil {
		if resolved, err := filepath.EvalSymlinks(onPath); err == nil && resolved == self {
			return "gremlord"
		}
	}
	return self
}

// installMesh registers the mesh MCP server and hooks during `gremlord
// setup` when mesh is enabled.
func installMesh(cfg *config.Config) {
	if cfg == nil || cfg.Mesh == nil || !cfg.Mesh.Enabled {
		return
	}
	err := meshcli.Install(meshBinary(), func(s string) { fmt.Println("✓ mesh " + s) })
	if err != nil {
		fmt.Printf("⚠ could not install mesh integration: %v\n", err)
	}
}

// meshStatus is the statusline's mesh segment: hub state plus this
// session's unread count and open channels. Empty when mesh is off or the
// leader is not reachable within a short timeout.
func meshStatus(dataDir, gremlordSession string) string {
	lc := leader.NewLocalClient(leader.SocketPath(dataDir), leader.ReadToken(dataDir), 300*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var st leader.Status
	if err := lc.Call(ctx, "/v1/status", map[string]any{}, &st); err != nil {
		return ""
	}
	seg := "mesh \033[32m●\033[0m"
	if !st.Connected {
		seg = "mesh \033[31m○\033[0m"
		if st.Queued > 0 {
			seg += fmt.Sprintf(" %d queued", st.Queued)
		}
	}
	for _, s := range st.Sessions {
		if s.Gremlord != gremlordSession {
			continue
		}
		if s.Unread > 0 {
			seg += fmt.Sprintf(" \033[33m%d unread\033[0m", s.Unread)
		}
		if s.Channels > 0 {
			seg += fmt.Sprintf(" %d ch", s.Channels)
		}
	}
	return seg
}

func init() {
	rootCmd.AddCommand(meshcli.Commands(meshDeps())...)
}
