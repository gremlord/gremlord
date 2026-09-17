//go:build unix

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/store"
)

// Each workflow gets its own real launcher/router, isolated home and database.
// Its upstream is the shared benchmark meter, whose token is loopback-only.
// The upstream API key never enters the candidate's environment or files.
func (e *executor) prepareCodexGremlord(home, session string) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	price, _ := e.proxy.prices.Get(e.proxy.route.Model.ID)
	model := e.proxy.route.Model
	model.Provider, model.API, model.MaxOutput, model.Pricing = "meter", config.APIResponses, 32768, &price
	zero := 0
	cfg := config.Config{
		Version: 1, DefaultProfile: "benchmark", Router: config.Router{Port: port}, SplashSeconds: &zero,
		Providers: map[string]config.Provider{"meter": {Type: config.ProviderOpenAI, API: config.APIResponses, BaseURL: e.proxy.url + "/upstream/" + session + "/v1", APIKey: e.proxy.token}},
		Models:    map[string]config.Model{"benchmark": model},
		Profiles:  map[string]config.Profile{"benchmark": {Model: "benchmark", Budget: &config.Budget{Daily: 10}}},
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".gremlord")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0600)
}

func auditCodexGremlord(home, artifactDir string) error {
	st, err := store.OpenReadOnly(filepath.Join(home, ".gremlord", config.DBName))
	if err != nil {
		return err
	}
	defer st.Close()
	session, err := st.LatestSessionID()
	if err != nil {
		return err
	}
	rows, err := st.SessionUsage(session)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("Codex Gremlord PoC recorded no usage")
	}
	return writeJSON(filepath.Join(artifactDir, "gremlord-usage.json"), rows)
}
