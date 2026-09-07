package launch

import (
	"fmt"
	"os"

	"github.com/gremlord/gremlord/internal/agents"
	"github.com/gremlord/gremlord/internal/config"
)

// noticeAgentDrift prints a one-shot, NON-BLOCKING notice when the generated
// per-alias subagents have drifted from config, pointing at `gremlord agents
// sync`. It deliberately does not read stdin: this runs immediately before
// the interactive claude child takes over the terminal, and the tty may be
// in raw mode (Enter arrives as \r, not \n) — a cooked line read would hang
// forever. Quiet when in sync, when not on a terminal, when opted out, or
// when the user already dismissed this exact config state.
func noticeAgentDrift(cfg *config.Config, dataDir string) {
	if config.Getenv("NO_AGENT_SYNC") != "" {
		return
	}
	if !isTerminal(os.Stderr) {
		return // no one to read the notice
	}
	dir, err := agents.Dir()
	if err != nil {
		return
	}
	changes, err := agents.Diff(cfg, dir)
	if err != nil || len(changes) == 0 {
		return // in sync (or unreadable) — stay silent
	}
	fp := agents.Fingerprint(cfg)
	if agents.Declined(dataDir, fp) {
		return // already shown for this exact config state
	}

	fmt.Fprintf(os.Stderr, "\ngremlord: %s — run `gremlord agents sync` to make them selectable (e.g. subagent_type: %q).\n",
		summarize(changes), firstName(changes))
	fmt.Fprintln(os.Stderr, "  (won't mention again until your model aliases change; GREMLORD_NO_AGENT_SYNC=1 to silence)")

	// Record now so the notice is one-shot per config state — we're not
	// waiting for an answer, so "shown" is the only signal we have.
	_ = agents.RecordDeclined(dataDir, fp)
}

func summarize(changes []agents.Change) string {
	var create, update, remove int
	for _, c := range changes {
		switch c.Kind {
		case "create":
			create++
		case "update":
			update++
		case "remove":
			remove++
		}
	}
	var parts []string
	if create > 0 {
		parts = append(parts, fmt.Sprintf("%d new", create))
	}
	if update > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", update))
	}
	if remove > 0 {
		parts = append(parts, fmt.Sprintf("%d stale", remove))
	}
	return "model-alias subagents out of date (" + joinComma(parts) + ")"
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// firstName returns a created/updated subagent name for the example line,
// preferring one the user will actually gain.
func firstName(changes []agents.Change) string {
	for _, c := range changes {
		if c.Kind == "create" {
			return c.Name
		}
	}
	return changes[0].Name
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
