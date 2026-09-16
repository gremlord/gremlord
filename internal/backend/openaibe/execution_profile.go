package openaibe

import (
	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/config"
)

// GPTEfficientPrompt is versioned and frozen for reproducible A/B evaluations.
// It supplements the actual client instructions; it does not replace tools,
// grant permissions, cap tool calls, or change model effort.
const GPTEfficientPrompt = `<gremlord_execution_profile version="gpt-efficient-v1">
Complete the user's requested outcome while preserving all applicable instructions, tool contracts, permission boundaries, and requirements from earlier turns.

For implementation work:
- Gather the relevant files and constraints, then make cohesive changes. Batch independent reads or searches in one tool round when supported. Follow dependencies in order; never parallelize conflicting writes.
- Use evidence already available in the conversation. Read again when content changed, was truncated, or a specific unresolved question requires it. Request focused ranges and searches instead of repeatedly dumping whole files or logs.
- Choose the smallest complete implementation that satisfies the contract. Avoid speculative features and broad rewrites outside the requested scope.
- Run all explicitly required checks. Add or update tests for changed behavior and important edge cases. Fix failures, then rerun affected checks. Repeat passing checks only when a subsequent edit, unresolved risk, or an explicit reliability requirement justifies it.
- Once the requested work is complete and the relevant checks pass on the final code, report the result and stop. If something remains incomplete or unverified, say exactly what it is; do not claim success or silently omit requirements to save time.
</gremlord_execution_profile>`

// ApplyExecutionProfile appends the opted-in supplement to a freshly parsed
// request. The router uses the same operation for its pre-dispatch size guard;
// the backend applies it to its own parsed copy before translation/counting.
func ApplyExecutionProfile(req *anthropic.MessagesRequest, route config.Resolved) {
	if route.Provider.Type != config.ProviderOpenAI || route.APIFlavor() != config.APIResponses || route.Model.ExecutionProfile != "gpt-efficient-v1" {
		return
	}
	req.System = append(req.System, anthropic.ContentBlock{Type: "text", Text: GPTEfficientPrompt})
}
