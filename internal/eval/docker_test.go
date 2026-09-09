package eval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeDocker writes a CLI fixture that behaves like the subset of Docker the
// candidate runner uses. Every invocation is recorded in calls.log; install,
// candidate output, patch, and failure behavior are controlled by env vars.
func fakeDocker(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := filepath.Join(dir, "docker")
	script := `#!/bin/sh
set -u
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
case "$1" in
  run)
    if [ "${FAKE_DOCKER_RUN_FAIL:-}" = "1" ]; then echo "pull failed" >&2; exit 125; fi
    printf 'container-123\n'
    ;;
  rm) exit 0 ;;
  exec)
    all="$*"
    case "$all" in
      *"registry.npmjs.org/@anthropic-ai/claude-code-linux-x64"*) exit 0 ;;
      *"command -v claude"*) printf '/usr/local/bin/claude\n'; exit 0 ;;
      *"git rev-parse HEAD"*) printf 'base-sha\n'; exit 0 ;;
      *"git add -A -N"*) exit 0 ;;
      *"iname CLAUDE.md"*) printf '/testbed/CLAUDE.md\n'; exit 0 ;;
      *"test -e "*) exit 1 ;;
      *"git cat-file -e"*) exit 0 ;;
      *"git checkout base-sha --"*) exit 0 ;;
      *"git diff base-sha --binary --no-ext-diff"*)
        if [ "${FAKE_DOCKER_DIFF_FAIL:-}" = "1" ]; then echo "diff failed" >&2; exit 1; fi
        printf '%s' "${FAKE_DOCKER_PATCH:-}"; exit 0 ;;
      *"/usr/local/bin/claude --print"*)
        if [ "${FAKE_DOCKER_CLAUDE_FAIL:-}" = "1" ]; then echo "candidate failed" >&2; exit 7; fi
        printf '%s' "${FAKE_DOCKER_CLAUDE_OUT:-{\"result\":\"done\"}}"
        exit 0
        ;;
      *) exit 0 ;;
    esac
    ;;
  *) echo "unexpected command: $*" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func withFakeDockerEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("FAKE_DOCKER_LOG", filepath.Join(dir, "calls.log"))
	t.Setenv("FAKE_DOCKER_PATCH", "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-old\n+new\n")
	t.Setenv("FAKE_DOCKER_CLAUDE_OUT", `{"result":"done"}`)
}

func TestRunDockerCandidateLifecycleAndRedaction(t *testing.T) {
	dir := t.TempDir()
	withFakeDockerEnv(t, dir)
	bin := fakeDocker(t, dir)
	result := RunDockerCandidate(context.Background(), DockerOptions{DockerBin: bin}, SWEBenchInstance{
		InstanceID: "repo__repo-1", InstanceImageKey: "sweb.eval.x86_64.repo__repo-1:latest",
	}, "fix the bug", "auto", time.Minute, DockerContainerEnv{
		BaseURL: "http://host.docker.internal:41234", Token: "super-secret", SessionID: "eval-session", Profile: "main",
	})
	if result.Status != StatusComplete || result.ExitCode != 0 || result.AgentMS > result.DurationMS || !strings.Contains(result.Patch, "+new") {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(string(result.Stdout), `"result":"done"`) {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	log := string(result.ContainerLog)
	if strings.Contains(log, "super-secret") {
		t.Fatalf("container log leaked token:\n%s", log)
	}
	for _, want := range []string{
		"docker run", "claude code installed", "ANTHROPIC_AUTH_TOKEN=<redacted>",
		"ANTHROPIC_BASE_URL=<redacted>", "git add -A -N", "git diff base-sha --binary --no-ext-diff",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("container log missing %q:\n%s", want, log)
		}
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "rm -f container-123") {
		t.Errorf("container was not removed:\n%s", calls)
	}
	if !strings.Contains(string(calls), "-u nonroot") || !strings.Contains(string(calls), "HOME=/tmp/gremlord-eval-home") || !strings.Contains(string(calls), "--strict-mcp-config") {
		t.Errorf("candidate did not run as nonroot:\n%s", calls)
	}
	if !strings.Contains(string(calls), "iname CLAUDE.local.md") || !strings.Contains(string(calls), "git checkout base-sha -- CLAUDE.md") {
		t.Errorf("harness instruction isolation/restore missing:\n%s", calls)
	}
	if !strings.Contains(string(calls), "X-Agentic-Session: eval-session") || !strings.Contains(string(calls), "X-Agentic-Profile: main") {
		t.Errorf("session/profile env missing:\n%s", calls)
	}
}

func TestRunDockerCandidateClassifiesInfrastructureAndModelFailures(t *testing.T) {
	t.Run("docker run", func(t *testing.T) {
		dir := t.TempDir()
		withFakeDockerEnv(t, dir)
		t.Setenv("FAKE_DOCKER_RUN_FAIL", "1")
		result := RunDockerCandidate(context.Background(), DockerOptions{DockerBin: fakeDocker(t, dir)}, SWEBenchInstance{
			InstanceImageKey: "image:latest",
		}, "prompt", "auto", time.Minute, DockerContainerEnv{})
		if result.Status != StatusDockerError {
			t.Fatalf("result = %+v", result)
		}
		if !((CandidateResult{Status: result.Status}).InfraFailed()) {
			t.Fatalf("docker error is not classified as infrastructure: %+v", result)
		}
	})

	t.Run("candidate", func(t *testing.T) {
		dir := t.TempDir()
		withFakeDockerEnv(t, dir)
		t.Setenv("FAKE_DOCKER_CLAUDE_FAIL", "1")
		result := RunDockerCandidate(context.Background(), DockerOptions{DockerBin: fakeDocker(t, dir)}, SWEBenchInstance{
			InstanceImageKey: "image:latest",
		}, "prompt", "auto", time.Minute, DockerContainerEnv{})
		if result.Status != StatusModelError || result.ExitCode != 7 {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestRunDockerCandidateCaptureFailureIsInfrastructure(t *testing.T) {
	dir := t.TempDir()
	withFakeDockerEnv(t, dir)
	t.Setenv("FAKE_DOCKER_DIFF_FAIL", "1")
	result := RunDockerCandidate(context.Background(), DockerOptions{DockerBin: fakeDocker(t, dir)}, SWEBenchInstance{
		InstanceImageKey: "image:latest",
	}, "prompt", "auto", time.Minute, DockerContainerEnv{})
	if result.Status != StatusDockerError || !strings.Contains(result.Error, "git diff") {
		t.Fatalf("result = %+v", result)
	}
}

func TestDockerContainerEnvAndArgRedaction(t *testing.T) {
	env := containerEnvArgs(DockerContainerEnv{BaseURL: "url", Token: "token", SessionID: "sid", Profile: "profile", Model: "gpt-5.6-sol"})
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=url", "ANTHROPIC_AUTH_TOKEN=token", "X-Agentic-Session: sid", "X-Agentic-Profile: profile",
		"X-Agentic-Pin-Model: gpt-5.6-sol",
		"ANTHROPIC_MODEL=gpt-5.6-sol",
		"ANTHROPIC_SMALL_FAST_MODEL=gpt-5.6-sol",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5.6-sol",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5.6-sol",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=gpt-5.6-sol",
		"CLAUDE_CODE_SUBAGENT_MODEL=gpt-5.6-sol",
		"ENABLE_TOOL_SEARCH=false",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q: %s", want, joined)
		}
	}
	redacted := strings.Join(redactDockerArgs([]string{"exec", "-e", "ANTHROPIC_AUTH_TOKEN=token", "-e", "AGENTIC_SESSION_ID=sid", "container"}), " ")
	if strings.Contains(redacted, "token") || strings.Contains(redacted, "sid") || !strings.Contains(redacted, "ANTHROPIC_AUTH_TOKEN=<redacted>") {
		t.Errorf("redacted args = %q", redacted)
	}
}

func TestRunnerSWEBenchEndToEndWithOfficialBridgeProtocol(t *testing.T) {
	dir := t.TempDir()
	withFakeDockerEnv(t, dir)
	docker := fakeDocker(t, dir)
	bridge := filepath.Join(dir, "bridge.sh")
	bridgeScript := `#!/bin/sh
op=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--op" ]; then op="$arg"; fi
  prev="$arg"
done
case "$op" in
  check)
    printf '%s\n' '{"swebench_version":"4.1.0","swebench_ok":true,"python_version":"3.11","arch":"arm64","docker_ok":true,"docker_error":"","error":""}'
    ;;
  resolve)
    printf '%s\n' '{"instances":[{"instance_id":"astropy__astropy-14309","repo":"astropy/astropy","base_commit":"abc","problem_statement":"fix astropy","version":"5.1","instance_image_key":"sweb.eval.arm64.astropy__astropy-14309:latest","env_image_key":"sweb.env.arm64.hash:latest","arch":"arm64","namespace":""}]}'
    ;;
  ensure_image)
    printf '%s\n' '{"images":{"astropy__astropy-14309":"sweb.eval.arm64.astropy__astropy-14309:latest"}}'
    ;;
  grade)
    printf '%s\n' '{"report_path":"official.json","instances":{"astropy__astropy-14309":{"patch_is_None":false,"patch_exists":true,"patch_successfully_applied":true,"resolved":true,"tests_status":{"FAIL_TO_PASS":{"success":["test_fix"],"failure":[]},"PASS_TO_PASS":{"success":["test_old"],"failure":[]}}}}}'
    ;;
  *) printf '%s\n' '{"error":"unknown op"}'; exit 1 ;;
esac
`
	if err := os.WriteFile(bridge, []byte(bridgeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	out := filepath.Join(dir, "out")
	manifest := &Manifest{
		Version: SchemaVersion, Name: "swebench-test",
		Dataset: Dataset{Type: "swebench", Source: "princeton-nlp/SWE-bench_Verified", Split: "test", Tasks: []string{"astropy__astropy-14309"}},
		Sandbox: Sandbox{Type: "docker"},
	}
	runner := &Runner{Options: Options{
		Baseline: "opus", MUT: "auto", Judge: "none", Attempts: 1, Timeout: time.Minute,
		OutputDir: out, BaseURL: upstream.URL, Token: "token", Profile: "main",
		SWEBench: SWEBenchEnv{Python: bridge, BridgePath: "ignored.py"},
		Docker:   DockerOptions{DockerBin: docker},
	}}
	summary, err := runner.Run(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Pairs) != 1 || summary.Pairs[0].Winner != WinnerTie || summary.BaselineVerifierPasses != 1 || summary.MUTVerifierPasses != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.DatasetFingerprint == "" || summary.SWEBenchVersion != SWEBenchPackageVersion {
		t.Fatalf("dataset metadata missing: %+v", summary)
	}
	for _, candidate := range []CandidateResult{summary.Pairs[0].Baseline, summary.Pairs[0].MUT} {
		if candidate.Status != StatusComplete || candidate.Verifier.SWEBench == nil || !candidate.Verifier.SWEBench.Resolved || len(candidate.Verifier.SWEBench.FailToPassOK) != 1 {
			t.Fatalf("candidate = %+v", candidate)
		}
	}
	for _, path := range []string{
		filepath.Join(out, "dataset.json"), filepath.Join(out, "summary.json"),
		filepath.Join(out, "tasks", "astropy__astropy-14309", "attempt-001", "baseline", "container.log"),
		filepath.Join(out, "tasks", "astropy__astropy-14309", "attempt-001", "mut", "swebench", "predictions.jsonl"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("artifact %s: %v", path, err)
		}
	}
}

func TestSWEBenchFingerprintChangesWithEnvironment(t *testing.T) {
	manifest := &Manifest{Dataset: Dataset{Type: "swebench", Source: "dataset", Tasks: []string{"x"}}}
	instances := []SWEBenchInstance{{InstanceID: "x", BaseCommit: "abc", InstanceImageKey: "image:a"}}
	a, err := swebenchFingerprint(manifest, instances, DockerOptions{InstallCmd: []string{"install", "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := swebenchFingerprint(manifest, instances, DockerOptions{InstallCmd: []string{"install", "v2"}})
	instances[0].InstanceImageKey = "image:b"
	c, _ := swebenchFingerprint(manifest, instances, DockerOptions{InstallCmd: []string{"install", "v1"}})
	if a == b || a == c || len(a) != 64 {
		t.Fatalf("fingerprints a=%s b=%s c=%s", a, b, c)
	}
}
