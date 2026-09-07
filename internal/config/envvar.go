package config

import "os"

const (
	// EnvPrefix namespaces gremlord's own environment variables.
	EnvPrefix = "GREMLORD_"

	// LegacyEnvPrefix is the pre-rename prefix, still read for one release.
	// A session launched by an older agentic binary — or a statusline hook in
	// ~/.claude/settings.json still pointing at the old executable — would
	// otherwise lose its session identity and silently stop tracking spend.
	LegacyEnvPrefix = "AGENTIC_"
)

// EnvName returns the current name of one of gremlord's variables.
func EnvName(suffix string) string { return EnvPrefix + suffix }

// Getenv reads GREMLORD_<suffix>, falling back to AGENTIC_<suffix>. The new
// spelling wins when both are set, so a half-migrated environment resolves to
// whichever launcher ran most recently rather than to a stale value.
func Getenv(suffix string) string {
	if v := os.Getenv(EnvPrefix + suffix); v != "" {
		return v
	}
	return os.Getenv(LegacyEnvPrefix + suffix)
}

// EnvNames returns both spellings, for code that has to strip gremlord's own
// variables out of a child process's environment.
func EnvNames(suffix string) []string {
	return []string{EnvPrefix + suffix, LegacyEnvPrefix + suffix}
}
