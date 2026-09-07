package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gremlord/gremlord/internal/config"
)

// taskClassifierPrompt asks the classifier for tier and task together in one
// request — task-aware routing never issues a second classifier call.
const taskClassifierPrompt = `You route requests inside a coding agent to a model tier and, when it clearly
applies, a task label.

Reply with ONLY a JSON object, no other text: {"tier": "deep|standard|light", "task": "<label or empty>"}

Tiers:
deep: planning, architecture, debugging hard problems, multi-step reasoning, ambiguous or high-stakes decisions
standard: writing or modifying code, ordinary multi-tool tasks
light: mechanical edits, renames, formatting, summaries, verifying provided output, short factual answers

Task labels — use one only if it clearly applies, otherwise use "":
implementation: writing new code or features
sql_data: SQL, data pipelines, database schema/queries
debugging: diagnosing or fixing a bug/failure
code_review: reviewing a diff or PR for correctness/quality
architecture: system design, planning, structural decisions
security_review: assessing security implications or vulnerabilities
critical_review: high-stakes sanity check of a decision or plan already made

Request to classify:
`

// taskTierDecision is the classifier's combined answer for a task-aware
// routing rule.
type taskTierDecision struct {
	Tier string `json:"tier"`
	Task string `json:"task"`
}

// parseTaskLabel is the allow-list parser for classifier-provided task
// labels: normalizes case/whitespace and validates against the fixed
// config.TaskLabels set, failing open (returning "") for anything that
// doesn't match exactly — including near-misses, synonyms, or the
// classifier inventing its own label. Classifier output is untrusted free
// text, not a contract, so "invalid" is a routing no-op, never an error.
func parseTaskLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, ".\"' \n")
	if config.IsTaskLabel(s) {
		return s
	}
	return ""
}

// classifyTaskViaBackend runs the combined tier+task classifier prompt
// through the router's own backends. Unparseable or non-JSON classifier
// output returns an error; route() handles that error by failing open to the
// configured tier and dropping the task override.
func (s *Server) classifyTaskViaBackend(ctx context.Context, rule config.RouteRule, cfg *config.Config, summary string) (tier, task string, err error) {
	resp, err := s.runClassifier(ctx, rule, cfg, taskClassifierPrompt+summary, 60)
	if err != nil {
		return "", "", err
	}
	for _, block := range resp.Content {
		if block.Type != "text" {
			continue
		}
		text := strings.TrimSpace(block.Text)
		// Classifiers occasionally wrap the JSON in a code fence despite
		// instructions; strip one if present before parsing.
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
		var d taskTierDecision
		if err := json.Unmarshal([]byte(text), &d); err != nil {
			// Return an error so route() still fails open to the configured tier,
			// while retaining an observable classifier-failure signal for callers.
			return "", "", fmt.Errorf("parse task classifier output: %w", err)
		}
		return strings.ToLower(strings.TrimSpace(d.Tier)), parseTaskLabel(d.Task), nil
	}
	return "", "", fmt.Errorf("task classifier returned no text")
}
