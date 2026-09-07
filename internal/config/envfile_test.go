package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyResolutionOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".gremlord")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "env"), []byte(
		"# comment\nTEST_AGENTIC_KEY=from-file\nQUOTED_KEY=\"quoted-value\"\n"), 0o600)

	// Reset the package cache so the temp HOME is picked up.
	envFile.mu.Lock()
	envFile.path, envFile.values = "", nil
	envFile.mu.Unlock()

	// Literal wins over everything.
	p := Provider{APIKey: "literal", APIKeyEnv: "TEST_AGENTIC_KEY"}
	if p.Key() != "literal" {
		t.Errorf("literal: %q", p.Key())
	}

	// Env file used when process env is unset.
	p = Provider{APIKeyEnv: "TEST_AGENTIC_KEY"}
	if p.Key() != "from-file" {
		t.Errorf("env file fallback: %q", p.Key())
	}
	p = Provider{APIKeyEnv: "QUOTED_KEY"}
	if p.Key() != "quoted-value" {
		t.Errorf("quoted value: %q", p.Key())
	}

	// Process env wins over the file.
	t.Setenv("TEST_AGENTIC_KEY", "from-env")
	p = Provider{APIKeyEnv: "TEST_AGENTIC_KEY"}
	if p.Key() != "from-env" {
		t.Errorf("process env precedence: %q", p.Key())
	}

	// No key anywhere.
	p = Provider{APIKeyEnv: "MISSING_KEY_XYZ"}
	if p.Key() != "" {
		t.Errorf("missing: %q", p.Key())
	}
}
