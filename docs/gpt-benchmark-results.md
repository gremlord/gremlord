# GPT-5.6 Sol: Gremlord versus native Codex

Measured locally on 2026-09-15 using Claude Code 2.1.271 and Codex CLI 0.154.0.
Both used GPT-5.6 Sol, high reasoning, a 600,000-token declared context budget,
and a 32,768-token per-response output cap. A shared proxy measured actual API
usage and preserved native request/response protocol headers. Native Codex used
Responses Lite; Gremlord used standard Responses function tools. Both routes
reported effective reasoning context `all_turns`.

## Initial coding and context pilot

Three synthetic tasks, two attempts each: incremental SSE decoding, an
out-of-order transactional ledger, and extracting/applying a policy from a
193 KB dossier. Grading used frozen external tests, with reference-solution
validation and broken-starter rejection before execution. All attempts had
fresh workspaces and homes, with web, MCP, customizations, and subagents disabled.
Claude retained its default prompt in safe mode. Pair order was seeded.

| Harness | Passed | Total agent time | API requests | Estimated token cost |
| --- | --- | --- | --- | --- |
| Native Codex | 6/6 | 509.5 seconds | 44 | $2.4729 |
| Gremlord | 6/6 | 640.2 seconds | 50 | $2.6229 |

Gremlord took 25.7% more total agent time and cost 6.1% more on this pilot.
The biggest latency difference was the ledger task: Gremlord took 188/178
seconds versus Codex's 111/118 seconds. Individual parser and context results
varied. These six attempts are repetitions of three tasks, not six independent
problems; they do not justify a general quality ranking.

No API or candidate execution failures occurred. Gremlord sent encrypted reasoning on
44/50 requests; Codex did so on 30/44. Requests without replay include initial
requests and histories where the model had not emitted reasoning. The API
returned 24,752 reasoning output tokens for Gremlord and 14,112 for Codex.

Artifacts: `.gremlord/evals/gpt-pilot-scored-v3/`. Its `report.md`, `summary.json`,
`requests.jsonl`, per-candidate patches/verifier logs, `environment.json`, and
`source-snapshot.tar.gz` preserve the evidence. The source archive includes the
uncommitted Go implementation; the benchmark executable hash is also recorded.

## Three-turn durable queue scenario

This separate scenario resumes the same session and workspace across three
user turns. Follow-up prompts contain new requirements without repeating the
earlier contract. The grader retains a database created by the first
implementation and checks it after later changes.

1. Durable SQLite queue, idempotency, priority order, atomic claims, fenced ack.
2. Expiry/recovery, fresh lease tokens, retry limits, renew/release/cancel.
3. Atomic batches and transactional migration of a legacy schema, preserving
   IDs, dedupe keys, payloads, scheduling and terminal state.

Concurrent workers use separate database connections. Every stage has its own
cumulative external grade and patch. Both harnesses receive identical scripted
follow-ups; grading feedback is not fed to either model.

| Harness | Checkpoints passed | Agent time | API requests | Estimated token cost |
| --- | --- | --- | --- | --- |
| Native Codex | 3/3 | 434.2 seconds | 27 | $1.6773 |
| Gremlord | 3/3 | 992.3 seconds | 55 | $3.4693 |

| User turn | Native Codex | Gremlord |
| --- | --- | --- |
| Initial durable queue | pass, 128.4 seconds | pass, 227.3 seconds |
| Recovery and fenced leases | pass, 160.5 seconds | pass, 324.5 seconds |
| Batches and migration | pass, 145.3 seconds | pass, 440.5 seconds |

All six cumulative checkpoints and both final graders passed. There were no
API or candidate execution failures. Individual shell/test commands did fail
and were repaired within the runs. Gremlord took **2.29× the agent time and 2.07× the
estimated token cost** on this workflow. Whole-workflow elapsed times, including
the short grading steps, were 434.8 and 992.9 seconds respectively.

Reasoning replay crossed actual user-turn boundaries: Gremlord's first requests
on turns 2 and 3 contained 23 and 52 encrypted reasoning items; Codex's contained
12 and 26. Every response reported `all_turns`. This verifies continuity across
follow-ups in the same session, not just tool calls within one user turn.

Gremlord made 71 tool calls versus Codex's 24, and emitted 54,378 output tokens
versus 32,104. Those totals included 29,547 versus 11,423 reasoning tokens.
Gremlord's persisted transcript contains 25 shell commands mentioning a test
runner or compiler, across seven distinct command strings. This motivates an
experiment on read/test loops; it does not prove each repeated invocation was
unnecessary or that prompt changes would preserve quality.

Artifacts: `.gremlord/evals/gpt-complex-scored-v1/`, including `report.md`,
`metrics.json`, `requests.jsonl`, every `turn-*/grade.json` and patch, the
persisted grading database, and a source archive with executable/source hashes.

### Where the extra time and cost went

Summing `duration_ms` at the shared upstream meter gives 982.6 seconds for
Gremlord and 425.6 for Codex. Total agent times were 992.3 and 434.2 seconds.
These request durations include networking and streamed delivery, not just
model computation; they are not a profiler of translator CPU time. Nevertheless,
almost all elapsed time was inside API round trips in both runs. Optimizing
local tool execution alone cannot explain away the observed difference.

| Estimated cost component | Native Codex | Gremlord | Gremlord excess |
| --- | --- | --- | --- |
| Uncached input | $0.2929 | $0.4700 | $0.1771 |
| Cached input | $0.4213 | $1.3681 | $0.9467 |
| Reasoning output | $0.3427 | $0.8864 | $0.5437 |
| Other output, including code/tool arguments | $0.6204 | $0.7449 | $0.1245 |

Cached input accounts for about 53% of the $1.7920 excess, and reasoning output
for another 30%. Gremlord's cache-hit fraction was actually higher: 96.7% versus
93.5%. The problem here is paying to reread a growing conversation on more
requests, plus generating more reasoning, despite good caching. It is not
evidence that a missing cache key caused the gap.

The trace supports experiments on the number of tool/model rounds and context
volume. It does not isolate prompt, tool schema, Responses Lite, or model
sampling as the cause. Both harnesses performed repeated tests; commands must
be interpreted alongside intervening edits before labeling them redundant.

## What the measurements support next

The protocol/context losses are fixed and exercised in real tool loops and
resumed sessions. **General Codex performance parity is not established.**
Even with all checks passing here, the longer workflow exposes a clear
efficiency gap. The experiment also does not isolate the improvements against
the old Chat Completions/no-reasoning configuration.

- Test a GPT-specific Claude Code prompt/profile that batches independent
  reads and uses focused test/stop criteria, without replacing its actual tool
  contracts. Compare it as a separate arm on held-out workflows before changing
  defaults.
- Retain native Codex as the reference arm. The product constraint for this
  follow-up is GPT inside Claude Code; the separate
  [native harness design](harness-eval-plan.md) is not the implementation path.
- Add a separate forced-compaction and restart benchmark. Neither completed
  scenario reaches the compaction threshold, and translator replay is still
  memory-only. Test persistent state and native compaction with explicit fork,
  retry, and context-edit boundaries before enabling them.

The implementation order and acceptance gates are in the
[GPT performance follow-up](gpt-performance-plan.md). It keeps GPT inside Claude
Code and tests execution guidance, repeated context volume, and compaction
separately. Parity cannot be promised from these traces.

## Interpretation and reproduction

Use [the benchmark instructions](../scripts/gpt-bench/README.md) to reproduce
both scenarios. Cost estimates use the rates saved with the run: $5/M uncached
input, $0.50/M cached input, and $30/M output. Input totals include cached tokens
once; output includes reasoning. These are estimates, not billing invoices.
Provider cache warmth affects cost and latency; cache usage is recorded.

Protocol/configuration preflights and interrupted runs are excluded. The first
meter did not preserve native Codex headers; a subsequent configuration trial
used a reserved provider ID. Neither contributes to scored results.

The pilot is not a before/after ablation, SWE-bench, or a compaction benchmark.
The longer workflow tests tool loops and user-turn continuity, while staying
below the compaction threshold. Native compaction, translator state persistence
across router restarts, and broad held-out repository quality remain untested.

The benchmarks used their own server built from the updated backend, not an
already-running Gremlord router. They compare updated Gremlord with native
Codex; they do not show whether the changes made Gremlord faster or slower than
its previous version. Updating an installed executable requires restarting any
existing router to use the new backend.
