//go:build unix

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

func claudeArm(arm string) bool { return arm == "gremlord" || arm == "gpt-efficient" }

func validateArms(baseline, mut string) error {
	for _, arm := range []string{baseline, mut} {
		if arm != "codex" && !claudeArm(arm) {
			return fmt.Errorf("unknown benchmark arm %q", arm)
		}
	}
	if baseline == mut {
		return fmt.Errorf("benchmark arms must differ")
	}
	return nil
}

// Serialized sizes and stable hashes support context-growth diagnosis without
// storing prompt contents or opaque reasoning. These are bytes, not tokens.
func measureInput(body map[string]json.RawMessage, m *measurement) {
	var instructions string
	json.Unmarshal(body["instructions"], &instructions)
	m.InstructionsBytes = len(instructions)
	m.InstructionsSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(instructions)))
	m.ToolSchemaBytes = len(body["tools"])
	m.ToolSchemaSHA256 = fmt.Sprintf("%x", sha256.Sum256(body["tools"]))
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
