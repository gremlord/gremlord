//go:build unix

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gremlord/gremlord/internal/backend/openaibe"
	"github.com/gremlord/gremlord/internal/config"
)

// benchmarkRoute only changes this run's copy; never rewrite the user's config.
func benchmarkRoute(route config.Resolved, api string, budget int) (config.Resolved, error) {
	if api != "" && api != config.APIResponses {
		return route, fmt.Errorf("benchmark API override must be responses")
	}
	if api != "" {
		route.Model.API = api
	}
	if route.Provider.Type != config.ProviderOpenAI || route.APIFlavor() != config.APIResponses {
		return route, fmt.Errorf("benchmark requires an OpenAI-compatible Responses route; use -api responses to override this run")
	}
	if budget <= 0 {
		return route, fmt.Errorf("context budget must be positive")
	}
	if route.Model.ContextWindow > 0 {
		budget = min(budget, route.Model.ContextWindow)
	}
	route.Model.Reasoning, route.Model.ReasoningEffort = "effort", "high"
	route.Model.ExecutionProfile = ""
	route.Model.EffectiveContext = budget
	return route, nil
}

func claudeArm(arm string) bool { return arm == "gremlord" || arm == "gpt-efficient" }

func validateArms(baseline, mut string) error {
	for _, arm := range []string{baseline, mut} {
		if arm != "codex" && arm != "codex-gremlord" && !claudeArm(arm) {
			return fmt.Errorf("unknown benchmark arm %q", arm)
		}
	}
	if baseline == mut {
		return fmt.Errorf("benchmark arms must differ")
	}
	return nil
}

// UTF-8 text sizes, serialized JSON sizes, and stable hashes support context
// diagnosis without storing prompt contents or opaque reasoning. Not tokens.
func measureInput(body map[string]json.RawMessage, m *measurement) {
	var instructions string
	json.Unmarshal(body["instructions"], &instructions)
	m.InstructionsBytes = len(instructions)
	m.InstructionsSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(instructions)))
	base := strings.TrimSuffix(instructions, "\n\n"+openaibe.GPTEfficientPrompt)
	m.BaseInstructionsSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(base)))
	m.ToolSchemaBytes = len(body["tools"])
	m.ToolSchemaSHA256 = fmt.Sprintf("%x", sha256.Sum256(body["tools"]))
	var tools []struct {
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	json.Unmarshal(body["tools"], &tools)
	m.ToolCount = len(tools)
	for _, tool := range tools {
		m.ToolDescriptionBytes += len(tool.Description)
		m.ToolParameterBytes += len(tool.Parameters)
	}
	var items []json.RawMessage
	json.Unmarshal(body["input"], &items)
	for _, item := range items {
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &header) == nil && header.Type != "reasoning" {
			m.VisibleInputBytes += len(item)
		}
	}
}
