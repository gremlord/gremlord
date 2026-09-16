# GPT execution profile experiment

`execution_profile: gpt-efficient-v1` is an opt-in supplement for a model using
OpenAI Responses. GPT continues to run inside Claude Code. The existing system
instructions, tool names and schemas, permissions, reasoning effort, and context
policy are retained. No call limit or early-success shortcut is added.

The supplement encourages batching independent reads, using available evidence,
making cohesive in-scope edits, and ending after the requested work and required
validation are complete. Failing checks, subsequent edits, and unresolved risks
still require follow-through. It adds 1,388 UTF-8 bytes including the separator;
that is extra prompt content, so a net benefit must be measured.

See the [Sol pilot](gpt-profile-results.md) and the expanded
[Grok/Astra results with a clean native Astra baseline](grok-astra-profile-results.md)
before enabling this experiment. Results vary by model and workflow: Grok
became faster with mixed cost changes; completed Astra profile pairs became
slower. A consistent improvement in both time and cost has not been established.
The profile remains experimental and off by default.

## Configuration

Add this to an existing OpenAI Responses model entry, or use a separate alias
for an experiment:

```yaml
models:
  sol-efficient:
    provider: openai
    id: gpt-5.6-sol
    api: responses
    reasoning: effort
    reasoning_effort: high
    execution_profile: gpt-efficient-v1
```

Retain the context and output limits appropriate to your existing endpoint.
Launch with `gremlord --model sol-efficient`. `models add` also accepts
`--execution-profile gpt-efficient-v1`. An absent or empty setting leaves the
prompt unchanged. Unsupported names and non-Responses routes are rejected.
No existing model entry is enabled automatically.

The router's pre-dispatch context guard, backend input estimate, and token-count
endpoint include the supplement. The routing rule's earlier tier-selection
estimate still uses client-visible input; pin a model for comparisons. Changing
the profile changes the reasoning cache's instruction scope, causing a safe
miss rather than reusing reasoning generated under different instructions.

## Matched evaluation

The benchmark now accepts `-baseline` and `-mut` with `codex`, `gremlord`, and
`gpt-efficient` arms. Both Gremlord arms use the same current backend and Claude
Code configuration; only the selected arm receives the supplement. The API
meter checks profile presence, upstream model, and effort on every request.

Use the frozen queue task with repeated paired attempts, then the separate
dependency-planner workflow with the same unchanged profile. Both workflows
have three cumulative user turns. The planner adds resource scheduling and
atomic graph updates/snapshots after the initial dependency planner; its grader
checks prior contracts after each extension, including 24 fixed random DAGs.
Reference solutions must pass and broken starters must fail before live runs.

Artifacts record the profile and its hash, executable hash, source revision,
per-attempt/per-turn results, API requests, tool calls, tokens, and estimated
cost. Request telemetry adds response-header time, first nonempty output-delta
time, full-response time, and instruction/tool/visible-history byte sizes.
It also separates function-tool descriptions from parameter-schema bytes and
hashes the base instructions with the supplement removed. Compare these fields
between the two Claude Code arms; native Responses Lite encodes tools differently.
Hashes and counts are retained; request content and encrypted reasoning are not
written by the meter. First-output timing includes text, reasoning summaries,
or tool-argument deltas; it is not timing invisible reasoning tokens. Byte sizes
are not token estimates: instructions and tool descriptions count decoded UTF-8
text; tool schemas, parameter schemas, and visible input count serialized JSON.

The reporting script sums all repetitions and retains failed/missing stages.
An interrupted HTTP stream without terminal usage leaves some spend unknown;
the report marks that cost as a lower bound. A timed-out workflow completed
less work, so its raw time/cost is not successful-completion time/cost.
These synthetic workflows remain below the compaction threshold. Their results
cannot establish general parity with Codex or validate restart/compaction.

## Rationale

The earlier [matched results](gpt-benchmark-results.md) showed many more model
rounds and reasoning tokens in the Claude Code path. The
[GPT-5.6 prompting guidance](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.6#prompting-best-practices)
also recommends concise instructions and evaluating prompt changes separately.
This experiment tests a small supplement; it does not remove Claude Code's
existing instructions or assume that an added prompt will improve performance.
