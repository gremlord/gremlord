package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A v0.1.x agentic install must come across intact, and the original must be
// left exactly as it was so the old binary still works.
func TestMigrateLegacyCarriesConfigAndHistory(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, LegacyDirName)
	if err := os.MkdirAll(filepath.Join(legacy, "evals", "run-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("config.yaml", "version: 1\n", 0o600)
	write("env", "ANTHROPIC_API_KEY=secret\n", 0o600)
	write("token", "tok\n", 0o600)
	write(DBName, "sqlite-bytes", 0o644)
	write(DBName+"-wal", "wal-bytes", 0o644)
	write("router.log", "noise\n", 0o644)
	write("router.json", `{"port":41100}`, 0o600)
	if err := os.WriteFile(filepath.Join(legacy, "evals", "run-1", "artifact"), []byte("big"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(home, DirName)
	migrated, err := migrateLegacy(home, dir)
	if err != nil || !migrated {
		t.Fatalf("migrateLegacy() = %v, %v; want true, nil", migrated, err)
	}

	for _, name := range []string{"config.yaml", "env", "token", DBName, DBName + "-wal"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not carried over: %v", name, err)
		}
	}
	// A stale leader discovery file would point the new binary at a dead port,
	// and the log and eval artifacts are large and regenerable.
	for _, name := range []string{"router.json", "router.log", "evals"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should not have been carried over", name)
		}
	}
	// Key material must not be widened by the copy.
	info, err := os.Stat(filepath.Join(dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("env mode = %v, want 0600", info.Mode().Perm())
	}
	// The original install stays usable.
	if b, err := os.ReadFile(filepath.Join(legacy, "config.yaml")); err != nil || string(b) != "version: 1\n" {
		t.Errorf("legacy config.yaml altered: %q, %v", b, err)
	}
}

// Running again must be a no-op rather than overwriting live state with a
// stale copy of the pre-rename directory.
func TestMigrateLegacyIsOnceOnly(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, LegacyDirName)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "config.yaml"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, DirName)
	if _, err := migrateLegacy(home, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	migrated, err := migrateLegacy(home, dir)
	if err != nil || migrated {
		t.Fatalf("second migrateLegacy() = %v, %v; want false, nil", migrated, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.yaml")); string(b) != "new\n" {
		t.Errorf("live config was overwritten: %q", b)
	}
}

func TestMigrateLegacyNoopWithoutLegacyDir(t *testing.T) {
	home := t.TempDir()
	migrated, err := migrateLegacy(home, filepath.Join(home, DirName))
	if err != nil || migrated {
		t.Fatalf("migrateLegacy() = %v, %v; want false, nil", migrated, err)
	}
}

// If the copy could not happen, reads still have to find an existing install
// rather than silently falling back to defaults.
func TestReadPathFallsBackToLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, LegacyDirName)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "config.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(legacy, "config.yaml")
	if got := readPath(filepath.Join(home, DirName, "config.yaml")); got != want {
		t.Errorf("readPath() = %q, want %q", got, want)
	}

	// Once the new file exists it wins, so the tool converges on the new dir.
	newPath := filepath.Join(home, DirName, "config.yaml")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readPath(newPath); got != newPath {
		t.Errorf("readPath() = %q, want %q", got, newPath)
	}
}

func TestGetenvPrefersCurrentPrefix(t *testing.T) {
	t.Setenv("AGENTIC_SESSION_ID", "old")
	if got := Getenv("SESSION_ID"); got != "old" {
		t.Errorf("Getenv with only legacy set = %q, want %q", got, "old")
	}
	t.Setenv("GREMLORD_SESSION_ID", "new")
	if got := Getenv("SESSION_ID"); got != "new" {
		t.Errorf("Getenv with both set = %q, want %q", got, "new")
	}
}
