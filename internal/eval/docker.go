// Package eval's Docker support runs one Claude Code candidate inside a
// disposable container built from the official SWE-bench instance image, so
// the model edits inside the exact environment the official grader will
// later check the patch against. It owns container lifecycle only —
// dataset loading, image identity, and grading remain in swebench.go's
// bridge to the official harness.
package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/wire"
)

// ClaudeCodeContainerVersion pins the official Linux x64 Claude Code native
// package installed into SWE-bench's x86_64 candidate images. Pinning it keeps
// repeated benchmark runs comparable and avoids the native installer's own
// short download timeout under Apple Silicon emulation.
const ClaudeCodeContainerVersion = "2.1.220"

// DockerOptions configures the SWE-bench candidate container lifecycle.
type DockerOptions struct {
	// DockerBin is the docker CLI to invoke. Defaults to "docker".
	DockerBin string
	// PullTimeout bounds `docker run`, which pulls the instance image if it
	// is not already present locally. Defaults to 20m.
	PullTimeout time.Duration
	// InstallTimeout bounds the one-time Claude Code install inside the
	// fresh container. Defaults to 5m.
	InstallTimeout time.Duration
	// ExecTimeout bounds a single `docker exec`, other than the candidate's
	// own Claude Code turn (which uses the Runner's shared Options.Timeout).
	// Defaults to 3m.
	ExecTimeout time.Duration
	// InstallCmd overrides how Claude Code is installed inside the
	// container. Defaults to the official native installer, which needs no
	// Node.js or other runtime — appropriate for a minimal instance image.
	InstallCmd []string
	// KeepContainers skips removal after the run, leaving the candidate's
	// idle container running for debugging by hand.
	KeepContainers bool
}

func (o DockerOptions) dockerBin() string {
	if o.DockerBin == "" {
		return "docker"
	}
	return o.DockerBin
}

func (o DockerOptions) pullTimeout() time.Duration {
	if o.PullTimeout <= 0 {
		return 20 * time.Minute
	}
	return o.PullTimeout
}

func (o DockerOptions) installTimeout() time.Duration {
	if o.InstallTimeout <= 0 {
		return 5 * time.Minute
	}
	return o.InstallTimeout
}

func (o DockerOptions) execTimeout() time.Duration {
	if o.ExecTimeout <= 0 {
		return 3 * time.Minute
	}
	return o.ExecTimeout
}

func (o DockerOptions) installCmd() []string {
	if len(o.InstallCmd) > 0 {
		return o.InstallCmd
	}
	url := fmt.Sprintf("https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/-/claude-code-linux-x64-%s.tgz", ClaudeCodeContainerVersion)
	return []string{"bash", "-lc", fmt.Sprintf("curl -fsSL %q | tar -xz -C /usr/local/bin --strip-components=1 package/claude && chmod 0755 /usr/local/bin/claude", url)}
}

// dockerWorkdir is the working directory the official instance image
// already checks the repository out into (swebench.harness.dockerfiles: the
// instance and env Dockerfile stages both set `WORKDIR /testbed/`).
const dockerWorkdir = "/testbed"

// DockerContainerEnv is what a candidate's Claude Code process needs to
// reach the host router through the eval relay and be attributed correctly.
type DockerContainerEnv struct {
	BaseURL   string // relay's container-reachable URL, e.g. http://host.docker.internal:PORT
	Token     string
	SessionID string
	Profile   string
	Model     string
}

func containerEnvArgs(e DockerContainerEnv) []string {
	return []string{
		"HOME=/home/nonroot",
		"USER=nonroot",
		"ANTHROPIC_BASE_URL=" + e.BaseURL,
		"ANTHROPIC_AUTH_TOKEN=" + e.Token,
		"ANTHROPIC_CUSTOM_HEADERS=" + wire.HeaderSession + ": " + e.SessionID +
			"\n" + wire.HeaderProfile + ": " + e.Profile +
			"\n" + wire.HeaderPinModel + ": " + e.Model +
			"\n" + wire.LegacyHeaderSession + ": " + e.SessionID +
			"\n" + wire.LegacyHeaderProfile + ": " + e.Profile +
			"\n" + wire.LegacyHeaderPinModel + ": " + e.Model,
		config.EnvName("SESSION_ID") + "=" + e.SessionID,
		config.EnvName("PROFILE") + "=" + e.Profile,
		"AGENTIC_SESSION_ID=" + e.SessionID,
		"AGENTIC_PROFILE=" + e.Profile,
		// Pin all tier fallbacks to the candidate model so subagent spawns don't escape
		// to Claude defaults (opus/sonnet).
		"ANTHROPIC_MODEL=" + e.Model,
		"ANTHROPIC_SMALL_FAST_MODEL=" + e.Model,
		"ANTHROPIC_DEFAULT_OPUS_MODEL=" + e.Model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL=" + e.Model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + e.Model,
		"CLAUDE_CODE_SUBAGENT_MODEL=" + e.Model,
	}
}

// DockerCandidateResult is what running one candidate in a container
// produces: the patch to grade, plus diagnostics. Status/Error follow the
// same convention as CandidateResult so callers can map them directly.
type DockerCandidateResult struct {
	Status     string
	Error      string
	ExitCode   int
	DurationMS int64
	Stdout     []byte
	Stderr     []byte
	Patch      string
	// ContainerLog is a combined, human-readable transcript of every docker
	// invocation for this candidate (run, install, exec, rm), for artifact
	// diagnostics independent of stdout/stderr capture above.
	ContainerLog []byte
	ContainerID  string
	Image        string
}

// RunDockerCandidate starts a fresh container from the official instance
// image, installs Claude Code, runs one non-interactive turn against the
// hydrated problem statement, extracts the resulting patch, and always
// removes the container (unless DockerOptions.KeepContainers is set).
func RunDockerCandidate(ctx context.Context, opts DockerOptions, instance SWEBenchInstance, prompt, model string, claudeTimeout time.Duration, containerEnv DockerContainerEnv) (res DockerCandidateResult) {
	var log bytes.Buffer
	logf := func(format string, args ...any) { fmt.Fprintf(&log, format+"\n", args...) }

	res = DockerCandidateResult{ExitCode: -1, Image: instance.InstanceImageKey}
	start := time.Now()
	defer func() {
		res.DurationMS = time.Since(start).Milliseconds()
		res.ContainerLog = append([]byte(nil), log.Bytes()...)
	}()

	if instance.InstanceImageKey == "" {
		res.Status, res.Error = StatusDockerError, "swebench: instance has no resolved image key"
		res.ContainerLog = log.Bytes()
		return res
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.pullTimeout())
	containerID, err := dockerRun(runCtx, opts, instance.InstanceImageKey, &log)
	cancel()
	if err != nil {
		res.Status, res.Error = StatusDockerError, "docker run: "+err.Error()
		res.ContainerLog = log.Bytes()
		return res
	}
	res.ContainerID = containerID
	logf("container %s started from %s", containerID, instance.InstanceImageKey)

	defer func() {
		if opts.KeepContainers {
			logf("keeping container %s for inspection", containerID)
			return
		}
		rmCtx, rmCancel := context.WithTimeout(context.Background(), opts.execTimeout())
		defer rmCancel()
		if _, _, err := dockerExec(rmCtx, opts, nil, []string{"true"}, &log, true, containerID); err != nil {
			// best-effort liveness probe only; removal below is unconditional
			_ = err
		}
		if err := dockerRemove(rmCtx, opts, containerID); err != nil {
			logf("docker rm %s: %v", containerID, err)
		}
	}()

	installCtx, installCancel := context.WithTimeout(ctx, opts.installTimeout())
	claudePath, err := installClaudeCode(installCtx, opts, containerID, &log)
	installCancel()
	if err != nil {
		res.Status, res.Error = StatusDockerError, "claude code install: "+err.Error()
		res.ContainerLog = log.Bytes()
		return res
	}
	logf("claude code installed at %s", claudePath)

	argv := []string{claudePath, "--print", "--output-format", "json", "--permission-mode", "bypassPermissions", "--disallowedTools", "Task", "--model", model, prompt}
	env := containerEnvArgs(containerEnv)
	turnCtx, turnCancel := context.WithTimeout(ctx, claudeTimeout)
	stdout, stderr, err := dockerExecUser(turnCtx, opts, env, argv, &log, false, containerID, "nonroot")
	turnCancel()
	res.Stdout, res.Stderr = stdout, stderr
	res.ExitCode = dockerExitCode(err)
	switch {
	case errors.Is(turnCtx.Err(), context.DeadlineExceeded):
		res.Status, res.Error = StatusTimeout, "candidate exceeded "+claudeTimeout.String()
	case err != nil:
		res.Status, res.Error = StatusModelError, err.Error()
	default:
		res.Status = StatusComplete
	}

	diffCtx, diffCancel := context.WithTimeout(ctx, opts.execTimeout())
	patch, _, perr := dockerExec(diffCtx, opts, nil, []string{"git", "diff", "--binary", "--no-ext-diff"}, &log, false, containerID)
	diffCancel()
	if perr != nil {
		logf("git diff: %v", perr)
	}
	res.Patch = string(patch)
	res.ContainerLog = log.Bytes()
	return res
}

func dockerRun(ctx context.Context, opts DockerOptions, image string, log *bytes.Buffer) (string, error) {
	args := []string{"run", "-d", "--rm=false", "-w", dockerWorkdir, image, "tail", "-f", "/dev/null"}
	out, err := dockerCommand(ctx, opts, args, log)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("docker run produced no container id")
	}
	return id, nil
}

func dockerRemove(ctx context.Context, opts DockerOptions, containerID string) error {
	_, err := dockerCommand(ctx, opts, []string{"rm", "-f", containerID}, nil)
	return err
}

// dockerExec runs argv inside containerID with the given extra env vars
// (each "KEY=value"), directly — no shell — so prompt text containing
// quotes or special characters needs no escaping. probe suppresses log
// output for internal liveness checks.
func dockerExec(ctx context.Context, opts DockerOptions, env []string, argv []string, log *bytes.Buffer, probe bool, containerID string) ([]byte, []byte, error) {
	return dockerExecUser(ctx, opts, env, argv, log, probe, containerID, "")
}

func dockerExecUser(ctx context.Context, opts DockerOptions, env []string, argv []string, log *bytes.Buffer, probe bool, containerID, user string) ([]byte, []byte, error) {
	args := []string{"exec", "-w", dockerWorkdir}
	if user != "" {
		args = append(args, "-u", user)
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, containerID)
	args = append(args, argv...)
	var l *bytes.Buffer
	if !probe {
		l = log
	}
	out, err := dockerCommandSplit(ctx, opts, args, l)
	return out.stdout, out.stderr, err
}

func installClaudeCode(ctx context.Context, opts DockerOptions, containerID string, log *bytes.Buffer) (string, error) {
	if _, _, err := dockerExec(ctx, opts, nil, opts.installCmd(), log, false, containerID); err != nil {
		return "", err
	}
	out, _, err := dockerExec(ctx, opts, nil, []string{"bash", "-lc", "command -v claude"}, log, false, containerID)
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("claude binary not found on PATH after install: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

type dockerOutput struct {
	stdout, stderr []byte
}

func dockerCommand(ctx context.Context, opts DockerOptions, args []string, log *bytes.Buffer) ([]byte, error) {
	out, err := dockerCommandSplit(ctx, opts, args, log)
	return out.stdout, err
}

func dockerCommandSplit(ctx context.Context, opts DockerOptions, args []string, log *bytes.Buffer) (dockerOutput, error) {
	cmd := exec.CommandContext(ctx, opts.dockerBin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if log != nil {
		fmt.Fprintf(log, "$ docker %s\n", strings.Join(redactDockerArgs(args), " "))
		if stdout.Len() > 0 {
			fmt.Fprintf(log, "%s\n", stdout.String())
		}
		if stderr.Len() > 0 {
			fmt.Fprintf(log, "stderr: %s\n", stderr.String())
		}
		if err != nil {
			fmt.Fprintf(log, "error: %v\n", err)
		}
	}
	return dockerOutput{stdout: stdout.Bytes(), stderr: stderr.Bytes()}, err
}

// redactDockerArgs hides -e ANTHROPIC_AUTH_TOKEN=... and any other env value
// from the human-readable container log; the raw token must not land in an
// artifact a manifest reviewer or CI log viewer might read.
func redactDockerArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out); i++ {
		if out[i] == "-e" && i+1 < len(out) {
			if k, _, ok := strings.Cut(out[i+1], "="); ok {
				out[i+1] = k + "=<redacted>"
			}
		}
	}
	return out
}

func dockerExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *exec.ExitError
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}

// writeDockerArtifacts persists the container transcript and metadata a
// SWE-bench candidate produced, alongside the same patch.diff/candidate.json
// files a local candidate writes.
func writeDockerArtifacts(dir string, r DockerCandidateResult) {
	os.WriteFile(filepath.Join(dir, "container.log"), r.ContainerLog, 0o644)
	meta := struct {
		Image       string `json:"image"`
		ContainerID string `json:"container_id"`
	}{Image: r.Image, ContainerID: r.ContainerID}
	writeJSONAtomic(filepath.Join(dir, "docker.json"), meta)
}

// prepareSWEBench validates prerequisites, hydrates the selected official
// instances once, records their exact metadata for reproducibility, and starts
// the one temporary relay every candidate container shares.
func (r *Runner) prepareSWEBench(ctx context.Context, manifest *Manifest) ([]Task, string, error) {
	if _, err := CheckSWEBench(ctx, r.Options.SWEBench); err != nil {
		return nil, "", err
	}
	bridgeDir := filepath.Join(r.Options.OutputDir, "swebench-bridge")
	instances, err := ResolveSWEBench(ctx, r.Options.SWEBench, SWEBenchResolveOptions{
		Dataset: manifest.Dataset.Source, Split: manifest.Dataset.Split,
		InstanceIDs: manifest.Dataset.Tasks, WorkDir: bridgeDir,
	})
	if err != nil {
		return nil, "", err
	}
	if len(instances) == 0 {
		return nil, "", errors.New("swebench: dataset resolved no instances")
	}

	r.swebench = make(map[string]SWEBenchInstance, len(instances))
	for _, instance := range instances {
		r.swebench[instance.InstanceID] = instance
	}
	ordered := make([]SWEBenchInstance, 0, len(manifest.Dataset.Tasks))
	tasks := make([]Task, 0, len(manifest.Dataset.Tasks))
	for _, id := range manifest.Dataset.Tasks {
		instance := r.swebench[id]
		ordered = append(ordered, instance)
		tasks = append(tasks, Task{ID: instance.InstanceID, Prompt: instance.ProblemStatement})
	}
	if err := EnsureSWEBenchImages(ctx, r.Options.SWEBench, SWEBenchResolveOptions{
		Dataset: manifest.Dataset.Source, Split: manifest.Dataset.Split,
		InstanceIDs: manifest.Dataset.Tasks, WorkDir: bridgeDir,
	}); err != nil {
		return nil, "", err
	}
	fingerprint, err := swebenchFingerprint(manifest, ordered, r.Options.Docker)
	if err != nil {
		return nil, "", err
	}

	r.relay, err = StartRelay(ctx, r.Options.BaseURL, r.Options.Token)
	if err != nil {
		return nil, "", err
	}
	return tasks, fingerprint, nil
}

func (r *Runner) writeSWEBenchEnvironment(manifest *Manifest, tasks []Task, fingerprint string) error {
	instances := make([]SWEBenchInstance, 0, len(tasks))
	for _, task := range tasks {
		instances = append(instances, r.swebench[task.ID])
	}
	if err := writeJSONAtomic(filepath.Join(r.Options.OutputDir, "dataset.json"), struct {
		Type        string             `json:"type"`
		Source      string             `json:"source"`
		Split       string             `json:"split"`
		Harness     string             `json:"swebench_version"`
		Fingerprint string             `json:"fingerprint"`
		Instances   []SWEBenchInstance `json:"instances"`
	}{
		Type: manifest.Dataset.Type, Source: manifest.Dataset.Source, Split: datasetSplit(manifest),
		Harness: SWEBenchPackageVersion, Fingerprint: fingerprint, Instances: instances,
	}); err != nil {
		return err
	}
	containerURL := ""
	if r.relay != nil {
		containerURL = r.relay.ContainerURL
	}
	return writeJSONAtomic(filepath.Join(r.Options.OutputDir, "environment.json"), struct {
		GOOS          string   `json:"goos"`
		GOARCH        string   `json:"goarch"`
		DockerBin     string   `json:"docker_bin"`
		ClaudeInstall []string `json:"claude_install"`
		RouterRelay   string   `json:"router_relay"`
	}{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, DockerBin: r.Options.Docker.dockerBin(),
		ClaudeInstall: r.Options.Docker.installCmd(), RouterRelay: containerURL,
	})
}

func datasetSplit(manifest *Manifest) string {
	if manifest.Dataset.Split == "" {
		return SWEBenchDefaultSplit
	}
	return manifest.Dataset.Split
}

func swebenchFingerprint(manifest *Manifest, instances []SWEBenchInstance, docker DockerOptions) (string, error) {
	payload := struct {
		Dataset          Dataset            `json:"dataset"`
		Instances        []SWEBenchInstance `json:"instances"`
		HarnessVersion   string             `json:"harness_version"`
		InstallCmd       []string           `json:"install_cmd"`
		GraderCacheLevel string             `json:"grader_cache_level"`
		GraderClean      bool               `json:"grader_clean"`
	}{
		Dataset: manifest.Dataset, Instances: instances, HarnessVersion: SWEBenchPackageVersion,
		InstallCmd: docker.installCmd(), GraderCacheLevel: "instance", GraderClean: false,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// runSWEBenchCandidate performs the full candidate arm: fresh official image
// container → Claude Code turn → patch extraction → official SWE-bench grade.
// Infrastructure failures are returned as CandidateResult statuses (not Go
// errors) so the paired run can checkpoint and suppress its judge cleanly.
func (r *Runner) runSWEBenchCandidate(ctx context.Context, manifest *Manifest, task Task, attempt int, label, model, dir string) (CandidateResult, error) {
	res := CandidateResult{Label: label, Model: model, SessionID: sessionID(r.Options.Seed, task.ID, attempt, label, model), ExitCode: -1}
	if err := os.RemoveAll(dir); err != nil {
		return res, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	instance, ok := r.swebench[task.ID]
	if !ok {
		res.Status, res.Error = StatusWorkspaceError, "swebench: resolved instance metadata missing"
		res.Verifier = VerifierResult{ExitCode: -1, Skipped: "instance metadata missing"}
		return r.finishSWEBenchCandidate(dir, res)
	}
	if r.relay == nil {
		res.Status, res.Error = StatusDockerError, "swebench: container relay is not running"
		res.Verifier = VerifierResult{ExitCode: -1, Skipped: "container relay unavailable"}
		return r.finishSWEBenchCandidate(dir, res)
	}

	run := RunDockerCandidate(ctx, r.Options.Docker, instance, task.Prompt, model, r.Options.Timeout, DockerContainerEnv{
		BaseURL: r.relay.ContainerURL, Token: r.Options.Token, SessionID: res.SessionID, Profile: r.Options.Profile, Model: model,
	})
	res.Status, res.Error, res.ExitCode, res.DurationMS = run.Status, run.Error, run.ExitCode, run.DurationMS
	res.FinalText = finalText(run.Stdout)
	res.Patch = run.Patch
	os.WriteFile(filepath.Join(dir, "claude.stdout.json"), run.Stdout, 0o644)
	os.WriteFile(filepath.Join(dir, "claude.stderr.log"), run.Stderr, 0o644)
	os.WriteFile(filepath.Join(dir, "patch.diff"), []byte(run.Patch), 0o644)
	writeDockerArtifacts(dir, run)

	if res.ModelFailed() || res.InfraFailed() {
		res.Verifier = VerifierResult{ExitCode: -1, Skipped: "candidate " + res.Status}
		return r.finishSWEBenchCandidate(dir, res)
	}
	if strings.TrimSpace(res.Patch) == "" {
		// An empty patch is a valid candidate outcome (not infrastructure): the
		// official grader accepts predictions but its schema requires a patch
		// string. Record a deterministic failure without spending a grader
		// container on a no-op.
		res.Verifier = VerifierResult{Ran: true, Passed: false, ExitCode: 0, Skipped: "candidate produced an empty patch",
			SWEBench: &SWEBenchVerdict{Resolved: false, PatchApplied: false}}
		return r.finishSWEBenchCandidate(dir, res)
	}

	graderDir := filepath.Join(dir, "swebench")
	if err := os.MkdirAll(graderDir, 0o755); err != nil {
		return res, err
	}
	gradeStart := time.Now()
	grade, err := RunSWEBench(ctx, r.Options.SWEBench, []SWEBenchPrediction{{
		InstanceID: task.ID, ModelNameOrPath: fmt.Sprintf("gremlord/%s/%s", label, model), ModelPatch: res.Patch,
	}}, SWEBenchOptions{
		Dataset: manifest.Dataset.Source, Split: datasetSplit(manifest),
		RunID:     fmt.Sprintf("gremlord-%d-%s-%03d-%s", r.Options.Seed, task.ID, attempt, label),
		ModelName: fmt.Sprintf("gremlord/%s/%s", label, model), InstanceIDs: []string{task.ID},
		// Keep the official instance image between the two paired arms. With
		// clean=true the first arm's grader removes it, making the second arm
		// attempt an impossible registry pull for a locally-built image.
		// run_evaluation still removes each grading container; this retains
		// only the reusable official image cache.
		MaxWorkers: 1, Timeout: r.Options.Timeout, CacheLevel: "instance", Clean: false,
		ReportDir: graderDir, WorkDir: graderDir,
	})
	os.WriteFile(filepath.Join(graderDir, "stdout.log"), []byte(grade.Stdout), 0o644)
	os.WriteFile(filepath.Join(graderDir, "stderr.log"), []byte(grade.Stderr), 0o644)
	if err != nil {
		res.Status, res.Error = StatusGradingError, err.Error()
		res.Verifier = VerifierResult{ExitCode: -1, DurationMS: time.Since(gradeStart).Milliseconds(), Skipped: "official grader failed", Error: err.Error()}
		return r.finishSWEBenchCandidate(dir, res)
	}
	report, ok := grade.Instances[task.ID]
	if !ok {
		res.Status, res.Error = StatusGradingError, "official grader returned no per-instance report"
		res.Verifier = VerifierResult{ExitCode: -1, DurationMS: time.Since(gradeStart).Milliseconds(), Skipped: "official grader report missing"}
		return r.finishSWEBenchCandidate(dir, res)
	}
	verdict := &SWEBenchVerdict{Resolved: report.Resolved, PatchApplied: report.PatchSuccessfullyApplied, ReportPath: grade.ReportPath}
	if report.TestsStatus != nil {
		verdict.FailToPassOK = report.TestsStatus.FailToPass.Success
		verdict.FailToPassBad = report.TestsStatus.FailToPass.Failure
		verdict.PassToPassOK = report.TestsStatus.PassToPass.Success
		verdict.PassToPassBad = report.TestsStatus.PassToPass.Failure
	}
	res.Verifier = VerifierResult{
		Ran: true, Passed: report.Resolved, ExitCode: 0,
		DurationMS: time.Since(gradeStart).Milliseconds(), SWEBench: verdict,
		Output: fmt.Sprintf("resolved=%v patch_applied=%v FAIL_TO_PASS=%d/%d PASS_TO_PASS=%d/%d",
			report.Resolved, report.PatchSuccessfullyApplied,
			len(verdict.FailToPassOK), len(verdict.FailToPassOK)+len(verdict.FailToPassBad),
			len(verdict.PassToPassOK), len(verdict.PassToPassOK)+len(verdict.PassToPassBad)),
	}
	return r.finishSWEBenchCandidate(dir, res)
}

func (r *Runner) finishSWEBenchCandidate(dir string, res CandidateResult) (CandidateResult, error) {
	res.Usage, res.RouteTrace = r.telemetry(res.SessionID)
	if err := writeJSONAtomic(filepath.Join(dir, "candidate.json"), res); err != nil {
		return res, err
	}
	return res, nil
}
