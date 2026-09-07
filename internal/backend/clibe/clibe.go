// Package clibe delegates a whole task to a locally installed coding-agent
// CLI (Codex, Grok Build) running under the user's own subscription login.
// gremlord never sees the CLI's credentials — the binary authenticates itself
// from its own cached login (`codex login` / `grok login`). The delegated CLI
// runs its own independent agent loop with filesystem access in the calling
// session's working directory, so a request here is minutes of autonomous
// work, not a completion — which is why config validation keeps cli aliases
// out of auto-routing and profile tiers, and why failures are surfaced as
// message text with end_turn instead of a retryable HTTP/SSE error: Claude
// Code retries those, and re-running a filesystem-mutating agent is the one
// thing this backend must never cause.
package clibe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/tokens"

	"github.com/gremlord/gremlord/internal/wire"
)

// CwdHeader carries the launching session's working directory on every
// request (set via ANTHROPIC_CUSTOM_HEADERS in internal/launch). The router
// leader is a long-lived shared process, so its own cwd is meaningless — the
// delegated CLI must run where the session runs. The header is set by the
// same local trusted launch process and every request is gated by the
// per-install token, so this is not a cross-trust boundary; the backend still
// validates it (absolute, existing directory) as defense against bugs.
const CwdHeader = wire.HeaderCwd

const (
	defaultTimeout   = 20 * time.Minute
	stderrTailBytes  = 32 * 1024
	stdoutLimitBytes = 1 << 20
	maxTaskBytes     = 100_000
	pingInterval     = 15 * time.Second
	teardownWait     = 5 * time.Second
)

// Executor is the process-exec seam (mirrors internal/eval's; duplicated so
// the backend doesn't depend on the eval package).
type Executor interface {
	Run(ctx context.Context, dir string, env, argv []string, stdin io.Reader, stdout, stderr io.Writer) error
}

type OSExecutor struct{}

func (OSExecutor) Run(ctx context.Context, dir string, env, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return errors.New("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, env, stdin, stdout, stderr
	isolateProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		killProcessTree(cmd)
		select {
		case <-done:
		case <-time.After(teardownWait):
		}
		return ctx.Err()
	}
}

type Backend struct {
	Exec     Executor
	LookPath func(string) (string, error)
	// Environ, when set, replaces os.Environ for the delegated process
	// (tests only).
	Environ func() []string
}

func New() *Backend {
	return &Backend{Exec: OSExecutor{}, LookPath: exec.LookPath}
}

func (b *Backend) Messages(ctx context.Context, call *backend.Call, w http.ResponseWriter) backend.Result {
	prov := call.Route.Provider

	cwd := wire.Cwd(call.Header)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return refuse(w, "cli delegation needs the session working directory (launch via `gremlord` so "+CwdHeader+" is set)")
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return refuse(w, fmt.Sprintf("cli delegation working directory %q is not an existing directory", cwd))
	}

	bin := prov.Bin()
	if _, err := b.LookPath(bin); err != nil {
		return refuse(w, fmt.Sprintf("%s CLI (%q) not found on PATH — install it first (%s)", prov.Dialect, bin, installHint(prov.Dialect)))
	}

	req, err := anthropic.ParseRequest(call.Raw)
	if err != nil {
		return refuse(w, err.Error())
	}
	task := taskText(req)
	if task == "" {
		return refuse(w, "cli delegation found no task text in the request")
	}
	if len(task) > maxTaskBytes {
		return refuse(w, fmt.Sprintf("cli delegation task is %d bytes; keep it under %d so it can be passed as a single argv element", len(task), maxTaskBytes))
	}

	timeout := defaultTimeout
	if prov.TimeoutMS > 0 {
		timeout = time.Duration(prov.TimeoutMS) * time.Millisecond
	}
	argv := buildArgv(prov, call.Route.Model.ID, task)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Open the stream before the run so keep-alive pings hold the
	// connection through a multi-minute delegation. A failed first write
	// means the client is already gone — do not spawn.
	var sse *anthropic.SSEWriter
	estIn := estimateTokens(task)
	if call.Envelope.Stream {
		sse = anthropic.NewSSEWriter(w)
		if err := sse.Event("message_start", map[string]any{
			"type": "message_start",
			"message": anthropic.MessagesResponse{
				ID: "msg_gremlord_cli", Type: "message", Role: "assistant",
				Model: call.Route.Alias, Content: []anthropic.ContentBlock{},
				Usage: anthropic.Usage{InputTokens: estIn},
			},
		}); err != nil {
			return backend.Result{Status: 499, ErrType: "client_disconnect", ErrMsg: "client disconnected before delegation", Usage: anthropic.Usage{InputTokens: estIn}, ReportedInput: estIn}
		}
		if err := sse.Ping(); err != nil {
			return backend.Result{Status: 499, ErrType: "client_disconnect", ErrMsg: "client disconnected before delegation", Usage: anthropic.Usage{InputTokens: estIn}, ReportedInput: estIn}
		}
	}

	var stdout limitedBuffer
	stdout.max = stdoutLimitBytes
	stderr := &tailWriter{max: stderrTailBytes}
	done := make(chan error, 1)
	go func() {
		done <- b.Exec.Run(runCtx, cwd, sanitizeEnv(b.environ()), argv, nil, &stdout, stderr)
	}()
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	var runErr error
	timedOut := false
wait:
	for {
		select {
		case runErr = <-done:
			break wait
		case <-runCtx.Done():
			timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
			select {
			case runErr = <-done:
			case <-time.After(teardownWait):
				if runErr == nil {
					runErr = runCtx.Err()
				}
			}
			break wait
		case <-ticker.C:
			if sse != nil && sse.Ping() != nil {
				cancel()
			}
		}
	}

	// Client gone after a completed run: keep the completed result. Only
	// classify disconnect when cancellation actually stopped the process.
	if ctx.Err() != nil && !timedOut && errors.Is(runErr, context.Canceled) {
		return backend.Result{Status: 499, ErrType: "client_disconnect", ErrMsg: "client disconnected mid-delegation", Usage: anthropic.Usage{InputTokens: estIn}, ReportedInput: estIn}
	}

	text := stdoutText(stdout.Bytes())
	// After spawn, always write a finished assistant turn (HTTP 200 /
	// end_turn). A 4xx/5xx or SSE error event would make Claude Code retry
	// a filesystem-mutating agent. Status/ErrType on Result stay for logs.
	res := backend.Result{Status: 200}
	switch {
	case timedOut || errors.Is(runErr, context.DeadlineExceeded):
		text = fmt.Sprintf("gremlord: %s delegation timed out after %s; partial changes may exist in %s", prov.Dialect, timeout, cwd)
		res.ErrType, res.ErrMsg = "api_error", "delegation timeout"
	case runErr != nil:
		msg := runErr.Error()
		if tail := stderr.String(); tail != "" {
			msg += ": " + tail
		}
		text = fmt.Sprintf("gremlord: %s delegation failed (%s)", prov.Dialect, msg)
		res.ErrType, res.ErrMsg = "api_error", truncate(msg, 512)
	case text == "":
		text = fmt.Sprintf("gremlord: %s delegation produced no output", prov.Dialect)
		res.ErrType, res.ErrMsg = "api_error", "empty delegation output"
	}
	if stdout.truncated {
		text += "\n\ngremlord: stdout truncated at 1MiB"
	}
	// Heuristic usage so the delegation shows up in `gremlord cost` as a $0
	// unpriced row (validation forbids pricing on cli models).
	res.Usage = anthropic.Usage{InputTokens: estIn, OutputTokens: estimateTokens(text)}
	res.ReportedInput = estIn

	if sse != nil {
		_ = sse.Event("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		_ = sse.Event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{"type": "text_delta", "text": text},
		})
		_ = sse.Event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		_ = sse.Event("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]int64{"input_tokens": res.Usage.InputTokens, "output_tokens": res.Usage.OutputTokens},
		})
		_ = sse.Event("message_stop", map[string]any{"type": "message_stop"})
		return res
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anthropic.MessagesResponse{
		ID: "msg_gremlord_cli", Type: "message", Role: "assistant",
		Model:      call.Route.Alias,
		Content:    []anthropic.ContentBlock{{Type: "text", Text: text}},
		StopReason: "end_turn",
		Usage:      res.Usage,
	})
	return res
}

// CountTokens serves a local estimate, like openaibe; cli models have no
// context budget (validation forbids one) so the scale factor is 1.
func (b *Backend) CountTokens(ctx context.Context, call *backend.Call, w http.ResponseWriter) backend.Result {
	req, err := anthropic.ParseRequest(call.Raw)
	if err != nil {
		anthropic.WriteError(w, 400, "invalid_request_error", "gremlord: "+err.Error())
		return backend.Result{Status: 400, ErrType: "invalid_request_error"}
	}
	n := tokens.ScaleCount(call.EstimateInput(req), tokens.ScaleFactor(call.ScaleBudget()))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anthropic.CountTokensResponse{InputTokens: n})
	return backend.Result{Status: 200}
}

// refuse rejects a delegation before any subprocess spawn or stream byte.
// 400 deliberately (same rationale as the router's budget gate): Claude Code
// surfaces 400s verbatim instead of retry-spinning, and retrying a
// side-effectful agent run is worse than retrying a completion.
func refuse(w http.ResponseWriter, msg string) backend.Result {
	full := "gremlord: " + msg
	anthropic.WriteError(w, 400, "invalid_request_error", full)
	return backend.Result{Status: 400, ErrType: "invalid_request_error", ErrMsg: truncate(msg, 512)}
}

func (b *Backend) environ() []string {
	if b.Environ != nil {
		return b.Environ()
	}
	return os.Environ()
}

// sanitizeEnv drops gremlord/router credentials and unrelated provider keys
// from the delegated process. The peer CLI authenticates itself from its own
// login; it does not need the launcher's secrets.
func sanitizeEnv(env []string) []string {
	drop := map[string]bool{
		"ANTHROPIC_BASE_URL":         true,
		"ANTHROPIC_AUTH_TOKEN":       true,
		"ANTHROPIC_API_KEY":          true,
		"ANTHROPIC_CUSTOM_HEADERS":   true,
		"ANTHROPIC_MODEL":            true,
		"ANTHROPIC_SMALL_FAST_MODEL": true,
		"CLAUDE_CODE_SUBAGENT_MODEL": true,
		"GREMLORD_SESSION_ID":        true,
		"GREMLORD_PROFILE":           true,
		"AGENTIC_SESSION_ID":         true,
		"AGENTIC_PROFILE":            true,
		"OPENAI_API_KEY":             true,
		"XAI_API_KEY":                true,
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if drop[key] || strings.HasPrefix(key, "ANTHROPIC_DEFAULT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func installHint(dialect string) string {
	switch dialect {
	case config.CLIDialectCodex:
		return "npm install -g @openai/codex"
	case config.CLIDialectGrok:
		return "see https://docs.x.ai/build"
	}
	return "install the CLI"
}

// buildArgv shapes the headless invocation per dialect. The task travels as
// a single argv element — never through a shell.
func buildArgv(p config.Provider, modelID, task string) []string {
	switch p.Dialect {
	case config.CLIDialectCodex:
		// `codex exec` prints only the final agent message to stdout,
		// progress to stderr. --skip-git-repo-check: delegations may target
		// non-git directories.
		argv := []string{p.Bin(), "exec", "--skip-git-repo-check"}
		if p.Sandbox != "" {
			argv = append(argv, "--sandbox", p.Sandbox)
		}
		if modelID != "" {
			argv = append(argv, "-m", modelID)
		}
		return append(argv, task)
	case config.CLIDialectGrok:
		argv := []string{p.Bin(), "--no-auto-update", "--output-format", "plain"}
		if modelID != "" {
			argv = append(argv, "--model", modelID)
		}
		return append(argv, "-p", task)
	}
	return nil // unreachable: config validation closes the dialect set
}

// taskText extracts the delegated task from the last user turn: the Task
// tool sends the task prompt as the sole user message. System prompt and
// tool definitions are deliberately not forwarded — the peer CLI runs its
// own harness.
func taskText(req *anthropic.MessagesRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		out := ""
		for _, blk := range req.Messages[i].Content {
			var t string
			switch blk.Type {
			case "text":
				t = blk.Text
			case "tool_result":
				t = blk.FlatText()
			}
			if t == "" {
				continue
			}
			if out != "" {
				out += "\n\n"
			}
			out += t
		}
		if strings.TrimSpace(out) != "" {
			return out
		}
	}
	return ""
}

// stdoutText harvests the CLI's answer: trimmed stdout, with a JSON probe
// (mirrors internal/eval's finalText) as a guard in case a CLI's plain
// output turns out to wrap JSON.
func stdoutText(out []byte) string {
	s := strings.TrimSpace(string(out))
	if !strings.HasPrefix(s, "{") {
		return s
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil {
		return s
	}
	for _, key := range []string{"result", "text", "content"} {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return s
}

// estimateTokens is the same bytes/4 heuristic internal/tokens uses.
func estimateTokens(s string) int64 {
	return int64(len(s)/4) + 1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// tailWriter keeps only the last max bytes written — enough stderr to
// surface a useful error without holding a whole agent run's progress log.
type tailWriter struct {
	buf []byte
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	return strings.TrimSpace(string(t.buf))
}

// limitedBuffer keeps at most max bytes and records whether it discarded the
// rest, so a verbose CLI cannot grow the shared router leader without bound.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remain := b.max - b.buf.Len()
	if remain <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remain {
		b.buf.Write(p[:remain])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
