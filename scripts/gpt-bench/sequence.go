//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type sequenceTurn struct {
	Prompt   string   `json:"prompt"`
	Verifier []string `json:"verifier"`
}
type turnState struct {
	claudeSession, nativeThread string
	turn                        int
}

func loadSequence(path string, tasks int) ([]sequenceTurn, error) {
	if path == "" {
		return nil, nil
	}
	if tasks != 1 {
		return nil, errors.New("a sequence requires exactly one manifest task")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var turns []sequenceTurn
	if err := json.Unmarshal(data, &turns); err != nil {
		return nil, err
	}
	if len(turns) < 2 {
		return nil, errors.New("a sequence requires at least two user turns")
	}
	for _, turn := range turns {
		if turn.Prompt == "" || len(turn.Verifier) == 0 {
			return nil, errors.New("every sequence turn requires a prompt and verifier")
		}
	}
	return turns, nil
}

func (e *executor) Run(ctx context.Context, dir string, env, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 || argv[0] != e.claude || len(e.sequence) == 0 {
		return e.runTurn(ctx, dir, env, argv, stdin, stdout, stderr, nil)
	}
	state := &turnState{}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	state.claudeSession = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	for i, step := range e.sequence {
		state.turn = i + 1
		args := append([]string(nil), argv...)
		args[len(args)-1] = step.Prompt
		turnDir := filepath.Join(filepath.Dir(dir), fmt.Sprintf("turn-%02d", state.turn))
		if err := os.MkdirAll(turnDir, 0700); err != nil {
			return err
		}
		var out, errs bytes.Buffer
		start := time.Now()
		turnCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := e.runTurn(turnCtx, dir, env, args, nil, &out, &errs, state)
		cancel()
		agentMS := time.Since(start).Milliseconds()
		if werr := os.WriteFile(filepath.Join(turnDir, "stdout.json"), out.Bytes(), 0600); werr != nil {
			return werr
		}
		if werr := os.WriteFile(filepath.Join(turnDir, "stderr.log"), errs.Bytes(), 0600); werr != nil {
			return werr
		}
		stderr.Write(errs.Bytes())
		if err != nil {
			return fmt.Errorf("user turn %d: %w", state.turn, err)
		}
		var grading bytes.Buffer
		gradeCtx, gradeCancel := context.WithTimeout(ctx, time.Minute)
		gradeErr := runCommand(gradeCtx, dir, os.Environ(), step.Verifier, nil, &grading, &grading)
		gradeCancel()
		result := map[string]any{"turn": state.turn, "passed": gradeErr == nil, "agent_ms": agentMS, "verifier_output": grading.String()}
		if err := writeJSON(filepath.Join(turnDir, "grade.json"), result); err != nil {
			return err
		}
		// Capture additions too, without committing or changing candidate history.
		add := exec.CommandContext(ctx, "git", "add", "-A", "-N")
		add.Dir = dir
		if err := add.Run(); err != nil {
			return err
		}
		patch := exec.CommandContext(ctx, "git", "diff", "HEAD", "--binary")
		patch.Dir = dir
		b, err := patch.Output()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(turnDir, "patch.diff"), b, 0600); err != nil {
			return err
		}
		fmt.Printf("User turn %d complete: pass=%t, agent=%.1fs\n", state.turn, gradeErr == nil, float64(agentMS)/1000)
		if i == len(e.sequence)-1 {
			stdout.Write(out.Bytes())
		}
	}
	return nil
}
