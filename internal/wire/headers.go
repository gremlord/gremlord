// Package wire names the HTTP headers the launcher and the router use to
// attribute a request to a session. Claude Code carries them transparently on
// every request via ANTHROPIC_CUSTOM_HEADERS, so they are a protocol between
// two gremlord processes rather than anything a user sets.
package wire

import "net/http"

// Current header names.
const (
	HeaderSession  = "X-Gremlord-Session"
	HeaderProfile  = "X-Gremlord-Profile"
	HeaderPinModel = "X-Gremlord-Pin-Model"
	HeaderCwd      = "X-Gremlord-Cwd"
)

// Pre-rename header names, still accepted on read and still written alongside
// the new ones for one release.
//
// Both directions of a mixed install are reachable, because the router is
// whichever binary won the port: a new gremlord launcher can be talking to an
// gremlord router left running from before the upgrade, or the reverse. A
// reader that understood only one spelling would silently drop session
// attribution, which surfaces as spend that no session accounts for.
const (
	LegacyHeaderSession  = "X-Agentic-Session"
	LegacyHeaderProfile  = "X-Agentic-Profile"
	LegacyHeaderPinModel = "X-Agentic-Pin-Model"
	LegacyHeaderCwd      = "X-Agentic-Cwd"
)

func first(h http.Header, name, legacy string) string {
	if v := h.Get(name); v != "" {
		return v
	}
	return h.Get(legacy)
}

// Session returns the launching session's id.
func Session(h http.Header) string { return first(h, HeaderSession, LegacyHeaderSession) }

// Profile returns the launching session's profile name.
func Profile(h http.Header) string { return first(h, HeaderProfile, LegacyHeaderProfile) }

// PinModel returns the model every tier is pinned to, or "" when unpinned.
func PinModel(h http.Header) string { return first(h, HeaderPinModel, LegacyHeaderPinModel) }

// Cwd returns the launching session's working directory.
func Cwd(h http.Header) string { return first(h, HeaderCwd, LegacyHeaderCwd) }

// Router control-plane paths, served under the Messages API on the same local
// port. Both spellings are served and probed for one release, for the same
// mixed-install reason as the headers: a gremlord CLI must be able to
// health-check and hot-reload an agentic router that still holds the port, or
// leader election reads a healthy peer as a foreign process and refuses to
// start.
const (
	PathHealth = "/gremlord/health"
	PathReload = "/gremlord/reload"

	LegacyPathHealth = "/agentic/health"
	LegacyPathReload = "/agentic/reload"
)
