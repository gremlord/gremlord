package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"

	"github.com/gremlord/gremlord/internal/wire"
)

// Version is stamped at build time via -ldflags.
var Version = "dev"

// Discovery is written to ~/.gremlord/router.json by the current leader.
type Discovery struct {
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	Version   string `json:"version"`
	StartedAt string `json:"started_at"`
}

// Manager keeps this process joined to the router: leader when it holds
// the port, follower otherwise. Binding 127.0.0.1:<port> IS the election.
type Manager struct {
	Port    int
	Token   string
	DataDir string
	Log     *slog.Logger

	leading bool
}

func (m *Manager) BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", m.Port) }

// Run maintains leadership until ctx is canceled. It first tries to become
// leader; if the port is held by a healthy gremlord router it follows and
// watches, racing to re-bind whenever the leader disappears.
func (m *Manager) Run(ctx context.Context) error {
	for {
		ln, err := m.tryBind()
		switch {
		case err == nil:
			if err := m.lead(ctx, ln); err != nil {
				return err
			}
			// lead only returns when ctx is done.
			return nil
		case errors.Is(err, syscall.EADDRINUSE):
			if !m.healthy(ctx) {
				// Port held by a foreign process, or a wedged leader.
				m.Log.Warn("router port busy but not healthy; retrying", "port", m.Port)
			}
		default:
			return fmt.Errorf("router: cannot bind 127.0.0.1:%d: %w (set router.port in ~/.gremlord/config.yaml)", m.Port, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// Ensure blocks until a healthy router is reachable (this process or
// another), suitable for calling before launching claude.
func (m *Manager) Ensure(ctx context.Context) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.healthy(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("router did not become healthy on %s", m.BaseURL())
}

func (m *Manager) tryBind() (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", m.Port))
}

func (m *Manager) healthy(ctx context.Context) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Probe both spellings: the port may be held by a pre-rename agentic
	// router, which serves only the legacy path. Reading a healthy peer as a
	// foreign process would make this instance refuse to start.
	for _, path := range []string{wire.PathHealth, wire.LegacyPathHealth} {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, m.BaseURL()+path, nil)
		if err != nil {
			continue
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true
		}
	}
	return false
}

func (m *Manager) lead(ctx context.Context, ln net.Listener) error {
	m.leading = true
	m.Log.Info("became router leader", "port", m.Port, "pid", os.Getpid())

	cfg, err := config.Load()
	if err != nil {
		ln.Close()
		return err
	}
	st, err := store.Open(filepath.Join(m.DataDir, config.DBName))
	if err != nil {
		ln.Close()
		return err
	}
	defer st.Close()

	srv := NewServer(cfg, m.Token, m.DataDir, st, m.Log)
	httpSrv := &http.Server{Handler: srv.Handler()}

	// Hot-reload on direct edits to ~/.gremlord/config.yaml.
	go watchConfig(ctx, srv, m.Log)

	m.writeDiscovery()
	defer m.removeDiscovery()

	done := make(chan error, 1)
	go func() { done <- httpSrv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutCtx)
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (m *Manager) discoveryPath() string { return filepath.Join(m.DataDir, "router.json") }

func (m *Manager) writeDiscovery() {
	d := Discovery{PID: os.Getpid(), Port: m.Port, Version: Version, StartedAt: time.Now().Format(time.RFC3339)}
	data, _ := json.MarshalIndent(d, "", "  ")
	os.WriteFile(m.discoveryPath(), data, 0o600)
}

func (m *Manager) removeDiscovery() {
	// Only remove our own record — a new leader may have already replaced it.
	data, err := os.ReadFile(m.discoveryPath())
	if err != nil {
		return
	}
	var d Discovery
	if json.Unmarshal(data, &d) == nil && d.PID == os.Getpid() {
		os.Remove(m.discoveryPath())
	}
}

// ReadDiscovery returns the current leader record, if any.
func ReadDiscovery(dataDir string) (*Discovery, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "router.json"))
	if err != nil {
		return nil, err
	}
	var d Discovery
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}
