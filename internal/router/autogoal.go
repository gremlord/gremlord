package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
)

// goalRouter runs a second, independent classification pass over each new
// user turn: does this task look like it would benefit from a persistent
// recurring loop (monitoring a build, retrying until a condition holds,
// babysitting a long-running process) rather than a single reply? Unlike
// autoRouter, there is nothing to keep sticky mid-turn — the check only
// ever runs on a fresh user turn, so no cache is needed here.
type goalRouter struct {
	classify func(ctx context.Context, rule config.RouteRule, cfg *config.Config, summary string) (goalDecision, error)
}

type goalDecision struct {
	Goal   bool   `json:"goal"`
	Reason string `json:"reason"`
}

const goalPrompt = `You watch requests inside a coding agent and decide whether the task would
benefit from a persistent recurring loop instead of a single reply — e.g.
monitoring a long-running build or deploy, retrying until a condition holds,
polling external state, babysitting a process over time. Ordinary one-shot
coding, questions, and edits are NOT goal-worthy.

Reply with ONLY a JSON object, no other text: {"goal": true|false, "reason": "<3-6 word phrase>"}
The reason should be omitted or empty when goal is false.

Request to classify:
`

// check decides whether the current turn is goal-worthy. isNewTurn tells
// the caller whether a decision was made at all: continuations never
// re-classify, so callers must not overwrite the prior persisted decision
// when isNewTurn is false (that decision belongs to the turn still in
// flight). worthy fails open (false) on classifier errors or unparseable
// answers.
func (g *goalRouter) check(ctx context.Context, rule config.RouteRule, cfg *config.Config, raw []byte, sessionID string) (worthy bool, reason string, isNewTurn bool) {
	req, err := anthropic.ParseRequest(raw)
	if err != nil {
		return false, "", false
	}
	userText, isNewTurn := lastUserText(req)
	if !isNewTurn || userText == "" {
		return false, "", isNewTurn
	}

	summary := fmt.Sprintf("(conversation: %d messages, %d tools available)\n%s",
		len(req.Messages), len(req.Tools), truncate(userText, 2000))
	// Bounded like tier routing (autoroute.go): this runs inline before the
	// real upstream call, so a stuck classifier would stall the whole turn.
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	decision, err := g.classify(cctx, rule, cfg, summary)
	if err != nil {
		return false, "", true
	}
	return decision.Goal, sanitizeReason(decision.Reason), true
}

// sanitizeReason clamps and neuters a classifier-provided reason before it
// is spliced into the system prompt. The classifier is asked for a short
// phrase, but that's a request, not a guarantee — this strips characters
// that could break out of the <system-reminder> block if the classifier
// echoes injected content back from the turn it read.
func sanitizeReason(s string) string {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("<", "", ">", "", "\n", " ", "\r", " ").Replace(s)
	const max = 80
	if len(s) > max {
		s = strings.TrimSpace(s[:max])
	}
	return s
}

// classifyGoalViaBackend runs the goal-detection prompt through the
// router's own backends and parses the strict-JSON answer.
func (s *Server) classifyGoalViaBackend(ctx context.Context, rule config.RouteRule, cfg *config.Config, summary string) (goalDecision, error) {
	resp, err := s.runClassifier(ctx, rule, cfg, goalPrompt+summary, 60)
	if err != nil {
		return goalDecision{}, err
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
		var d goalDecision
		if err := json.Unmarshal([]byte(text), &d); err != nil {
			return goalDecision{}, fmt.Errorf("goal classifier returned unparseable answer: %w", err)
		}
		return d, nil
	}
	return goalDecision{}, fmt.Errorf("goal classifier returned no text")
}

// goalReminderTemplate is injected into the system prompt when a turn is
// judged goal-worthy, naming the harness's own recognized mechanisms so the
// model can act on it directly rather than being told to invent one.
const goalReminderTemplate = `<system-reminder>
gremlord: this task looks well suited to a recurring goal loop rather than a
single reply (%s). If a persistent loop would help — checking back on
progress, retrying until a condition holds, babysitting a long-running
process — call ScheduleWakeup with prompt "<<autonomous-loop-dynamic>>" and a
reason, or invoke the /loop skill. This is a suggestion, not a requirement:
ignore it for tasks that finish in one pass.
</system-reminder>`

// injectGoalReminder appends the goal-loop reminder to the request's system
// prompt, preserving every other byte-equivalent field (numbers survive via
// json.Number, same as backend.RewriteModel). The "system" field may be
// absent, a plain string, or a content-block array — all three are folded
// into the block-array form with the reminder appended, matching the shape
// Claude Code's own <system-reminder> blocks already arrive in.
func injectGoalReminder(raw []byte, reason string) ([]byte, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	reminder := map[string]any{"type": "text", "text": fmt.Sprintf(goalReminderTemplate, reason)}

	var blocks []any
	switch sys := m["system"].(type) {
	case nil:
		// absent
	case string:
		if sys != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": sys})
		}
	case []any:
		blocks = sys
	default:
		return nil, fmt.Errorf("unexpected system field type %T", sys)
	}
	m["system"] = append(blocks, reminder)
	return json.Marshal(m)
}
