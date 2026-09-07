package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readStatusline(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s.StatusLine.Command
}

func writeSettings(t *testing.T, home string, body string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The rename must not be a dead end: setup declined to touch an existing
// statusline and doctor reported it unregistered, so a migrating user was
// stuck unless they edited settings.json by hand.
func TestRegisterStatuslineUpgradesPreRenameEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSettings(t, home, `{"statusLine":{"type":"command","command":"agentic statusline"},"theme":"dark"}`)

	if err := registerStatusline(); err != nil {
		t.Fatal(err)
	}
	if got := readStatusline(t, home); got != statuslineCommand {
		t.Errorf("statusline = %q, want %q", got, statuslineCommand)
	}
	// Unrelated settings must survive the read-merge-write.
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var all map[string]any
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	if all["theme"] != "dark" {
		t.Errorf("theme = %v, want dark; other settings were dropped", all["theme"])
	}
}

// A statusline the user chose themselves is still off limits.
func TestRegisterStatuslineLeavesCustomAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSettings(t, home, `{"statusLine":{"type":"command","command":"my-own-thing --fancy"}}`)

	if err := registerStatusline(); err != nil {
		t.Fatal(err)
	}
	if got := readStatusline(t, home); got != "my-own-thing --fancy" {
		t.Errorf("statusline = %q, want it untouched", got)
	}
}

func TestRegisterStatuslineWritesWhenAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := registerStatusline(); err != nil {
		t.Fatal(err)
	}
	if got := readStatusline(t, home); got != statuslineCommand {
		t.Errorf("statusline = %q, want %q", got, statuslineCommand)
	}
}
