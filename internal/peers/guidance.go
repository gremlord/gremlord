package peers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	guidanceStart = "<!-- gremlord:peers:start -->"
	guidanceEnd   = "<!-- gremlord:peers:end -->"

	// Pre-rename markers. Found and replaced in place, so the rename does not
	// strand an orphaned block that duplicates the guidance.
	legacyStart = "<!-- agentic:peers:start -->"
	legacyEnd   = "<!-- agentic:peers:end -->"
)

// Guidance tells a session how to turn an approximate name into a specific
// peer. It lands in ~/.claude/CLAUDE.md so plain `claude` sessions get it too,
// not just gremlord-launched ones.
const Guidance = `## Talking to other Claude sessions

When asked to work with another session by an approximate name — "work with
labs-service-secondlife-be", "ask the labs-ui session" — resolve it yourself
rather than asking which session was meant. Session names are auto-derived from
whatever that session is doing, so the name a user says is usually the project
directory, and ListAgents does not show directories.

1. Run ` + "`gremlord peers <approximate name>`" + ` — it matches on both session name and
   working directory, and lists only sessions that can actually receive a
   message.
2. Call ListAgents to get the ` + "`[ref]`" + ` for the name it returned.
3. SendMessage with ` + "`to: \"<name> [ref]\"`" + ` on first contact. The bare name is
   rejected the first time and works from then on.
4. To reply to an incoming message, copy its ` + "`from=`" + ` value verbatim as ` + "`to`" + `.

If several sessions match closely, ask which one — messaging the wrong project
is worse than a question. If the session someone means is unreachable, say so
instead of quietly picking a different one.`

// Action reports what InstallGuidance did to the file.
type Action int

const (
	Unchanged Action = iota
	Created
	Updated
)

// InstallGuidance writes Guidance into the CLAUDE.md at path, wrapped in
// markers so re-running replaces the block instead of stacking copies. Text
// outside the markers is left alone.
func InstallGuidance(path string) (Action, error) {
	block := guidanceStart + "\n" + Guidance + "\n" + guidanceEnd

	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return Unchanged, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Unchanged, err
		}
		return Created, os.WriteFile(path, []byte(block+"\n"), 0o644)
	}

	text := string(existing)
	startMarker, endMarker := guidanceStart, guidanceEnd
	start := strings.Index(text, startMarker)
	if start < 0 {
		// Fall back to the pre-rename markers so an agentic-era block is
		// rewritten where it stands instead of being left behind alongside a
		// second, near-identical block.
		if i := strings.Index(text, legacyStart); i >= 0 {
			startMarker, endMarker, start = legacyStart, legacyEnd, i
		}
	}
	if start < 0 {
		if strings.Contains(text, endMarker) {
			return Unchanged, fmt.Errorf("%s has a stray %s marker — remove it and re-run", path, endMarker)
		}
		return Updated, os.WriteFile(path, []byte(strings.TrimRight(text, "\n")+"\n\n"+block+"\n"), 0o644)
	}

	end := strings.Index(text[start:], endMarker)
	if end < 0 {
		return Unchanged, fmt.Errorf("%s has an unterminated gremlord block — restore its %s marker and re-run", path, endMarker)
	}
	end += start + len(endMarker)

	if text[start:end] == block {
		return Unchanged, nil
	}
	return Updated, os.WriteFile(path, []byte(text[:start]+block+text[end:]), 0o644)
}
