package launch

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gremlord/gremlord/internal/config"
)

// The wordmark is box-drawing characters at three bytes each, so measuring in
// bytes trebled the width and pushed centred text off the terminal.
func TestCenterMeasuresRunesNotBytes(t *testing.T) {
	const width = 20
	got := center("abc", width)
	if pad := len(got) - len(strings.TrimLeft(got, " ")); pad != (width-3)/2 {
		t.Errorf("ascii pad = %d, want %d", pad, (width-3)/2)
	}

	wide := "██████╗" // 7 runes, 21 bytes
	if utf8.RuneCountInString(wide) != 7 {
		t.Fatalf("fixture is %d runes", utf8.RuneCountInString(wide))
	}
	got = center(wide, width)
	pad := len(got) - len(strings.TrimLeft(got, " "))
	if pad != (width-7)/2 {
		t.Errorf("multibyte pad = %d, want %d (byte length would give %d)",
			pad, (width-7)/2, (width-len(wide))/2)
	}
}

func TestCenterNeverNegative(t *testing.T) {
	s := "a string wider than the banner"
	if got := center(s, 5); got != s {
		t.Errorf("center() = %q, want it unpadded", got)
	}
}

func TestSplashSecondsPrecedence(t *testing.T) {
	zero, ten := 0, 10
	cfg := &config.Config{SplashSeconds: &zero}

	if got := splashSeconds(cfg); got != 0 {
		t.Errorf("config 0 = %d, want 0 (off)", got)
	}
	t.Setenv("GREMLORD_SPLASH_SECONDS", "3")
	if got := splashSeconds(cfg); got != 3 {
		t.Errorf("env should win over config, got %d", got)
	}
	t.Setenv("GREMLORD_SPLASH_SECONDS", "not-a-number")
	if got := splashSeconds(cfg); got != 0 {
		t.Errorf("unparseable env should fall back to config, got %d", got)
	}
	cfg.SplashSeconds = &ten
	t.Setenv("GREMLORD_SPLASH_SECONDS", "")
	if got := splashSeconds(cfg); got != 10 {
		t.Errorf("config 10 = %d, want 10", got)
	}
	if got := splashSeconds(&config.Config{}); got != DefaultSplashSeconds {
		t.Errorf("unset = %d, want %d", got, DefaultSplashSeconds)
	}
}

// The pre-rename spelling still works for one release, like every other var.
func TestSplashSecondsAcceptsLegacyEnv(t *testing.T) {
	t.Setenv("AGENTIC_SPLASH_SECONDS", "4")
	if got := splashSeconds(&config.Config{}); got != 4 {
		t.Errorf("legacy env = %d, want 4", got)
	}
}
