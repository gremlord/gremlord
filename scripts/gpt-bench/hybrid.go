//go:build unix

package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gremlord/gremlord/internal/config"
)

//go:embed hybrid_worker.py
var hybridWorker []byte

func (p *proxy) requestRoute(arm, component string) (config.Resolved, string) {
	if arm == "claude-codex" && component == "coordinator" && p.coordinator != nil {
		return *p.coordinator, p.coordinator.Provider.Key()
	}
	return p.route, p.key
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Only the experimental arm receives this supplement. Coordinator and worker
// usage are both billed to the candidate, even with different model aliases.
func (e *executor) prepareHybrid(home, sid string) (string, error) {
	if err := e.prepareCodexGremlord(home, sid+"~worker"); err != nil {
		return "", err
	}
	script := filepath.Join(home, "hybrid_worker.py")
	if err := os.WriteFile(script, hybridWorker, 0600); err != nil {
		return "", err
	}
	command := "python3 " + shellQuote(script) + " --gremlord " + shellQuote(e.gremlord) + " --model benchmark --state-dir " + shellQuote(filepath.Join(home, "codex-worker")) + " --benchmark-isolation --dangerously-bypass-approvals-and-sandbox"
	return fmt.Sprintf(`For this workflow you coordinate a persistent headless Codex worker.
For each user request, use Bash to invoke the following command with the complete task on stdin (a quoted heredoc is suitable):
%s
Delegate the whole implementation and its tests in one coherent task, including every acceptance condition from the user. The worker has access to this workspace and runs its own native tools. It automatically resumes its own conversation, so later requests may refer to earlier requirements. Do not split work into individual shell commands or file edits for the worker, and do not implement the task yourself.
Wait for the worker to finish (use Bash background execution/polling if needed). You may inspect its changes or run checks afterward. If the worker reports a failure, report it without automatically repeating a potentially mutating task. Return a concise result to the user. Do not read files outside the task workspace except the worker's own artifacts when diagnosing a failure.`, command), nil
}
