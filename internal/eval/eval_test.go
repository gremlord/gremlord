package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/store"
	"github.com/gremlord/gremlord/internal/wire"
)

func TestLoadManifestStrictAndValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.yaml")
	valid := `version: 1
name: smoke
tasks:
  - id: one
    repo: /tmp/repo
    prompt: fix it
    verifier:
      run: [go, test, ./...]
      timeout: 2m
`
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Tasks[0].Verifier.Timeout != 2*time.Minute {
		t.Fatalf("timeout = %v", m.Tasks[0].Verifier.Timeout)
	}

	invalid := strings.Replace(valid, "name: smoke", "unknown: value\nname: smoke", 1)
	if err := os.WriteFile(path, []byte(invalid), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadDatasetManifestAndFilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swebench.yaml")
	manifest := `version: 1
name: swebench-smoke
dataset:
  type: swebench
  source: princeton-nlp/SWE-bench_Verified
  tasks:
    - astropy__astropy-14309
    - django__django-11001
sandbox:
  type: docker
`
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsDataset() || m.Dataset.Type != "swebench" || m.Dataset.Source != "princeton-nlp/SWE-bench_Verified" || m.Sandbox.Type != "docker" {
		t.Fatalf("manifest = %+v", m)
	}
	if err := FilterTasks(m, []string{"astropy__astropy-14309"}); err != nil {
		t.Fatal(err)
	}
	if len(m.Dataset.Tasks) != 1 || m.Dataset.Tasks[0] != "astropy__astropy-14309" {
		t.Fatalf("filtered dataset tasks = %+v", m.Dataset.Tasks)
	}
}

func TestDatasetManifestValidation(t *testing.T) {
	base := func() *Manifest {
		return &Manifest{Version: SchemaVersion, Name: "swebench-smoke", Dataset: Dataset{
			Type: "swebench", Source: "princeton-nlp/SWE-bench_Verified", Tasks: []string{"astropy__astropy-14309"},
		}, Sandbox: Sandbox{Type: "docker"}}
	}
	if m := base(); m.Validate() != nil {
		t.Fatalf("valid dataset manifest rejected: %v", m.Validate())
	}
	m := base()
	m.Dataset.Type = "other"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported dataset type") {
		t.Fatalf("dataset type error = %v", err)
	}
	m = base()
	m.Dataset.Source = ""
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "dataset.source") {
		t.Fatalf("dataset source error = %v", err)
	}
	m = base()
	m.Dataset.Tasks = nil
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "dataset.tasks") {
		t.Fatalf("dataset tasks error = %v", err)
	}
	m = base()
	m.Dataset.Tasks = []string{"a", "a"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate dataset task") {
		t.Fatalf("duplicate task error = %v", err)
	}
	m = base()
	m.Sandbox.Type = ""
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "sandbox.type: docker") {
		t.Fatalf("sandbox requirement error = %v", err)
	}
	m = base()
	m.Tasks = []Task{{ID: "x", Repo: "/tmp", Prompt: "p", Verifier: Command{Run: []string{"t"}}}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "cannot also define local tasks") {
		t.Fatalf("local task conflict error = %v", err)
	}
	m = base()
	m.Setup = Command{Run: []string{"make"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "cannot define a local setup command") {
		t.Fatalf("local setup conflict error = %v", err)
	}

	local := testManifest("/tmp/repo", "")
	local.Sandbox = Sandbox{Type: "docker"}
	if err := local.Validate(); err == nil || !strings.Contains(err.Error(), "sandbox is only supported alongside dataset") {
		t.Fatalf("local manifest sandbox error = %v", err)
	}
}

func TestDeterministicWinnerRequiresVerifier(t *testing.T) {
	pass := CandidateResult{Verifier: VerifierResult{Ran: true, Passed: true}}
	fail := CandidateResult{Verifier: VerifierResult{Ran: true}}
	skipped := CandidateResult{Status: StatusTimeout, Verifier: VerifierResult{Skipped: "candidate timeout"}}
	if got := deterministicWinner(pass, fail); got != WinnerBaseline {
		t.Errorf("pass vs fail = %q", got)
	}
	if got := deterministicWinner(fail, pass); got != WinnerMUT {
		t.Errorf("fail vs pass = %q", got)
	}
	if got := deterministicWinner(pass, pass); got != WinnerTie {
		t.Errorf("pass vs pass = %q", got)
	}
	if got := deterministicWinner(pass, skipped); got != WinnerBaseline {
		t.Errorf("pass vs skipped = %q", got)
	}
}

func judgeServer(t *testing.T, response string, status int, inspect ...func(*http.Request, []byte)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		for _, fn := range inspect {
			fn(r, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 200 && status < 300 {
			data, _ := json.Marshal(map[string]any{
				"id": "msg_judge", "type": "message", "role": "assistant", "model": "judge-model",
				"content":     []map[string]string{{"type": "text", "text": response}},
				"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
			})
			_, _ = w.Write(data)
			return
		}
		_, _ = w.Write([]byte(response))
	}))
}

type scriptExecutor struct {
	setupErr     error
	claudeErr    error
	claudeStdout string
	claudeDelay  time.Duration
	judgeStdout  string
	judgeErr     error
	judgeDir     string
	verifierErr  error
	verifierRuns int
}

func (s *scriptExecutor) Run(ctx context.Context, dir string, _ []string, argv []string, _ io.Reader, stdout, _ io.Writer) error {
	isClaude := strings.HasSuffix(argv[0], "claude")
	isJudge := isClaude && containsArg(argv, "judge-model")
	switch {
	case isJudge:
		s.judgeDir = dir
		_, _ = stdout.Write([]byte(s.judgeStdout))
		return s.judgeErr
	case isClaude:
		if s.claudeDelay > 0 {
			select {
			case <-time.After(s.claudeDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		_, _ = stdout.Write([]byte(s.claudeStdout))
		return s.claudeErr
	default:
		if containsArg(argv, "setup") {
			return s.setupErr
		}
		s.verifierRuns++
		_, _ = stdout.Write([]byte("verifier ran"))
		return s.verifierErr
	}
}

func containsArg(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "initial")
	return dir
}

func headSHA(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func testManifest(repo, base string) *Manifest {
	return &Manifest{Version: SchemaVersion, Name: "unit", Tasks: []Task{{
		ID: "task-a", Repo: repo, Base: base, Prompt: "do the thing",
		Verifier: Command{Run: []string{"verify"}, Timeout: time.Minute},
	}}}
}

func TestPrepareWorkspaceChecksOutCommitSHA(t *testing.T) {
	repo := gitRepo(t)
	sha := headSHA(t, repo)
	dst := filepath.Join(t.TempDir(), "work")
	if err := prepareWorkspace(context.Background(), repo, sha, dst); err != nil {
		t.Fatal(err)
	}
	if got := headSHA(t, dst); got != sha {
		t.Errorf("checked out %s, want %s", got, sha)
	}
}

func TestCapturePatchIncludesAllCandidateChanges(t *testing.T) {
	repo := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "add", "file.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := capturePatch(context.Background(), repo, headSHA(t, repo), nil)
	for _, want := range []string{"+staged", "diff --git a/untracked.txt b/untracked.txt", "+new"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch missing %q:\n%s", want, patch)
		}
	}
}

func TestCapturePatchIncludesCandidateCommits(t *testing.T) {
	repo := gitRepo(t)
	base := headSHA(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "commit", "-am", "candidate")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	if patch := capturePatch(context.Background(), repo, base, nil); !strings.Contains(patch, "+committed") {
		t.Fatalf("patch omitted committed change:\n%s", patch)
	}
}

func TestCapturePatchExcludesHarnessInstructionRemoval(t *testing.T) {
	repo := gitRepo(t)
	base := headSHA(t, repo)
	instruction := filepath.Join(repo, "CLAUDE.md")
	if err := os.WriteFile(instruction, []byte("harness instruction\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "add", "CLAUDE.md")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	cmd = exec.Command("git", "-C", repo, "commit", "-m", "instructions")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	base = headSHA(t, repo)
	if err := os.Remove(instruction); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := capturePatch(context.Background(), repo, base, []string{"CLAUDE.md"})
	if strings.Contains(patch, "CLAUDE.md") || !strings.Contains(patch, "+candidate") {
		t.Fatalf("patch did not isolate harness removal:\n%s", patch)
	}
}

func TestCandidateArgsDisableNonComparableTools(t *testing.T) {
	args := candidateArgs("claude", "model", "prompt")
	for _, want := range []string{"Task,WebSearch,WebFetch", "--strict-mcp-config", `{"mcpServers":{}}`, "--setting-sources"} {
		if !containsArg(args, want) {
			t.Errorf("candidate args missing %q: %v", want, args)
		}
	}
}

func TestStripHarnessInstructions(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "CLAUDE.md"), filepath.Join(root, "nested", "AGENTS.md"), filepath.Join(root, "nested", ".git", "CLAUDE.md"), filepath.Join(root, "keep.md")} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := stripHarnessInstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v", removed)
	}
	for _, path := range []string{filepath.Join(root, "CLAUDE.md"), filepath.Join(root, "nested", "AGENTS.md")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("instruction file survived: %s", path)
		}
	}
	for _, path := range []string{filepath.Join(root, "nested", ".git", "CLAUDE.md"), filepath.Join(root, "keep.md")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("non-target was removed: %s: %v", path, err)
		}
	}
}

func TestCandidateFailureSkipsVerifier(t *testing.T) {
	repo := gitRepo(t)
	ex := &scriptExecutor{claudeErr: exitError(t), claudeStdout: `{"result":"boom"}`}
	runner := &Runner{Exec: ex, Options: Options{
		Baseline: "a", MUT: "b", Judge: "none", OutputDir: t.TempDir(),
		Timeout: time.Minute, ClaudeBin: "claude",
	}}
	s, err := runner.Run(context.Background(), testManifest(repo, ""))
	if err != nil {
		t.Fatal(err)
	}
	if ex.verifierRuns != 0 {
		t.Errorf("verifier ran %d times", ex.verifierRuns)
	}
	for _, c := range []CandidateResult{s.Pairs[0].Baseline, s.Pairs[0].MUT} {
		if c.Status != StatusModelError || !c.ModelFailed() || c.Verifier.Ran || c.Verifier.Passed {
			t.Errorf("candidate = %+v", c)
		}
	}
}

// A verifier that exits with VerifierInfraExit is saying it could not judge
// the candidate — a missing image or toolchain. Scoring that as a model loss
// blames the model for our machine.
// Two runs comparing different models at the same seed must not share a
// session id: the usage table aggregates by it, so a collision merges their
// spend into one figure that reads as a per-model result.
func TestSessionIDSeparatesModels(t *testing.T) {
	a := sessionID(1, "task-a", 1, "mut", "grok")
	b := sessionID(1, "task-a", 1, "mut", "glm53")
	if a == b {
		t.Fatalf("different models share session id %q", a)
	}
	if same := sessionID(1, "task-a", 1, "mut", "grok"); same != a {
		t.Errorf("session id is not stable: %q then %q", a, same)
	}
	if other := sessionID(1, "task-a", 1, "baseline", "grok"); other == a {
		t.Error("baseline and mut arms of the same model share a session id")
	}
	if other := sessionID(2, "task-a", 1, "mut", "grok"); other == a {
		t.Error("the seed no longer affects the session id")
	}
}

func TestVerifierInfraExitIsNotAModelLoss(t *testing.T) {
	repo := gitRepo(t)
	ex := &scriptExecutor{
		claudeStdout: `{"result":"done"}`,
		verifierErr:  exitCodeError(t, VerifierInfraExit),
	}
	runner := &Runner{Exec: ex, Options: Options{
		Baseline: "a", MUT: "b", Judge: "none", OutputDir: t.TempDir(),
		Timeout: time.Minute, ClaudeBin: "claude",
	}}
	s, err := runner.Run(context.Background(), testManifest(repo, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []CandidateResult{s.Pairs[0].Baseline, s.Pairs[0].MUT} {
		if c.Status != StatusVerifierError {
			t.Errorf("status = %q, want %q", c.Status, StatusVerifierError)
		}
		if !c.InfraFailed() || c.ModelFailed() {
			t.Errorf("candidate = %+v, want infra failure and not a model failure", c)
		}
	}
	if s.Pairs[0].Winner != WinnerInfraError {
		t.Errorf("winner = %q, want %q", s.Pairs[0].Winner, WinnerInfraError)
	}
}

// An ordinary non-zero verifier exit stays a candidate loss.
func TestVerifierFailureIsStillAModelLoss(t *testing.T) {
	repo := gitRepo(t)
	ex := &scriptExecutor{claudeStdout: `{"result":"done"}`, verifierErr: exitCodeError(t, 1)}
	runner := &Runner{Exec: ex, Options: Options{
		Baseline: "a", MUT: "b", Judge: "none", OutputDir: t.TempDir(),
		Timeout: time.Minute, ClaudeBin: "claude",
	}}
	s, err := runner.Run(context.Background(), testManifest(repo, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []CandidateResult{s.Pairs[0].Baseline, s.Pairs[0].MUT} {
		if c.Status != StatusComplete || c.InfraFailed() || c.Verifier.Passed {
			t.Errorf("candidate = %+v, want a completed run with a failed verifier", c)
		}
	}
}

func TestInfraFailureSuppressesJudgeAndIsNotTie(t *testing.T) {
	repo := gitRepo(t)
	ex := &scriptExecutor{
		setupErr:    exitError(t),
		judgeStdout: `{"winner":"candidate_1","confidence":1,"reason":"bad","candidate_1_score":5,"candidate_2_score":1}`,
	}
	manifest := testManifest(repo, "")
	manifest.Setup = Command{Run: []string{"setup"}}
	runner := &Runner{Exec: ex, Options: Options{
		Baseline: "a", MUT: "b", Judge: "judge-model", OutputDir: t.TempDir(),
		Timeout: time.Minute, ClaudeBin: "claude",
	}}
	s, err := runner.Run(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pairs) != 1 || s.Pairs[0].Winner != WinnerInfraError {
		t.Fatalf("pairs = %+v", s.Pairs)
	}
	if s.Pairs[0].Judge != nil || s.Pairs[0].JudgeError != "" {
		t.Fatalf("judge ran or errored: %+v", s.Pairs[0])
	}
	if s.InfraPairs != 1 || s.InfraFailures != 1 || s.Ties != 0 || s.JudgeErrors != 0 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestInfraFailureSuppressesJudgeWhenOnlyOneCandidateFails(t *testing.T) {
	repo := gitRepo(t)
	ex := &selectiveSetupExecutor{scriptExecutor: scriptExecutor{
		setupErr:     exitError(t),
		claudeStdout: `{"result":"done"}`,
		judgeStdout:  `{"winner":"candidate_1","confidence":1,"reason":"bad","candidate_1_score":5,"candidate_2_score":1}`,
	}}
	manifest := testManifest(repo, "")
	manifest.Setup = Command{Run: []string{"setup"}}
	runner := &Runner{Exec: ex, Options: Options{
		Baseline: "a", MUT: "b", Judge: "judge-model", OutputDir: t.TempDir(),
		Timeout: time.Minute, ClaudeBin: "claude",
	}}
	s, err := runner.Run(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Pairs[0]
	if p.Winner != WinnerInfraError || p.Judge != nil || p.JudgeError != "" || ex.judgeRuns != 0 {
		t.Fatalf("pair = %+v, judge runs = %d", p, ex.judgeRuns)
	}
	infra, completed := p.Baseline, p.MUT
	if !infra.InfraFailed() {
		infra, completed = completed, infra
	}
	if !infra.InfraFailed() || completed.InfraFailed() || !completed.Verifier.Passed {
		t.Fatalf("candidates = baseline %+v, mut %+v", p.Baseline, p.MUT)
	}
}

type selectiveSetupExecutor struct {
	scriptExecutor
	setupRuns int
	judgeRuns int
}

func (s *selectiveSetupExecutor) Run(ctx context.Context, dir string, env []string, argv []string, input io.Reader, stdout, stderr io.Writer) error {
	if strings.HasSuffix(argv[0], "claude") && containsArg(argv, "judge-model") {
		s.judgeRuns++
	}
	if containsArg(argv, "setup") {
		s.setupRuns++
		if s.setupRuns > 1 {
			return nil
		}
	}
	return s.scriptExecutor.Run(ctx, dir, env, argv, input, stdout, stderr)
}

func TestAggregateCountsSingleInfraPairForTwoFailedCandidates(t *testing.T) {
	s := &Summary{Pairs: []PairResult{{
		Winner:   WinnerInfraError,
		Baseline: CandidateResult{Status: StatusSetupError},
		MUT:      CandidateResult{Status: StatusWorkspaceError},
	}}}
	s.aggregate()
	if s.InfraPairs != 1 || s.InfraFailures != 1 || s.Ties != 0 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestJudgeUsesDirectBlindedMessagesRequest(t *testing.T) {
	repo := gitRepo(t)
	ex := &scriptExecutor{claudeStdout: `{"result":"done"}`}
	judge := judgeServer(t, `{"winner":"tie","confidence":1,"reason":"equal","candidate_1_score":3,"candidate_2_score":3}`, http.StatusOK,
		func(req *http.Request, body []byte) {
			if req.URL.Path != "/v1/messages" || req.Header.Get("x-api-key") != "token" || req.Header.Get(wire.HeaderPinModel) != "judge-model" {
				t.Errorf("judge request = %s headers=%v", req.URL.Path, req.Header)
			}
			var message anthropic.MessagesRequest
			if err := json.Unmarshal(body, &message); err != nil {
				t.Error(err)
				return
			}
			text := message.Messages[0].Content[0].Text
			if message.Model != "judge-model" || !strings.Contains(text, "Candidate identities and order are randomized") || strings.Contains(text, "Model: a") || strings.Contains(text, "Model: b") {
				t.Errorf("unblinded or malformed judge request: %+v", message)
			}
		})
	defer judge.Close()
	out := t.TempDir()
	runner := &Runner{Exec: ex, Options: Options{
		Baseline: "a", MUT: "b", Judge: "judge-model", OutputDir: out, BaseURL: judge.URL, Token: "token", Profile: "main",
		Timeout: time.Minute, ClaudeBin: "claude",
	}}
	if _, err := runner.Run(context.Background(), testManifest(repo, "")); err != nil {
		t.Fatal(err)
	}
	if ex.judgeDir != "" {
		t.Fatalf("judge unexpectedly ran through candidate executor in %q", ex.judgeDir)
	}
	workDir := filepath.Join(out, "tasks", "task-a", "attempt-001", "judge", "workspace")
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("judge workspace contained identity-leaking artifacts: %v", entries)
	}
}

func TestJudgeFailureIsRecordedAndRunContinues(t *testing.T) {
	repo := gitRepo(t)
	ex := &scriptExecutor{claudeStdout: `{"result":"done"}`}
	judge := judgeServer(t, "not json", http.StatusOK)
	defer judge.Close()
	runner := &Runner{Exec: ex, Options: Options{
		Baseline: "a", MUT: "b", Judge: "judge-model", OutputDir: t.TempDir(), BaseURL: judge.URL,
		Timeout: time.Minute, ClaudeBin: "claude", Attempts: 2,
	}}
	s, err := runner.Run(context.Background(), testManifest(repo, ""))
	if err != nil {
		t.Fatalf("judge error aborted run: %v", err)
	}
	if len(s.Pairs) != 2 || s.JudgeErrors != 2 || s.Ties != 0 {
		t.Fatalf("summary = %+v", s)
	}
	for _, p := range s.Pairs {
		if p.Winner != WinnerJudgeError || p.JudgeError == "" {
			t.Errorf("pair = %+v", p)
		}
	}
}

func TestTelemetryPopulatesUsageAndRoutes(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "agentic.db"))
	if err != nil {
		t.Fatal(err)
	}
	sid := sessionID(7, "task-a", 1, "baseline", "opus")
	now := time.Now().Truncate(time.Second)
	if err := st.RecordUsage(store.UsageEvent{TS: now, SessionID: sid, InputTokens: 100, OutputTokens: 20, CostUSD: 0.25, DurationMS: 900, Status: 200}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRouteDecision(sid, "auto", "deep", "opus", "", now); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	r := &Runner{Options: Options{DataDir: dataDir}}
	usage, trace := r.telemetry(sid)
	if usage.Requests != 1 || usage.InputTokens != 100 || usage.CostUSD != 0.25 || usage.DurationMS != 900 {
		t.Errorf("usage = %+v", usage)
	}
	if len(trace) != 1 || trace[0].Model != "opus" || trace[0].Tier != "deep" {
		t.Errorf("trace = %+v", trace)
	}
}

func TestEvalEnvRoutesAndAttributes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	env := evalEnv([]string{"ANTHROPIC_API_KEY=bad"}, Options{BaseURL: "http://router", Token: "token", Profile: "main"}, "eval-1", "gpt-5.6-sol", home)
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"HOME=" + home, "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
		"ANTHROPIC_BASE_URL=http://router", "ANTHROPIC_AUTH_TOKEN=token",
		"X-Agentic-Session: eval-1", "AGENTIC_SESSION_ID=eval-1",
		"X-Agentic-Pin-Model: gpt-5.6-sol",
		"ANTHROPIC_MODEL=gpt-5.6-sol",
		"ANTHROPIC_SMALL_FAST_MODEL=gpt-5.6-sol",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=gpt-5.6-sol",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=gpt-5.6-sol",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=gpt-5.6-sol",
		"CLAUDE_CODE_SUBAGENT_MODEL=gpt-5.6-sol",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "ANTHROPIC_API_KEY=") {
		t.Errorf("api key was not removed")
	}
}

func TestFinalText(t *testing.T) {
	if got := finalText([]byte(`{"result":"hello"}`)); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := finalText(bytes.TrimSpace([]byte(" plain "))); got != "plain" {
		t.Fatalf("got %q", got)
	}
}

func TestCandidateArtifactIsDurable(t *testing.T) {
	repo := gitRepo(t)
	out := t.TempDir()
	runner := &Runner{Exec: &scriptExecutor{claudeStdout: `{"result":"done"}`}, Options: Options{
		Baseline: "a", MUT: "b", Judge: "none", OutputDir: out,
		Timeout: time.Minute, ClaudeBin: "claude",
	}}
	if _, err := runner.Run(context.Background(), testManifest(repo, "")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "tasks", "task-a", "attempt-001", "baseline", "candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result CandidateResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusComplete || !result.Verifier.Ran || !result.Verifier.Passed {
		t.Errorf("result = %+v", result)
	}
}

func exitCodeError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("expected exit %d", code)
	}
	return err
}

func exitError(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 3").Run()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	return err
}

// Interactive sessions export ENABLE_TOOL_SEARCH=true, and every Bash command
// they run inherits it — so an eval launched from inside a session would defer
// tool schemas while the same command from a plain terminal would not. Whether
// a run is comparable to the one stored beside it must not depend on where it
// was invoked from.
func TestEvalEnvPinsToolSearch(t *testing.T) {
	for _, inherited := range []string{"ENABLE_TOOL_SEARCH=true", "ENABLE_TOOL_SEARCH=auto:50", ""} {
		var in []string
		if inherited != "" {
			in = []string{inherited}
		}
		env := evalEnv(in, Options{BaseURL: "http://router", Token: "token", Profile: "main"}, "eval-1", "glm53")
		joined := strings.Join(env, "\n")
		if !strings.Contains(joined, "ENABLE_TOOL_SEARCH=false") {
			t.Errorf("with %q inherited, env did not pin tool search: %s", inherited, joined)
		}
		if strings.Contains(joined, "ENABLE_TOOL_SEARCH=true") || strings.Contains(joined, "ENABLE_TOOL_SEARCH=auto") {
			t.Errorf("with %q inherited, the inherited value survived: %s", inherited, joined)
		}
	}
}
