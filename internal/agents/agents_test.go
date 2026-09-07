package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/config"
)

func testCfg(aliases ...string) *config.Config {
	models := map[string]config.Model{}
	for _, a := range aliases {
		models[a] = config.Model{Provider: "p", ID: a + "-upstream"}
	}
	return &config.Config{
		Providers: map[string]config.Provider{"p": {Type: config.ProviderOpenAI, BaseURL: "http://x"}},
		Models:    models,
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"qwen":        "qwen",
		"gpt-5.6-sol": "gpt-5-6-sol", // dots are not safe in a name
		"glm52":       "glm52",
		"GPT_4o":      "gpt-4o",
		"a..b":        "a-b", // separator runs collapse
		"-lead-":      "lead",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// The whole feature rests on the frontmatter carrying the alias verbatim —
// that string is what reaches the router.
func TestDesiredEmitsAliasAsModel(t *testing.T) {
	got := Desired(testCfg("gpt-5.6-sol"))
	if len(got) != 1 {
		t.Fatalf("got %d definitions, want 1", len(got))
	}
	d := got[0]
	if d.Name != "gremlord-gpt-5-6-sol" || d.Filename != "gremlord-gpt-5-6-sol.md" {
		t.Errorf("name=%q file=%q", d.Name, d.Filename)
	}
	if !strings.Contains(d.Body, "\nmodel: gpt-5.6-sol\n") {
		t.Errorf("body must carry the raw alias in model frontmatter:\n%s", d.Body)
	}
	if !strings.Contains(d.Body, "name: gremlord-gpt-5-6-sol") {
		t.Errorf("body must declare the slugged name:\n%s", d.Body)
	}
}

func TestDesiredDescribesCLIDelegation(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"codex-cli": {Type: config.ProviderCLI, Dialect: config.CLIDialectCodex},
		},
		Models: map[string]config.Model{
			"codex": {Provider: "codex-cli"},
		},
	}
	got := Desired(cfg)
	if len(got) != 1 {
		t.Fatalf("got %d definitions, want 1", len(got))
	}
	body := got[0].Body
	for _, want := range []string{
		"model: codex",
		"Delegates the ENTIRE task",
		"your own ChatGPT subscription",
		"possible file modifications",
		"It sees NONE of the conversation",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cli definition missing %q:\n%s", want, body)
		}
	}
}

// Routing rules are classifier rules, not concrete models — a subagent that
// silently re-tiers itself would make the chosen model unpredictable.
func TestDesiredExcludesRoutingRules(t *testing.T) {
	cfg := testCfg("opus", "qwen")
	cfg.Routing = map[string]config.RouteRule{
		"auto": {Classifier: "qwen", Tiers: map[string]string{"deep": "opus"}},
	}
	for _, d := range Desired(cfg) {
		if d.Alias == "auto" {
			t.Error("routing rule 'auto' must not get a subagent")
		}
	}
	if len(Desired(cfg)) != 2 {
		t.Errorf("want 2 definitions (opus, qwen), got %d", len(Desired(cfg)))
	}
}

func TestDiffAndSyncRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg("qwen", "opus")

	// Everything is new.
	changes, err := Diff(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("want 2 creates, got %+v", changes)
	}

	if _, err := Sync(cfg, dir); err != nil {
		t.Fatal(err)
	}
	// Now in sync: no changes, and Sync is a no-op.
	if changes, _ := Diff(cfg, dir); len(changes) != 0 {
		t.Errorf("after sync, want no changes, got %+v", changes)
	}
	if applied, _ := Sync(cfg, dir); len(applied) != 0 {
		t.Errorf("second sync should be a no-op, got %+v", applied)
	}

	// Dropping an alias makes its file stale.
	smaller := testCfg("qwen")
	changes, _ = Diff(smaller, dir)
	if len(changes) != 1 || changes[0].Kind != "remove" || changes[0].Name != "gremlord-opus" {
		t.Fatalf("want remove of gremlord-opus, got %+v", changes)
	}
	if _, err := Sync(smaller, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gremlord-opus.md")); !os.IsNotExist(err) {
		t.Error("stale gremlord-opus.md should have been removed")
	}
}

// The safety property that matters most: a user's own agents are never read,
// written, or deleted — only the gremlord- prefix is owned.
func TestSyncNeverTouchesForeignFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "my-reviewer.md")
	const body = "---\nname: my-reviewer\nmodel: opus\n---\nmine, hands off\n"
	if err := os.WriteFile(foreign, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Sync(testCfg("qwen"), dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatalf("foreign agent was removed: %v", err)
	}
	if string(got) != body {
		t.Error("foreign agent was rewritten")
	}
	// It must not even be reported as stale.
	for _, c := range mustDiff(t, testCfg("qwen"), dir) {
		if c.Name == "my-reviewer" {
			t.Error("foreign agent must not appear in the diff")
		}
	}
}

// A hand-edited gremlord-* file is reported as an update so sync can restore
// it — that file is ours by prefix.
func TestDiffDetectsHandEditedOwnedFile(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg("qwen")
	if _, err := Sync(cfg, dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gremlord-qwen.md")
	if err := os.WriteFile(path, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes := mustDiff(t, cfg, dir)
	if len(changes) != 1 || changes[0].Kind != "update" {
		t.Fatalf("want one update, got %+v", changes)
	}
}

func TestDiffMissingDirIsAllCreates(t *testing.T) {
	changes, err := Diff(testCfg("qwen"), filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir must not error: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != "create" {
		t.Errorf("want one create, got %+v", changes)
	}
}

func TestFingerprintTracksConfig(t *testing.T) {
	a := Fingerprint(testCfg("qwen"))
	if a != Fingerprint(testCfg("qwen")) {
		t.Error("fingerprint must be stable for the same config")
	}
	if a == Fingerprint(testCfg("qwen", "opus")) {
		t.Error("adding an alias must change the fingerprint")
	}
}

func TestDeclinedState(t *testing.T) {
	dir := t.TempDir()
	fp := Fingerprint(testCfg("qwen"))
	if Declined(dir, fp) {
		t.Error("nothing recorded yet")
	}
	if err := RecordDeclined(dir, fp); err != nil {
		t.Fatal(err)
	}
	if !Declined(dir, fp) {
		t.Error("should be declined after recording")
	}
	// A different config state is not covered by the old decline.
	if Declined(dir, Fingerprint(testCfg("qwen", "opus"))) {
		t.Error("a changed config must ask again")
	}
	ClearDeclined(dir)
	if Declined(dir, fp) {
		t.Error("cleared state should not be declined")
	}
}

func mustDiff(t *testing.T, cfg *config.Config, dir string) []Change {
	t.Helper()
	c, err := Diff(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// After the rename, the agentic-* files a previous version wrote are orphans:
// they still show up in Claude Code's subagent picker but point at the old
// binary's naming. Sync has to delete them, which means the scan has to see
// them even though they no longer carry the current prefix.
func TestSyncRemovesPreRenameFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg("qwen")

	stale := filepath.Join(dir, LegacyPrefix+"opus.md")
	if err := os.WriteFile(stale, []byte("old definition\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changes, err := Diff(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawRemove bool
	for _, c := range changes {
		if c.Kind == "remove" && c.Name == LegacyPrefix+"opus" {
			sawRemove = true
		}
	}
	if !sawRemove {
		t.Fatalf("want a remove for %sopus, got %+v", LegacyPrefix, changes)
	}

	if _, err := Sync(cfg, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("%s should have been removed", stale)
	}
	if _, err := os.Stat(filepath.Join(dir, Prefix+"qwen.md")); err != nil {
		t.Errorf("current-prefix file missing: %v", err)
	}
}
