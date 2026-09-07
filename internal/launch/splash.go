package launch

import (
	"embed"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gremlord/gremlord/internal/config"
)

//go:embed gremlord.txt
var bannerFS embed.FS

// DefaultSplashSeconds is how long the banner is held before Claude Code takes
// the terminal.
const DefaultSplashSeconds = 10

var taglines = []string{
	"it's smart. it's in the wires. it's already in prod.",
	"gremlins break things. this one files the expense report.",
	"your tokens now answer to a higher authority.",
}

// showSplash prints the banner and holds it for the configured duration.
//
// Deliberately does not read stdin. This runs immediately before the
// interactive claude child inherits the terminal, so consuming input here
// could swallow a keystroke meant for it, or block on a tty that is not ours —
// the same reason noticeAgentDrift stays non-blocking.
//
// Everything goes to stderr, so `gremlord ... | something` is unaffected, and
// it is skipped entirely when stderr is not a terminal, which covers CI, pipes
// and scripted launches.
func showSplash(cfg *config.Config) {
	secs := splashSeconds(cfg)
	if secs <= 0 || !isTerminal(os.Stderr) {
		return
	}

	art, err := bannerFS.ReadFile("gremlord.txt")
	if err != nil {
		return // embedded and therefore unreachable, but never block a launch over art
	}
	// Count runes, not bytes: the wordmark is box-drawing characters at three
	// bytes each, so a byte length would treble the width and shove anything
	// centred against it off the right of the terminal.
	width := 0
	for _, line := range strings.Split(string(art), "\n") {
		if n := utf8.RuneCountInString(line); n > width {
			width = n
		}
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, string(art))
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, center(taglines[rand.IntN(len(taglines))], width))
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, center(fmt.Sprintf("starting claude in %ds  ·  %s=0 to skip this",
		secs, config.EnvName("SPLASH_SECONDS")), width))

	time.Sleep(time.Duration(secs) * time.Second)
}

// splashSeconds resolves the hold duration: the environment wins, then config,
// then the default. Zero or negative disables it.
func splashSeconds(cfg *config.Config) int {
	if v := config.Getenv("SPLASH_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if cfg != nil && cfg.SplashSeconds != nil {
		return *cfg.SplashSeconds
	}
	return DefaultSplashSeconds
}

func center(s string, width int) string {
	if pad := (width - utf8.RuneCountInString(s)) / 2; pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}
