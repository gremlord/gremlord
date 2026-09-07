package wire

import (
	"net/http"
	"testing"
)

// A pre-rename launcher's headers must still identify the session, or its spend
// lands in the log with nothing to attribute it to.
func TestReadersAcceptLegacySpelling(t *testing.T) {
	h := http.Header{}
	h.Set(LegacyHeaderSession, "sess")
	h.Set(LegacyHeaderProfile, "main")
	h.Set(LegacyHeaderPinModel, "opus")
	h.Set(LegacyHeaderCwd, "/tmp/p")

	if got := Session(h); got != "sess" {
		t.Errorf("Session() = %q, want %q", got, "sess")
	}
	if got := Profile(h); got != "main" {
		t.Errorf("Profile() = %q, want %q", got, "main")
	}
	if got := PinModel(h); got != "opus" {
		t.Errorf("PinModel() = %q, want %q", got, "opus")
	}
	if got := Cwd(h); got != "/tmp/p" {
		t.Errorf("Cwd() = %q, want %q", got, "/tmp/p")
	}
}

func TestCurrentSpellingWins(t *testing.T) {
	h := http.Header{}
	h.Set(LegacyHeaderSession, "old")
	h.Set(HeaderSession, "new")
	if got := Session(h); got != "new" {
		t.Errorf("Session() = %q, want %q", got, "new")
	}
}

func TestMissingHeaderIsEmpty(t *testing.T) {
	if got := Session(http.Header{}); got != "" {
		t.Errorf("Session() = %q, want empty", got)
	}
}
