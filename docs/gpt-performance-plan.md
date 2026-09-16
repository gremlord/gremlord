# GPT performance follow-up

Status: 2026-09-16. The Responses fidelity fixes, matched benchmarks, and opt-in
execution profile are implemented. [Profile pilot results](gpt-profile-results.md)
do not establish a consistent improvement in both time and cost. Defaults remain
unchanged; context reduction, persistence, and compaction below are follow-up work.

The [three-turn benchmark](gpt-benchmark-results.md) passed for both harnesses,
but updated Gremlord took 2.29× the agent time and 2.07× the estimated cost.
This is one workflow, not a general ranking or a before/after regression test.
The next target is equal task success with fewer model rounds and less context
reprocessing. Lowering reasoning effort is not a substitute for a matched
comparison: both harnesses already used high effort.

## Product constraint

GPT stays inside Claude Code. Preserve its interactive interface, tools, and
permissions. Native Codex remains the reference benchmark arm. A new native
Codex front-end is outside this follow-up's scope.

## 1. Reduce unnecessary model rounds

The opt-in GPT execution profile is available as a separate benchmark arm. Retain actual
Claude tool schemas and permissions. Give concise guidance to batch independent
reads, plan cohesive edits, run relevant checks after meaningful changes, and
finish after acceptance checks pass unless a new failure or edit warrants more
work. Do not impose a hard tool-call cap that can hide unfinished work.

Measure tool rounds, generated reasoning, input growth, test failures and fixes,
and cumulative checkpoint success. Add timing for first token, full response,
and tool execution before attributing the remaining latency to the API or
protocol. Responses Lite and standard function tools differ; the current
benchmark does not isolate their effects. Do not copy internal protocol headers
without implementing and validating the corresponding protocol.

Use the queue task for development, then freeze the profile before evaluating
held-out multi-turn repository tasks. A profile that merely makes this one task
cheaper is insufficient to change defaults. Keep both arms on the same model
and effort; tune effort separately only after this comparison.

Default-enablement gate: the current and experimental profiles must pass every cumulative
queue checkpoint, with repeated paired attempts showing lower time and cost.
Record model/CLI versions, prompt hashes, request counts, tool calls, and token
breakdowns. Freeze the winning profile before held-out evaluation. A ratio at
or below 1.10 of native Codex on time and cost is a proposed engineering target,
not a measured result or a guarantee; preserve quality even if that target is
not reached.

The current pilot does not meet this gate: the completed queue pair has only
small gains, and the held-out planner trades lower cost for higher latency.

## 2. Reduce repeated input without losing task state

The meter now records prompt, tool-schema, visible-history, and replay counts
without persisting opaque reasoning or private prompt contents. Tool descriptions
account for 42.3 KB of text within 58.5 KB of serialized tool definitions in the
pilot. Test compact descriptions next, retaining their necessary instructions.
Introduce one opt-in change at a time so a speedup has an attributable cause:

- Bound oversized tool results, preserving useful head/tail sections and a way
  to retrieve the full result. Check that error details and required facts stay
  available; do not silently truncate arbitrary conversation history.
- Test stable compact tool descriptions while retaining the real tool names,
  parameter schemas, tool behavior, and permission semantics.
- Keep tool declarations and history prefixes stable when possible. The current
  96.7% cache-hit rate is already high; measure total cached tokens billed as
  well as hit rate. Extra rounds still cost money with a warm cache.

Do not drop encrypted reasoning to make the cost number smaller without testing
multi-turn correctness. Any reasoning-context or effort change is its own
matched experiment, with continuity and long-horizon checks.

## 3. Context pressure, compaction, and restart

The completed benchmarks never force compaction. Add scenarios with large tool
logs, actual compaction, resumed work, router restart, edited/forked history,
interrupted calls, and model switches. Check that later requirements still
preserve earlier invariants and persisted state.

Experiment with a translator-owned persistent history/checkpoint before native
compaction. Keep full logs retrievable, mark truncation, preserve call/result
pairs, and avoid
reintroducing context removed by the client. Synchronize client compaction and
context edits with the native replay state. Encrypted reasoning replay by itself
does not solve these boundaries.

Report quality, total cost, compaction cost, and latency together; earlier
compaction is not automatically an improvement.

## Release evidence

After the profile pilot, use a frozen set of held-out repository workflows
with multiple user turns, repeated paired attempts, and task-clustered
uncertainty estimates. Include context-pressure tasks as their own stratum.
Require no material task-success regression and publish time, cost per resolved
task, and tail latency against native Codex. Preserve failures and excluded-run
reasons. Report ratios as observations on that suite, not universal parity.
