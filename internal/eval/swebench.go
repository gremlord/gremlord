// Package eval's SWE-bench support delegates all benchmark semantics —
// dataset loading, official Docker image identity, test-patch application,
// FAIL_TO_PASS/PASS_TO_PASS grading, and report generation — to the pinned
// official swebench Python package via a small repository-owned bridge
// script (swebench_bridge.py). This file only shells out to that bridge and
// parses its JSON; it must never reimplement or guess at harness behavior.
package eval

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// SWEBenchPackageVersion is the exact official `swebench` PyPI release this
// bridge is verified against (swebench.harness.run_evaluation.main and
// swebench.harness.test_spec.test_spec.make_test_spec signatures). Bumping
// this requires re-verifying the bridge against the new release.
const SWEBenchPackageVersion = "4.1.0"

//go:embed swebench_bridge.py
var swebenchBridge []byte

const (
	SWEBenchDefaultSplit = "test"

	swebenchOpCheck       = "check"
	swebenchOpResolve     = "resolve"
	swebenchOpEnsureImage = "ensure_image"
	swebenchOpGrade       = "grade"
)

// SWEBenchEnv describes how to reach the pinned Python bridge.
type SWEBenchEnv struct {
	// Python is the interpreter to run the bridge with. Defaults to "python3".
	Python string
	// BridgePath is the path to swebench_bridge.py.
	BridgePath string
}

func (e SWEBenchEnv) python() string {
	if e.Python == "" {
		return "python3"
	}
	return e.Python
}

func (e SWEBenchEnv) validate() error {
	if e.BridgePath == "" {
		return errors.New("swebench: bridge path is required")
	}
	return nil
}

// MaterializeSWEBenchBridge writes the embedded pinned bridge to outputDir
// and returns an environment ready for Check/Resolve/Grade calls. Keeping the
// bridge embedded makes a released single-file gremlord binary self-contained;
// users only need Python + `swebench==4.1.0` + Docker, not the source tree.
func MaterializeSWEBenchBridge(outputDir, python string) (SWEBenchEnv, error) {
	if outputDir == "" {
		return SWEBenchEnv{}, errors.New("swebench: output directory is required")
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return SWEBenchEnv{}, err
	}
	path, err := filepath.Abs(filepath.Join(outputDir, "swebench_bridge.py"))
	if err != nil {
		return SWEBenchEnv{}, err
	}
	if err := os.WriteFile(path, swebenchBridge, 0o755); err != nil {
		return SWEBenchEnv{}, err
	}
	return SWEBenchEnv{Python: python, BridgePath: path}, nil
}

// SWEBenchCheckResult reports whether the local environment can run the
// SWE-bench Docker adapter, gathered before any model spend.
type SWEBenchCheckResult struct {
	SWEBenchVersion string `json:"swebench_version"`
	SWEBenchOK      bool   `json:"swebench_ok"`
	PythonVersion   string `json:"python_version"`
	Arch            string `json:"arch"`
	DockerOK        bool   `json:"docker_ok"`
	DockerError     string `json:"docker_error"`
	Error           string `json:"error"`
}

// OK reports whether every prerequisite the bridge checked is satisfied.
func (r SWEBenchCheckResult) OK() bool {
	return r.Error == "" && r.SWEBenchOK && r.DockerOK
}

// CheckSWEBench verifies the pinned swebench package, its expected
// run_evaluation API, and Docker availability, without touching any dataset.
// Callers should run this once before spending any model tokens on a
// SWE-bench eval.
func CheckSWEBench(ctx context.Context, env SWEBenchEnv) (SWEBenchCheckResult, error) {
	if err := env.validate(); err != nil {
		return SWEBenchCheckResult{}, err
	}
	var result SWEBenchCheckResult
	if err := runSWEBenchBridge(ctx, env, []string{"--op", swebenchOpCheck}, &result); err != nil {
		return result, err
	}
	if !result.OK() {
		return result, fmt.Errorf("swebench: prerequisites not satisfied: %s", swebenchCheckSummary(result))
	}
	return result, nil
}

func swebenchCheckSummary(r SWEBenchCheckResult) string {
	switch {
	case r.Error != "":
		return r.Error
	case !r.SWEBenchOK:
		return fmt.Sprintf("swebench %s does not match pinned version %s or lacks the expected run_evaluation API", r.SWEBenchVersion, SWEBenchPackageVersion)
	case !r.DockerOK:
		return "docker is not available: " + r.DockerError
	default:
		return "unknown"
	}
}

// SWEBenchInstance is one official task instance's hydrated metadata: the
// problem statement and repository facts Claude Code needs to attempt the
// task, plus the official image identity the Docker candidate runner uses to
// select the correct pre-built environment. FAIL_TO_PASS/PASS_TO_PASS test
// selection and grading remain inside the official harness.
type SWEBenchInstance struct {
	InstanceID       string `json:"instance_id"`
	Repo             string `json:"repo"`
	BaseCommit       string `json:"base_commit"`
	ProblemStatement string `json:"problem_statement"`
	Version          string `json:"version"`
	InstanceImageKey string `json:"instance_image_key"`
	EnvImageKey      string `json:"env_image_key"`
	Arch             string `json:"arch"`
	Namespace        string `json:"namespace"`
}

// SWEBenchResolveOptions selects which instances to hydrate.
type SWEBenchResolveOptions struct {
	Dataset          string
	Split            string
	InstanceIDs      []string
	Namespace        string
	InstanceImageTag string
	EnvImageTag      string
	// WorkDir receives bridge-generated image-build logs. It defaults to a
	// temporary directory, never the caller's current working directory.
	WorkDir string
}

func defaultSWEBenchNamespace(namespace string) string {
	if namespace == "" && runtime.GOARCH != "arm64" {
		return "swebench"
	}
	return namespace
}

// ResolveSWEBench loads the requested instances from the official dataset
// and derives their official test spec / image identity via
// swebench.harness.test_spec.test_spec.make_test_spec. It does not build or
// pull any image; that happens when the Docker runner starts a container.
func ResolveSWEBench(ctx context.Context, env SWEBenchEnv, opts SWEBenchResolveOptions) ([]SWEBenchInstance, error) {
	if err := env.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Dataset) == "" {
		return nil, errors.New("swebench: dataset is required")
	}
	if opts.Split == "" {
		opts.Split = SWEBenchDefaultSplit
	}
	if len(opts.InstanceIDs) == 0 {
		return nil, errors.New("swebench: at least one instance id is required")
	}
	if opts.InstanceImageTag == "" {
		opts.InstanceImageTag = "latest"
	}
	if opts.EnvImageTag == "" {
		opts.EnvImageTag = "latest"
	}
	opts.Namespace = defaultSWEBenchNamespace(opts.Namespace)
	args := []string{
		"--op", swebenchOpResolve,
		"--dataset-name", opts.Dataset,
		"--split", opts.Split,
		"--instance-image-tag", opts.InstanceImageTag,
		"--env-image-tag", opts.EnvImageTag,
	}
	if opts.Namespace != "" {
		args = append(args, "--namespace", opts.Namespace)
	}
	if len(opts.InstanceIDs) > 0 {
		args = append(args, "--instance-ids")
		args = append(args, opts.InstanceIDs...)
	}
	var response struct {
		Instances []SWEBenchInstance `json:"instances"`
		Error     string             `json:"error"`
	}
	if err := runSWEBenchBridgeDir(ctx, env, args, opts.WorkDir, &response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, fmt.Errorf("swebench: resolve: %s", response.Error)
	}
	found := make(map[string]bool, len(response.Instances))
	for _, inst := range response.Instances {
		found[inst.InstanceID] = true
	}
	for _, id := range opts.InstanceIDs {
		if !found[id] {
			return nil, fmt.Errorf("swebench: instance %q not found in dataset %q split %q", id, opts.Dataset, opts.Split)
		}
	}
	return response.Instances, nil
}

// EnsureSWEBenchImages asks the official harness to build any missing
// instance images for the selected tasks. It delegates to
// swebench.harness.prepare_images.main and verifies every expected image key
// exists before returning, so candidate runs never spend model tokens after
// an image-build failure.
func EnsureSWEBenchImages(ctx context.Context, env SWEBenchEnv, opts SWEBenchResolveOptions) error {
	if err := env.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(opts.Dataset) == "" || len(opts.InstanceIDs) == 0 {
		return errors.New("swebench: dataset and instance ids are required to prepare images")
	}
	if opts.Split == "" {
		opts.Split = SWEBenchDefaultSplit
	}
	if opts.InstanceImageTag == "" {
		opts.InstanceImageTag = "latest"
	}
	if opts.EnvImageTag == "" {
		opts.EnvImageTag = "latest"
	}
	opts.Namespace = defaultSWEBenchNamespace(opts.Namespace)
	args := []string{
		"--op", swebenchOpEnsureImage,
		"--dataset-name", opts.Dataset,
		"--split", opts.Split,
		"--max-workers", "1",
		"--force-rebuild", "false",
		"--instance-image-tag", opts.InstanceImageTag,
		"--env-image-tag", opts.EnvImageTag,
	}
	if opts.Namespace != "" {
		args = append(args, "--namespace", opts.Namespace)
	}
	args = append(args, "--instance-ids")
	args = append(args, opts.InstanceIDs...)
	var response struct {
		Images map[string]string `json:"images"`
		Error  string            `json:"error"`
	}
	if err := runSWEBenchBridgeDir(ctx, env, args, opts.WorkDir, &response); err != nil {
		return err
	}
	if response.Error != "" {
		return fmt.Errorf("swebench: image preparation: %s", response.Error)
	}
	for _, id := range opts.InstanceIDs {
		if response.Images[id] == "" {
			return fmt.Errorf("swebench: image preparation returned no image for %q", id)
		}
	}
	return nil
}

// SWEBenchOptions configures a grading run of the official harness.
type SWEBenchOptions struct {
	Dataset, Split, RunID, ModelName           string
	InstanceIDs                                []string
	MaxWorkers                                 int
	Timeout                                    time.Duration
	CacheLevel                                 string
	Clean, ForceRebuild, RewriteReports, Modal bool
	Namespace, InstanceImageTag, EnvImageTag   string
	ReportDir, WorkDir                         string
}

type SWEBenchPrediction struct {
	InstanceID      string `json:"instance_id"`
	ModelNameOrPath string `json:"model_name_or_path"`
	ModelPatch      string `json:"model_patch"`
}

type SWEBenchRunResult struct {
	PredictionsPath, ReportPath, Stdout, Stderr string
	// Instances holds the official per-instance grading verdict
	// (swebench.harness.grading.get_eval_report), keyed by instance id.
	Instances map[string]SWEBenchInstanceReport
}

// SWEBenchInstanceReport mirrors the official per-instance report.json.
// FAIL_TO_PASS/PASS_TO_PASS pass/fail lists come from the grader verbatim.
type SWEBenchInstanceReport struct {
	PatchIsNone              bool                         `json:"patch_is_None"`
	PatchExists              bool                         `json:"patch_exists"`
	PatchSuccessfullyApplied bool                         `json:"patch_successfully_applied"`
	Resolved                 bool                         `json:"resolved"`
	TestsStatus              *SWEBenchInstanceTestsStatus `json:"tests_status,omitempty"`
}

type SWEBenchInstanceTestsStatus struct {
	FailToPass SWEBenchTestOutcomes `json:"FAIL_TO_PASS"`
	PassToPass SWEBenchTestOutcomes `json:"PASS_TO_PASS"`
}

type SWEBenchTestOutcomes struct {
	Success []string `json:"success,omitempty"`
	Failure []string `json:"failure,omitempty"`
}

// RunSWEBench writes official predictions JSONL and invokes the pinned
// bridge's grade operation, which delegates to
// swebench.harness.run_evaluation.main. The official harness owns image
// selection, test-patch application, FAIL_TO_PASS/PASS_TO_PASS evaluation,
// and report generation; this function only marshals predictions in and
// parses the resulting report path back out.
func RunSWEBench(ctx context.Context, env SWEBenchEnv, predictions []SWEBenchPrediction, opts SWEBenchOptions) (SWEBenchRunResult, error) {
	if err := env.validate(); err != nil {
		return SWEBenchRunResult{}, err
	}
	if ctx == nil {
		return SWEBenchRunResult{}, errors.New("swebench: nil context")
	}
	if err := ctx.Err(); err != nil {
		return SWEBenchRunResult{}, fmt.Errorf("swebench: %w", err)
	}
	if len(predictions) == 0 {
		return SWEBenchRunResult{}, errors.New("swebench: at least one prediction is required")
	}
	if strings.TrimSpace(opts.Dataset) == "" || opts.RunID == "" {
		return SWEBenchRunResult{}, errors.New("swebench: dataset and run id are required")
	}
	if opts.Split == "" {
		opts.Split = SWEBenchDefaultSplit
	}
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = 4
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Minute
	}
	if opts.CacheLevel == "" {
		opts.CacheLevel = "env"
	}
	opts.Namespace = defaultSWEBenchNamespace(opts.Namespace)
	if opts.InstanceImageTag == "" {
		opts.InstanceImageTag = "latest"
	}
	if opts.EnvImageTag == "" {
		opts.EnvImageTag = "latest"
	}
	if opts.ReportDir == "" {
		opts.ReportDir = "."
	}
	if opts.WorkDir == "" {
		opts.WorkDir = "."
	}
	if err := validateSWEBenchPredictions(predictions, opts.ModelName); err != nil {
		return SWEBenchRunResult{}, err
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return SWEBenchRunResult{}, err
	}
	if err := os.MkdirAll(opts.ReportDir, 0o755); err != nil {
		return SWEBenchRunResult{}, err
	}
	predictionsPath := filepath.Join(opts.WorkDir, "predictions.jsonl")
	if err := writeSWEBenchPredictions(predictionsPath, predictions, opts.ModelName); err != nil {
		return SWEBenchRunResult{}, err
	}

	args := []string{
		"--op", swebenchOpGrade,
		"--dataset-name", opts.Dataset, "--split", opts.Split, "--predictions-path", predictionsPath,
		"--max-workers", fmt.Sprint(opts.MaxWorkers), "--run-id", opts.RunID,
		"--timeout", fmt.Sprint(int(opts.Timeout.Seconds())), "--cache-level", opts.CacheLevel,
		"--clean", fmt.Sprint(opts.Clean), "--force-rebuild", fmt.Sprint(opts.ForceRebuild),
		"--rewrite-reports", fmt.Sprint(opts.RewriteReports), "--modal", fmt.Sprint(opts.Modal),
		"--namespace", opts.Namespace, "--instance-image-tag", opts.InstanceImageTag,
		"--env-image-tag", opts.EnvImageTag, "--report-dir", opts.ReportDir,
	}
	if len(opts.InstanceIDs) > 0 {
		args = append(args, "--instance-ids")
		args = append(args, opts.InstanceIDs...)
	}

	var response struct {
		ReportPath string                            `json:"report_path"`
		Instances  map[string]SWEBenchInstanceReport `json:"instances"`
		Error      string                            `json:"error"`
	}
	stdout, stderr, err := runSWEBenchBridgeRawDir(ctx, env, args, opts.WorkDir)
	result := SWEBenchRunResult{PredictionsPath: predictionsPath, Stdout: stdout, Stderr: stderr}
	if err == nil {
		if uerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &response); uerr == nil {
			result.ReportPath = response.ReportPath
			result.Instances = response.Instances
			if response.Error != "" {
				err = fmt.Errorf("swebench: grade: %s", response.Error)
			}
		}
	}
	if err != nil {
		return result, fmt.Errorf("swebench bridge: %w: %s", err, strings.TrimSpace(stderr))
	}
	return result, nil
}

// runSWEBenchBridge runs the bridge with a fixed --expected-version and
// decodes its single-line JSON stdout into v.
func runSWEBenchBridge(ctx context.Context, env SWEBenchEnv, args []string, v any) error {
	return runSWEBenchBridgeDir(ctx, env, args, "", v)
}

func runSWEBenchBridgeDir(ctx context.Context, env SWEBenchEnv, args []string, dir string, v any) error {
	cleanup := func() {}
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "gremlord-swebench-")
		if err != nil {
			return err
		}
		cleanup = func() { os.RemoveAll(dir) }
	}
	defer cleanup()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stdout, stderr, err := runSWEBenchBridgeRawDir(ctx, env, args, dir)
	if err != nil {
		return fmt.Errorf("swebench bridge: %w: %s", err, strings.TrimSpace(stderr))
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), v); err != nil {
		return fmt.Errorf("swebench bridge: invalid JSON output: %w", err)
	}
	return nil
}

func runSWEBenchBridgeRaw(ctx context.Context, env SWEBenchEnv, args []string) (stdout, stderr string, err error) {
	return runSWEBenchBridgeRawDir(ctx, env, args, "")
}

func runSWEBenchBridgeRawDir(ctx context.Context, env SWEBenchEnv, args []string, dir string) (stdout, stderr string, err error) {
	if ctx == nil {
		return "", "", errors.New("swebench: nil context")
	}
	if cerr := ctx.Err(); cerr != nil {
		return "", "", fmt.Errorf("swebench: %w", cerr)
	}
	full := append([]string{env.BridgePath, "--expected-version", SWEBenchPackageVersion}, args...)
	cmd := exec.CommandContext(ctx, env.python(), full...)
	cmd.Dir = dir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	runErr := cmd.Run()
	return outBuf.String(), errBuf.String(), runErr
}

func validateSWEBenchPredictions(predictions []SWEBenchPrediction, defaultModel string) error {
	seen := map[string]bool{}
	for i, p := range predictions {
		if strings.TrimSpace(p.InstanceID) == "" {
			return fmt.Errorf("swebench: prediction %d missing instance_id", i)
		}
		if seen[p.InstanceID] {
			return fmt.Errorf("swebench: duplicate instance_id %q", p.InstanceID)
		}
		seen[p.InstanceID] = true
		if strings.TrimSpace(p.ModelNameOrPath) == "" && strings.TrimSpace(defaultModel) == "" {
			return fmt.Errorf("swebench: prediction %q missing model_name_or_path", p.InstanceID)
		}
		if strings.TrimSpace(p.ModelPatch) == "" {
			return fmt.Errorf("swebench: prediction %q has an empty model_patch", p.InstanceID)
		}
	}
	return nil
}

func writeSWEBenchPredictions(path string, predictions []SWEBenchPrediction, defaultModel string) error {
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	enc.SetEscapeHTML(false)
	for _, p := range predictions {
		if p.ModelNameOrPath == "" {
			p.ModelNameOrPath = defaultModel
		}
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
