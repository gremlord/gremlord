package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMaterializeSWEBenchBridge(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	env, err := MaterializeSWEBenchBridge(dir, "custom-python")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(env.BridgePath) || env.Python != "custom-python" {
		t.Fatalf("env = %+v", env)
	}
	data, err := os.ReadFile(env.BridgePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "swebench.harness.run_evaluation") || !strings.Contains(string(data), SWEBenchPackageVersion) {
		t.Fatalf("embedded bridge does not contain pinned harness contract")
	}
}

func TestWriteSWEBenchPredictionsOfficialSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "predictions.jsonl")
	predictions := []SWEBenchPrediction{
		{InstanceID: "django__django-11001", ModelPatch: "diff --git a/a b/a\n"},
		{InstanceID: "sympy__sympy-20590", ModelNameOrPath: "other", ModelPatch: "patch"},
	}
	if err := validateSWEBenchPredictions(predictions, "gremlord/auto"); err != nil {
		t.Fatal(err)
	}
	if err := writeSWEBenchPredictions(path, predictions, "gremlord/auto"); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	var got []SWEBenchPrediction
	for s.Scan() {
		var p SWEBenchPrediction
		if err := json.Unmarshal(s.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ModelNameOrPath != "gremlord/auto" || got[1].ModelNameOrPath != "other" || got[0].ModelPatch != predictions[0].ModelPatch {
		t.Fatalf("predictions = %#v", got)
	}
}

func TestValidateSWEBenchPredictions(t *testing.T) {
	valid := SWEBenchPrediction{InstanceID: "repo__repo-1", ModelNameOrPath: "model", ModelPatch: "patch"}
	for _, tc := range []struct {
		name        string
		values      []SWEBenchPrediction
		model, want string
	}{
		{"empty", nil, "model", "at least one"},
		{"id", []SWEBenchPrediction{{ModelPatch: "patch"}}, "model", "missing instance_id"},
		{"duplicate", []SWEBenchPrediction{valid, valid}, "", "duplicate instance_id"},
		{"model", []SWEBenchPrediction{{InstanceID: "x", ModelPatch: "patch"}}, "", "missing model_name_or_path"},
		{"patch", []SWEBenchPrediction{{InstanceID: "x", ModelNameOrPath: "model"}}, "", "empty model_patch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.values == nil {
				_, err := RunSWEBench(context.Background(), SWEBenchEnv{BridgePath: "unused.py"}, nil, SWEBenchOptions{Dataset: "d", RunID: "r"})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			err := validateSWEBenchPredictions(tc.values, tc.model)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// fakeBridge writes a shell script standing in for swebench_bridge.py: it
// records every argument it was called with and prints a canned JSON
// response, so these tests exercise Go's argument construction and response
// parsing without requiring Python, swebench, or Docker.
func fakeBridge(t *testing.T, dir string, response string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bridge := filepath.Join(dir, "bridge.sh")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %s/calls.txt\nprintf '\\n---\\n' >> %s/calls.txt\nprintf '%s\\n'\n", dir, dir, response)
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bridge
}

func TestCheckSWEBenchReportsPrerequisites(t *testing.T) {
	dir := t.TempDir()
	bridge := fakeBridge(t, dir, `{"swebench_version":"4.1.0","swebench_ok":true,"python_version":"3.11.0","arch":"x86_64","docker_ok":true,"docker_error":"","error":""}`)
	env := SWEBenchEnv{Python: bridge, BridgePath: "ignored.py"}
	result, err := CheckSWEBench(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK() || result.SWEBenchVersion != "4.1.0" {
		t.Fatalf("result = %+v", result)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls.txt"))
	if !strings.Contains(string(calls), "--op\ncheck") || !strings.Contains(string(calls), "--expected-version\n"+SWEBenchPackageVersion) {
		t.Errorf("bridge invocation missing expected args:\n%s", calls)
	}
}

func TestCheckSWEBenchFailsClosedOnMissingDocker(t *testing.T) {
	dir := t.TempDir()
	bridge := fakeBridge(t, dir, `{"swebench_version":"4.1.0","swebench_ok":true,"python_version":"3.11.0","arch":"x86_64","docker_ok":false,"docker_error":"docker not found","error":""}`)
	env := SWEBenchEnv{Python: bridge, BridgePath: "ignored.py"}
	result, err := CheckSWEBench(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("error = %v, result = %+v", err, result)
	}
}

func TestResolveSWEBenchParsesInstancesAndValidatesCoverage(t *testing.T) {
	dir := t.TempDir()
	bridge := fakeBridge(t, dir, `{"instances":[{"instance_id":"astropy__astropy-14309","repo":"astropy/astropy","base_commit":"abc123","problem_statement":"fix the bug","version":"5.1","instance_image_key":"sweb.eval.x86_64.astropy__astropy-14309:latest","env_image_key":"sweb.env.x86_64.deadbeef:latest","arch":"x86_64","namespace":"swebench"}]}`)
	env := SWEBenchEnv{Python: bridge, BridgePath: "ignored.py"}
	instances, err := ResolveSWEBench(context.Background(), env, SWEBenchResolveOptions{
		Dataset: "princeton-nlp/SWE-bench_Verified", InstanceIDs: []string{"astropy__astropy-14309"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].InstanceID != "astropy__astropy-14309" || instances[0].InstanceImageKey == "" {
		t.Fatalf("instances = %+v", instances)
	}

	// Requesting an instance id the bridge didn't return must fail loudly
	// rather than silently proceeding with a partial task set.
	bridge2 := fakeBridge(t, t.TempDir(), `{"instances":[]}`)
	env2 := SWEBenchEnv{Python: bridge2, BridgePath: "ignored.py"}
	_, err = ResolveSWEBench(context.Background(), env2, SWEBenchResolveOptions{
		Dataset: "princeton-nlp/SWE-bench_Verified", InstanceIDs: []string{"missing-instance"},
	})
	if err == nil || !strings.Contains(err.Error(), "not found in dataset") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnsureSWEBenchImagesInvokesOfficialBridge(t *testing.T) {
	dir := t.TempDir()
	bridge := fakeBridge(t, dir, `{"images":{"astropy__astropy-14309":"sweb.eval.x86_64.astropy__astropy-14309:latest"}}`)
	env := SWEBenchEnv{Python: bridge, BridgePath: "ignored.py"}
	err := EnsureSWEBenchImages(context.Background(), env, SWEBenchResolveOptions{
		Dataset: "princeton-nlp/SWE-bench_Verified", InstanceIDs: []string{"astropy__astropy-14309"},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls.txt"))
	joined := string(calls)
	for _, want := range []string{"--op\nensure_image", "--instance-ids\nastropy__astropy-14309", "--max-workers\n1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bridge invocation missing %q:\n%s", want, joined)
		}
	}

	missingDir := t.TempDir()
	missingBridge := fakeBridge(t, missingDir, `{"images":{}}`)
	err = EnsureSWEBenchImages(context.Background(), SWEBenchEnv{Python: missingBridge, BridgePath: "ignored.py"}, SWEBenchResolveOptions{
		Dataset: "dataset", InstanceIDs: []string{"missing"},
	})
	if err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("missing image error = %v", err)
	}
}

func TestRunSWEBenchInvokesPinnedBridge(t *testing.T) {
	dir := t.TempDir()
	bridge := fakeBridge(t, dir, `{"report_path":"report.json"}`)
	env := SWEBenchEnv{Python: bridge, BridgePath: "ignored.py"}
	result, err := RunSWEBench(context.Background(), env, []SWEBenchPrediction{{InstanceID: "repo__repo-1", ModelPatch: "patch"}}, SWEBenchOptions{
		Dataset: "princeton-nlp/SWE-bench_Verified", RunID: "unit", ModelName: "model", WorkDir: dir,
		ReportDir: filepath.Join(dir, "reports"), Timeout: 2 * time.Minute, InstanceIDs: []string{"repo__repo-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReportPath != "report.json" {
		t.Fatalf("report = %q", result.ReportPath)
	}
	args, err := os.ReadFile(filepath.Join(dir, "calls.txt"))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(args)
	for _, want := range []string{
		"--op\ngrade", "--expected-version\n" + SWEBenchPackageVersion,
		"--dataset-name\nprinceton-nlp/SWE-bench_Verified", "--instance-ids\nrepo__repo-1",
		"--predictions-path\n" + result.PredictionsPath,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("arguments missing %q:\n%s", want, joined)
		}
	}
}

func TestRunSWEBenchSurfacesBridgeReportedError(t *testing.T) {
	dir := t.TempDir()
	bridge := fakeBridge(t, dir, `{"error":"unsupported run_evaluation API; missing ['modal']"}`)
	env := SWEBenchEnv{Python: bridge, BridgePath: "ignored.py"}
	_, err := RunSWEBench(context.Background(), env, []SWEBenchPrediction{{InstanceID: "repo__repo-1", ModelNameOrPath: "model", ModelPatch: "patch"}}, SWEBenchOptions{
		Dataset: "d", RunID: "unit", WorkDir: dir, ReportDir: filepath.Join(dir, "reports"),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported run_evaluation API") {
		t.Fatalf("error = %v", err)
	}
}

func TestSWEBenchBridgeSyntax(t *testing.T) {
	if _, err := os.Stat("swebench_bridge.py"); err != nil {
		t.Fatal(err)
	}
	python := "python3"
	if _, err := exec.LookPath(python); err != nil {
		t.Skip("python3 not available")
	}
	cmd := exec.Command(python, "-m", "py_compile", "swebench_bridge.py")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("py_compile: %v\n%s", err, out)
	}
}
