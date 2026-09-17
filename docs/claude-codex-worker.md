# Claude Code coordinator with a native Codex worker

This experiment keeps Claude Code as Gremlord's main harness and delegates
whole implementation tasks to headless Codex. GPT uses its native prompt,
tools, reasoning history and compaction inside the worker. The coordinator
receives its final answer, without replaying every worker tool call through
the Anthropic translator.

Claude Code still owns the user conversation, tools and peer messaging. Its
existing Gremlord classifier can route ordinary Messages requests. **The
classifier does not automatically choose this worker.** Delegation is explicit,
at a task boundary. The worker stays on one GPT alias, avoiding migration of
opaque reasoning between models.

## Experiment

`claude-codex` is a benchmark arm, not a replacement default or new production
provider. Its Claude Code coordinator calls a resumable worker through Bash.
By default both use the same GPT model and high effort, so that comparison
isolates delegation rather than a cheaper coordinator model. Both costs count.
An optional `-coordinator-model` selects a different OpenAI-compatible alias
for the coordinator, with its own provider credentials, price and context limit.
That is a separate mixed-model experiment, not an isolated harness effect.

The worker invokes the real native Gremlord launcher from PR #33. API calls
pass through its Responses gateway and profile budget checks, then an
independent meter. Both components share the benchmark's 64-request limit.
The report separates their usage and reconciles worker calls with Gremlord's
own ledger.

The wrapper retains an explicit Codex thread ID across user turns. It rejects
concurrent use of one state directory, a changed directory/model/profile and
unexpected thread changes. After incomplete work, continuation requires an
explicit recovery flag; a failure does not silently start a fresh task.

## Try it from Claude Code

Build this branch and use a configured GPT Responses alias. This requires the
native gateway PoC; restart any older running router before using that gateway.

```sh
go build -o /tmp/gremlord-hybrid-poc .
python3 scripts/gpt-bench/hybrid_worker.py \
  --gremlord /tmp/gremlord-hybrid-poc \
  --model gpt-6-astra \
  --state-dir /tmp/gremlord-worker-my-task \
  --cwd /absolute/path/to/project <<'TASK'
Implement the complete task, including these requirements and acceptance checks.
Run the relevant tests and return a concise result.
TASK
```

Use the helper's absolute path when calling it from another project. Send the
next request to the same state directory to resume; use a new directory for
unrelated work. Prompts travel over stdin. Defaults permit workspace writes
and decline approval requests while preserving Codex configuration/rules.
Only the benchmark adds isolation and explicit sandbox-bypass flags, matching
the existing pilot.

Output is one JSON result with status, thread identity, resume status and final
answer. Detailed events/errors stay in the private state directory. Worker
billing uses Gremlord's API route, not a ChatGPT subscription. Unlike the
existing `cli` provider, this path meters worker API spend.

A named Claude subagent could wrap the helper. Setting only a subagent's
`model` to a GPT alias still uses Claude's execution loop. An extra LLM wrapper
could increase overhead; that variant is not measured here.

## Reproduce

Use the same frozen CLI executables and planner fixture as the earlier pilot.
Each output directory must be new. Runtime source and executable hashes are
retained, and the worker helper is embedded in the benchmark executable.

```sh
go build -o /tmp/gremlord-hybrid-bench ./scripts/gpt-bench
/tmp/gremlord-hybrid-bench \
  -model gpt-6-astra -cache-write-price 12.5 \
  -baseline gremlord -mut claude-codex \
  -gremlord-bin /tmp/gremlord-hybrid-poc \
  -manifest FIXTURES/manifest.yaml -sequence FIXTURES/sequence.json \
  -out NEW_RUN_DIRECTORY -attempts 1 -timeout 25m
python3 scripts/gpt-bench/hybrid_report.py NEW_RUN_DIRECTORY public-results.json
```

For the practical Grok coordinator/Astra worker comparison, keep
`-model gpt-6-astra`, add `-coordinator-model grok`, and use `-baseline codex`
to include a fresh clean Astra reference. The coordinator uses Responses for
this run even if its normal configured API is Chat Completions. User config
is not rewritten. Its separately recorded local prices are used for billing
estimates; the Astra cache-write override applies only to Astra.

Three cumulative planner stages cover dependency planning, resource scheduling,
and atomic graph updates/snapshots. External graders run after every turn;
their feedback is not returned to the models. Inspect every stage. This task
does not force compaction.

## Limits

- Claude Code retains the outer integration point for messaging and classifier
  routing. This benchmark disables external integrations and does not certify
  their behavior while a worker is busy.
- The Codex worker is not a Claude messaging peer. Direct mid-task messaging
  and steering need a bridge; follow-ups currently use resumed calls.
- Each worker launch has its own Gremlord session. Production parent-session
  cost rollup is not implemented; the benchmark explicitly includes both
  components. Worker profile/global gates still apply using configured prices.
- The caller supplies timeout/process-tree cleanup. A production task service
  needs cancellation, progress delivery and job lifecycle handling.
- Delegation adds coordinator tokens, handoff latency and potentially repeated
  verification. Fine-grained delegation may cost more. One synthetic workflow
  cannot establish general savings.

The [official OpenAI documentation](https://learn.chatgpt.com/docs/non-interactive-mode)
describes the JSON events, final-result files and explicit session resume
interfaces used here.
