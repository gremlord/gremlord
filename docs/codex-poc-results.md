# Native Codex PoC results

Measured 2026-09-16 with `gpt-6-astra`, high effort and Codex CLI 0.154.0.
Both the real Gremlord PoC executable and clean Codex passed all three
cumulative checkpoints of the dependency-planner workflow.

| Harness | Checkpoints | Agent seconds | API requests | Estimated USD |
| --- | --- | --- | --- | --- |
| Codex through Gremlord | 3/3 | 288.5 | 16 | $1.5941 |
| Clean Codex | 3/3 | 404.1 | 14 | $1.9311 |

The PoC used 0.71× the time and 0.83× the estimated cost in this pair.
This is an integration pilot, not evidence that a proxy makes Codex faster.
Both arms run the same native harness; their model decisions and generated
output vary. There is no observed 2× penalty in this run, but one paired
workflow cannot establish general performance parity.

## Task and controls

The same frozen [planner fixture](../scripts/gpt-bench/planner.py) used in
the earlier Astra comparison has three user turns in one resumed thread and
workspace: deterministic dependency planning, resource-constrained scheduling,
then atomic graph updates and portable snapshots. Each checkpoint retests
earlier behavior, edge cases and 24 fixed random DAGs. No grader feedback is
sent back to either model.

Both arms use the same frozen Codex binary, fresh homes/workspaces, no user
rules or integrations, no web/subagents, a 600,000-token context budget,
32,768 output-token limit, and the same shared upstream meter. Both retain
Responses Lite, `reasoning.context=all_turns`, high effort and default service
tier. Their first requests contain 8,659 and 8,656 input tokens respectively.
The PoC runs first; the clean baseline runs afterward. An idle-sleep inhibitor
was active for the pair. Automatic compaction was not triggered by this task.

| Turn | PoC seconds | Clean seconds | PoC / clean checkpoint |
| --- | --- | --- | --- |
| Dependency planning | 85.8 | 93.1 | pass / pass |
| Scheduling and affected tasks | 85.4 | 115.9 | pass / pass |
| Atomic updates and snapshots | 117.3 | 195.1 | pass / pass |

Times above sum the measured user-turn durations. The outer candidate timers
also include a small amount of orchestration overhead.

## Metering and profile

All 16 PoC requests appear in both Gremlord's own SQLite ledger and the
independent benchmark meter. Input, output, cache reads, cache writes and
estimated dollars reconcile exactly on each of the three turns. Neither arm
has an API error or a request with unknown terminal usage. Encrypted reasoning
is present in subsequent native requests, including resumed turns.

| Usage | PoC | Clean Codex |
| --- | --- | --- |
| Total input | 344,532 | 332,319 |
| Cached input | 306,181 | 288,509 |
| Cache writes | 38,303 | 43,768 |
| Output, including reasoning | 16,174 | 21,902 |
| Reasoning output | 2,388 | 6,693 |
| Metered upstream seconds | 285.1 | 402.0 |

The PoC generated fewer output tokens, especially reasoning tokens, despite
making two more requests. This helps explain this pair's cost/time difference;
it does not identify a repeatable proxy optimization. Time outside metered
API requests was approximately 3.4 seconds for the PoC and 2.1 seconds for
clean Codex, including local tools and process startup.

The nested request timers differ by 46 ms in aggregate. They timestamp before
the final database write, so that number is not a complete measurement of
gateway overhead. The ledger audit preserves it as a diagnostic only.

Rates are the prior pilot's USD-per-million estimates: input 10, output 50,
cache read 1, cache write 12.5. The write rate is explicitly configured in the
PoC benchmark profile. These are estimates, not invoices; production budgets
depend on the user's configured prices.

## Separate live compaction check

The final source also passed `TestCodexLiveCompaction` using stock Codex
app-server and the production Gremlord router. After one turn establishes
two requirements, the test explicitly triggers compaction and asks for both
requirements again. Both survive. Telemetry records one native
`compaction_trigger`, one replayed opaque `compaction` item, and three
successfully metered calls: 25,833 total input tokens (8,528 cached reads and
17,271 writes) and 203 output tokens.

This is a small continuity smoke, not a long-context quality benchmark.
The first test setup used an 8,192-token output cap; Astra rejected compaction
because it requires at least 20,000 when a cap is provided. The successful
test uses 32,768. Gremlord preserves the configured cap rather than raising
it silently. A separate fake-upstream test covers `/responses/compact`;
Astra's native compactor in this live check uses `/responses`.

## Reproducibility and exclusions

[Sanitized data](codex-poc-results.json) includes request metrics, ledger
audits, model settings, and binary/fixture hashes. It contains no prompts,
API credentials, encrypted reasoning or private provider URLs. Generate and
reconcile it with:

```sh
python3 scripts/gpt-bench/poc_report.py RUN_DIRECTORY public-results.json
```

Local raw artifacts are retained at `/tmp/gremlord-codex-poc-planner-v2`.
The benchmark uses a freshly built PoC binary whose SHA-256 is recorded in
the data. Subsequent hardening covers global CLI argument ordering,
unsupported endpoint fallback and rejection of `store:null`; those paths
are exercised by separate regression/installed-CLI tests, not this timing pair.

The first attempt at `/tmp/gremlord-codex-poc-planner-v1` is retained and
explicitly excluded. Root-level Codex configuration overrides were ignored
when `exec` supplied its own overrides, causing the PoC to attempt its default
endpoint and fail authentication. No PoC request reached the benchmark meter.
The completed clean arm from that attempt is not mixed into the replacement
pair. The fix places gateway settings in the selected command's scope, with
a real-CLI regression test for both `exec` and `exec resume`.
