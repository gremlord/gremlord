# Headless Codex worker pilot

Run date: September 17, 2026. This tests a Claude Code conversation delegating
three cumulative implementation requests to the **same native Codex thread**,
through Gremlord's metered Responses gateway. It tests the user's proposed
hybrid without replacing Claude Code as the outer harness.

These are two separate paired experiments, one attempt per arm. They use the
same dependency-planner fixture but different controls. Do not combine their
baselines into a simultaneous ranking. Costs below include both coordinator
and worker, use recorded local token prices, and are estimates rather than
provider invoices.

**Result:** all 12 cumulative checkpoints passed, but neither hybrid improved
time or cost against its paired control. Keep delegation optional and prioritize
Gremlord feature adapters for direct native GPT execution.

## Same-model delegation

| Arm | Cumulative checkpoints | Agent seconds | Estimated USD | API requests |
| --- | --- | --- | --- | --- |
| Claude Code / Astra through Gremlord | 3/3 | 543.5 | 2.1336 | 16 |
| Claude Code / Astra → native Codex / Astra | 3/3 | 771.3 | 4.0118 | 32 |

The hybrid used **1.42× the time and 1.88× the estimated cost**. Its coordinator
cost $1.0366 across 12 requests and 124.3 seconds of API time; the worker cost
$2.9752 across 20 requests and 666.9 seconds. Coordinator spend accounts for
55% of the extra dollars. The worker alone also cost more than the control.

Combined input grew from 428,123 to 904,292 tokens (including cache reads and
writes); output grew from 22,971 to 35,980. Reasoning tokens were similar:
7,480 versus 7,657. In this pair, additional context and output explain the bill
increase better than a claim of dramatically more reasoning. Worker output
alone was 32,299 tokens, so the difference is not only coordinator overhead.

| User turn | Claude/Astra seconds | Astra → Astra seconds |
| --- | --- | --- |
| 1: dependency validation and topological planning | 115.3 | 166.1 |
| 2: resource scheduling and affected tasks | 182.0 | 262.6 |
| 3: atomic graph updates and canonical snapshots | 246.2 | 342.6 |

## Cheaper coordinator versus clean Codex

| Arm | Cumulative checkpoints | Agent seconds | Estimated USD | API requests |
| --- | --- | --- | --- | --- |
| Clean native Codex / Astra | 3/3 | 355.6 | 1.6150 | 16 |
| Claude Code / Grok 4.6 → native Codex / Astra | 3/3 | 525.1 | 2.2272 | 21 |

The hybrid used **1.48× the time and 1.38× the estimated cost** in this pair.
Its Grok coordinator cost $0.1276 across six requests and 54.3 seconds of API
time. The native Astra worker cost $2.0997 across 15 requests and 467.3 seconds
of API time. The worker alone exceeded the clean baseline's cost/time.

The worker generated 22,762 output tokens versus clean Codex's 15,920 (+43%),
including 4,361 versus 2,562 reasoning tokens (+70%). The coordinator accounts
for about 21% of the dollar difference; the remainder is the worker's changed
trajectory. This is **not** a measurement of unavoidable orchestration overhead.
The delegated prompt and model choices differ, and a single realization can
vary. A cheap coordinator worked, but this run demonstrated no savings.

| User turn | Clean Astra seconds | Grok → Astra seconds |
| --- | --- | --- |
| 1: dependency validation and topological planning | 98.2 | 133.6 |
| 2: resource scheduling and affected tasks | 112.5 | 168.9 |
| 3: atomic graph updates and canonical snapshots | 144.9 | 222.7 |

## Method and audit

- The same workspace and parent conversation persist across three user turns;
  the native worker also resumes its explicit thread ID. Changed requirements
  are supplied in later turns, without cumulative grader feedback.
- External checks cover edge cases and 24 fixed random DAGs. Each checkpoint
  checks all requirements so far. This is a synthetic integration task, not a
  representative repository benchmark.
- Frozen Claude Code 2.1.271 and Codex 0.154.0 executables; isolated homes; no
  web, MCP, plugins, user customization or extra model subagents. Astra uses
  high effort and a 600,000-token declared context budget; Grok uses high
  effort and 500,000. Output cap: 32,768 tokens per response. There is no
  forced compaction in this task.
- Worker calls use the actual Gremlord native launcher and a $10 daily worker
  profile budget. The benchmark combines coordinator and worker usage under
  the same candidate and 64-request limit; each turn has an eight-minute cap.
- Astra rates per million: $10 ordinary input, $1 cache read, $12.50 cache
  write, $50 output. Grok: $2 / $0.50 / $2 / $6 respectively. Astra's write
  rate is an explicit benchmark override; user configuration is unchanged.
- Latency is the sum of user-turn agent durations, including worker waiting,
  excluding external grading. API durations include network and streaming;
  overlapping coordinator/worker API requests need not sum to agent time.
- Public evidence contains binary/source hashes, per-turn outcomes, usage and
  component audits. It excludes prompts, credentials and reasoning ciphertext.
  Private artifacts retain patches, graders, native events and router ledgers.

Both hybrids completed all three native calls on one thread, with no API errors
or streams missing terminal usage. Worker ledgers matched the independent meter
exactly, and totals included coordinator spend. The same-model run's overall
wall duration was 1,315.771 seconds versus 1,314.793 seconds of summed agent
time; per-turn wall/monotonic differences were at most six milliseconds. The
mixed-model run's overall wall duration was 881.819 seconds versus 880.735
seconds of summed agent time, plus orchestration/grading overhead. Neither
shows the suspension discrepancy seen in the excluded attempt.

The mixed-model binary predates the per-turn clock check; its runtime sources
otherwise match the replacement binary. Source snapshots/hashes are retained.
The final branch only changes comments in those runtime files after the run;
reporting and documentation were finalized from the completed artifacts.

Local validation passed: `go test ./...`, `go vet ./...`,
`go test -race ./scripts/gpt-bench`, six Python worker tests, both live ledger
audits, the excluded-run rejection check, and public artifact/link checks.

## Excluded attempts

`gremlord-hybrid-planner-v1` was blocked by the sandbox's loopback listener
restriction before any model call. `gremlord-hybrid-planner-v2` was interrupted
by host suspension and network failure: approximately 61 minutes of wall time
versus 1,001 seconds of summed recorded agent time. One hybrid stage had about
44 minutes of uncounted suspension. It logged seven API errors and one
successful HTTP stream without terminal usage; its native worker completed
only two of three calls, despite the final workspace passing the grader.

Both attempts are excluded from performance conclusions. The interrupted pair's
apparently clean control is also excluded; the entire pair was rerun. Its
known cost is a lower bound, not a valid successful-workflow cost. New benchmark
code records wall and monotonic time per turn and marks a run excluded when
they differ by more than five seconds; the reporter refuses excluded runs.

## What this means for features

Keeping Claude Code outside the worker preserves its existing integration
point, but does not certify messaging or classifier behavior while the worker
is busy. The worker is not a Claude messaging peer, and the classifier does not
automatically select it. Production parent/child cost rollup, mid-task steering,
progress and reliable cancellation remain follow-up work.

Keep the worker opt-in. The experiments support testing native execution with
shared Gremlord policies; they do not justify a mandatory coordinator or a
default harness migration. See the [direction decision](harness-direction.md),
[setup and reproduction](claude-codex-worker.md), and
[sanitized evidence](claude-codex-worker-results.json).
