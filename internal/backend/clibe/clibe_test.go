package clibe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
)

type fakeExecutor struct {
	dir    string
	env    []string
	argv   []string
	stdout string
	stderr string
	err    error
	block  time.Duration
}

func (f *fakeExecutor) Run(ctx context.Context, dir string, env, argv []string, _ io.Reader, stdout, stderr io.Writer) error {
	f.dir = dir
	f.env = append([]string(nil), env...)
	f.argv = append([]string(nil), argv...)
	io.WriteString(stdout, f.stdout)
	io.WriteString(stderr, f.stderr)
	if f.block > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.block):
		}
	}
	return f.err
}

func testCall(t *testing.T, dir string, p config.Provider, modelID string, stream bool) *backend.Call {
	t.Helper()
	raw := []byte(`{"model":"peer","messages":[{"role":"user","content":[{"type":"text","text":"fix the tests"}]}]}`)
	if stream {
		raw = []byte(`{"model":"peer","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"fix the tests"}]}]}`)
	}
	env, err := anthropic.ParseEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set(CwdHeader, dir)
	return &backend.Call{
		Raw:      raw,
		Envelope: env,
		Header:   h,
		Route: config.Resolved{
			Alias:    "peer",
			Provider: p,
			Model:    config.Model{ID: modelID},
		},
	}
}

func TestMessagesRunsCodexInSessionDirectory(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecutor{stdout: "all fixed\n"}
	b := &Backend{
		Exec:     exec,
		LookPath: func(string) (string, error) { return "/usr/bin/codex", nil },
	}
	p := config.Provider{Type: config.ProviderCLI, Dialect: config.CLIDialectCodex, Sandbox: "workspace-write"}
	rr := httptest.NewRecorder()
	res := b.Messages(context.Background(), testCall(t, dir, p, "gpt-5", false), rr)

	if res.Status != 200 {
		t.Fatalf("status = %d, body = %s", res.Status, rr.Body.String())
	}
	if exec.dir != dir {
		t.Errorf("dir = %q, want %q", exec.dir, dir)
	}
	want := []string{"codex", "exec", "--skip-git-repo-check", "--sandbox", "workspace-write", "-m", "gpt-5", "fix the tests"}
	if !reflect.DeepEqual(exec.argv, want) {
		t.Errorf("argv = %#v, want %#v", exec.argv, want)
	}
	if !strings.Contains(rr.Body.String(), `"text":"all fixed"`) {
		t.Errorf("response missing final output: %s", rr.Body.String())
	}
}

func TestMessagesGrokStreamingSequence(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecutor{stdout: `{"result":"done"}`}
	b := &Backend{
		Exec:     exec,
		LookPath: func(string) (string, error) { return "/usr/bin/grok", nil },
	}
	p := config.Provider{Type: config.ProviderCLI, Dialect: config.CLIDialectGrok, Command: "grok-build"}
	rr := httptest.NewRecorder()
	res := b.Messages(context.Background(), testCall(t, dir, p, "grok-4", true), rr)

	if res.Status != 200 {
		t.Fatalf("status = %d, body = %s", res.Status, rr.Body.String())
	}
	want := []string{"grok-build", "--no-auto-update", "--output-format", "plain", "--model", "grok-4", "-p", "fix the tests"}
	if !reflect.DeepEqual(exec.argv, want) {
		t.Errorf("argv = %#v, want %#v", exec.argv, want)
	}
	body := rr.Body.String()
	last := -1
	for _, event := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		i := strings.Index(body[last+1:], "event: "+event)
		if i < 0 {
			t.Fatalf("missing %s after offset %d:\n%s", event, last, body)
		}
		last += i + 1
	}
	if !strings.Contains(body, `"text":"done"`) {
		t.Errorf("stream missing parsed final output: %s", body)
	}
}

func TestMessagesFailureIsFinalTextNotErrorEvent(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecutor{stderr: "authentication expired", err: errors.New("exit status 1")}
	b := &Backend{
		Exec:     exec,
		LookPath: func(string) (string, error) { return "/usr/bin/codex", nil },
	}
	p := config.Provider{Type: config.ProviderCLI, Dialect: config.CLIDialectCodex}
	rr := httptest.NewRecorder()
	res := b.Messages(context.Background(), testCall(t, dir, p, "", true), rr)

	if res.Status != 200 || res.ErrType != "api_error" {
		t.Fatalf("result = %+v", res)
	}
	body := rr.Body.String()
	if strings.Contains(body, "event: error") {
		t.Fatalf("failure must not emit retryable SSE error event:\n%s", body)
	}
	if !strings.Contains(body, "authentication expired") || !strings.Contains(body, "event: message_stop") {
		t.Errorf("failure should be final assistant text: %s", body)
	}
}

func TestMessagesNonStreamingFailureIsHTTP200(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecutor{stderr: "bad credentials", err: errors.New("exit status 1")}
	b := &Backend{
		Exec:     exec,
		LookPath: func(string) (string, error) { return "/usr/bin/codex", nil },
	}
	rr := httptest.NewRecorder()
	res := b.Messages(context.Background(), testCall(t, dir, config.Provider{Type: config.ProviderCLI, Dialect: config.CLIDialectCodex}, "", false), rr)
	if res.Status != 200 || rr.Code != 200 {
		t.Fatalf("result status=%d HTTP status=%d body=%s", res.Status, rr.Code, rr.Body.String())
	}
	if res.ErrType != "api_error" {
		t.Errorf("log result should still record the failure: %+v", res)
	}
	if !strings.Contains(rr.Body.String(), "bad credentials") {
		t.Errorf("body missing failure details: %s", rr.Body.String())
	}
}

func TestMessagesRefusesMissingBinaryBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecutor{}
	b := &Backend{
		Exec:     exec,
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	}
	rr := httptest.NewRecorder()
	res := b.Messages(context.Background(), testCall(t, dir, config.Provider{Type: config.ProviderCLI, Dialect: config.CLIDialectCodex}, "", false), rr)

	if res.Status != 400 || !strings.Contains(rr.Body.String(), "npm install -g @openai/codex") {
		t.Fatalf("result = %+v, body = %s", res, rr.Body.String())
	}
	if exec.argv != nil {
		t.Errorf("executor was invoked: %#v", exec.argv)
	}
}

func TestMessagesRefusesMissingWorkingDirectory(t *testing.T) {
	exec := &fakeExecutor{}
	b := &Backend{
		Exec:     exec,
		LookPath: func(string) (string, error) { return "/usr/bin/codex", nil },
	}
	call := testCall(t, t.TempDir(), config.Provider{Type: config.ProviderCLI, Dialect: config.CLIDialectCodex}, "", false)
	call.Header.Del(CwdHeader)
	rr := httptest.NewRecorder()
	res := b.Messages(context.Background(), call, rr)
	if res.Status != 400 || !strings.Contains(rr.Body.String(), CwdHeader) {
		t.Fatalf("result = %+v, body = %s", res, rr.Body.String())
	}
	if exec.argv != nil {
		t.Errorf("executor was invoked: %#v", exec.argv)
	}
}

func TestMessagesRefusesOversizedTask(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecutor{}
	b := &Backend{
		Exec:     exec,
		LookPath: func(string) (string, error) { return "/usr/bin/codex", nil },
	}
	task := strings.Repeat("x", maxTaskBytes+1)
	raw := []byte(`{"model":"peer","messages":[{"role":"user","content":[{"type":"text","text":"` + task + `"}]}]}`)
	env, err := anthropic.ParseEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set(CwdHeader, dir)
	call := &backend.Call{
		Raw: raw, Envelope: env, Header: h,
		Route: config.Resolved{Alias: "peer", Provider: config.Provider{Type: config.ProviderCLI, Dialect: config.CLIDialectCodex}},
	}
	rr := httptest.NewRecorder()
	res := b.Messages(context.Background(), call, rr)
	if res.Status != 400 || !strings.Contains(rr.Body.String(), "keep it under") {
		t.Fatalf("result = %+v, body = %s", res, rr.Body.String())
	}
	if exec.argv != nil {
		t.Errorf("executor was invoked: %#v", exec.argv)
	}
}

func TestMessagesStripsRouterSecretsFromDelegatedEnv(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecutor{stdout: "ok"}
	b := &Backend{
		Exec:     exec,
		LookPath: func(string) (string, error) { return "/usr/bin/codex", nil },
		Environ: func() []string {
			return []string{
				"PATH=/usr/bin",
				"ANTHROPIC_AUTH_TOKEN=secret",
				"ANTHROPIC_BASE_URL=http://127.0.0.1:41100",
				"ANTHROPIC_CUSTOM_HEADERS=X-Agentic-Session: x",
				"OPENAI_API_KEY=sk-test",
				"CODEX_HOME=/tmp/codex",
			}
		},
	}
	rr := httptest.NewRecorder()
	res := b.Messages(context.Background(), testCall(t, dir, config.Provider{Type: config.ProviderCLI, Dialect: config.CLIDialectCodex}, "", false), rr)
	if res.Status != 200 {
		t.Fatalf("status = %d body=%s", res.Status, rr.Body.String())
	}
	joined := strings.Join(exec.env, "\n")
	for _, secret := range []string{"ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_BASE_URL=", "ANTHROPIC_CUSTOM_HEADERS=", "OPENAI_API_KEY="} {
		if strings.Contains(joined, secret) {
			t.Errorf("delegated env leaked %s: %v", secret, exec.env)
		}
	}
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "CODEX_HOME=/tmp/codex") {
		t.Errorf("delegated env dropped required values: %v", exec.env)
	}
}

func TestMessagesTimeoutCancelsBlockedExecutor(t *testing.T) {
	dir := t.TempDir()
	exec := &fakeExecutor{block: 20 * time.Second}
	b := &Backend{
		Exec:     exec,
		LookPath: func(string) (string, error) { return "/usr/bin/codex", nil },
	}
	p := config.Provider{Type: config.ProviderCLI, Dialect: config.CLIDialectCodex, TimeoutMS: 50}
	start := time.Now()
	rr := httptest.NewRecorder()
	res := b.Messages(context.Background(), testCall(t, dir, p, "", false), rr)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout waited too long: %s", time.Since(start))
	}
	if res.Status != 200 || res.ErrType != "api_error" || !strings.Contains(rr.Body.String(), "timed out") {
		t.Fatalf("result = %+v body=%s", res, rr.Body.String())
	}
}

func TestOSExecutorKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are unix-only")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "still-running")
	script := filepath.Join(dir, "hang.sh")
	body := "#!/bin/sh\n(while true; do touch '" + marker + "'; sleep 0.1; done) &\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := OSExecutor{}.Run(ctx, dir, os.Environ(), []string{script}, nil, io.Discard, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err = %v, want deadline", err)
	}
	os.Remove(marker)
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("descendant kept writing after cancellation")
	}
}

func TestTaskTextUsesLastUserTurnAndToolResult(t *testing.T) {
	req, err := anthropic.ParseRequest([]byte(`{
		"model":"peer",
		"messages":[
			{"role":"user","content":"old"},
			{"role":"assistant","content":"working"},
			{"role":"user","content":[
				{"type":"text","text":"new"},
				{"type":"tool_result","tool_use_id":"x","content":"result"}
			]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := taskText(req); got != "new\n\nresult" {
		t.Errorf("taskText = %q", got)
	}
}
