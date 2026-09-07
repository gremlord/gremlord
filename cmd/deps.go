package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mattn/go-isatty"
)

// ensureClaude checks for the claude binary and, if missing, offers to
// install it. claude is a required dependency.
func ensureClaude() bool {
	return promptInstall("claude", "claude binary", true,
		"curl -fsSL https://claude.ai/install.sh | bash", "https://claude.com/claude-code")
}

// ensureClauder checks for the clauder binary and, if missing, offers to
// install it. clauder is optional — gremlord works without it, and since
// Claude Code gained native cross-instance messaging it is wanted only for
// persistent memory.
func ensureClauder() bool {
	return promptInstall("clauder", "clauder (persistent memory)", false,
		"curl -fsSL https://raw.githubusercontent.com/MaorBril/clauder/main/install.sh | sh",
		"https://github.com/MaorBril/clauder")
}

// promptInstall checks binName on PATH. If missing and stdin is a terminal,
// it asks [y/N] and, on yes, runs installCmd via `sh -c` with inherited
// stdio, then re-checks PATH. Returns whether the dep is present afterward.
func promptInstall(binName, label string, required bool, installCmd, docsURL string) bool {
	if _, err := exec.LookPath(binName); err == nil {
		fmt.Printf("✓ %s found\n", label)
		return true
	}
	marker := "·"
	if required {
		marker = "⚠"
	}
	fmt.Printf("%s %s not found\n", marker, label)

	if !isatty.IsTerminal(os.Stdin.Fd()) {
		fmt.Printf("  Install with: %s  (%s)\n", installCmd, docsURL)
		return false
	}
	fmt.Printf("Install now by running:\n  %s\n[y/N] ", installCmd)
	reply, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(reply)), "y") {
		fmt.Printf("  Install later: %s  (%s)\n", installCmd, docsURL)
		return false
	}
	c := exec.Command("sh", "-c", installCmd)
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := c.Run(); err != nil {
		fmt.Printf("  install failed: %v\n", err)
		return false
	}
	_, err := exec.LookPath(binName)
	return err == nil
}
