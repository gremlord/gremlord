# GPT-5.6 Sol execution profile pilot

For the follow-up Grok 4.6 and GPT-6 Astra runs, including a fresh native
Codex baseline on Astra, see [the expanded results](grok-astra-profile-results.md).

Measured on 2026-09-16. GPT stays inside Claude Code in **both** arms. The
baseline is the updated backend after PR #31; treatment adds only the frozen
`gpt-efficient-v1` instruction supplement. This is not a test against an old
Gremlord process or a new native-Codex comparison.

The pilot does **not** establish a consistent improvement in both time and cost.
In the queue's one fully completed matched pair, the supplement reduced API calls
by 23.3%, but time by only 2.6% and estimated cost by 1.9%. The separate planner
pair was 21.1% slower and 13.1% cheaper with the supplement. The profile passed
all nine cumulative checkpoints across its three executions. Keep it
experimental and off by default; Codex performance parity remains unproven.

## Protocol and quality controls

Both arms use GPT-5.6 Sol at high effort, Claude Code 2.1.271, the same actual
tool schemas and permission configuration, a 600,000-token declared context
budget, and a 32,768-token response limit. Each candidate has an isolated home
and workspace, with one conversation resumed across three user turns. The
translator remains live between turns. The limit is eight minutes per user
turn, 25 minutes and 64 requests per candidate.

Every turn has a frozen, cumulative external grader. Models receive later user
requirements without grader feedback. Reference implementations pass and broken
starters fail before execution. The queue additionally preserves a database
created by the first implementation to test later compatibility. The planner
workflow was prepared separately after the profile was frozen; no prompt tuning
used its results. Runs are sequential, with seeded arm order; the two queue
repetitions use opposite orders.

## Durable queue: both repetitions

| Attempt | Arm | Checkpoints | Agent seconds | Requests | Tool calls | Estimated USD |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Baseline | 1/3; recovery timed out | 795.8 | 36 | 41 | ≥ $1.8686 |
| 1 | Profile | 3/3 | 725.3 | 34 | 41 | $2.1717 |
| 2 | Baseline | 3/3 | 981.4 | 43 | 57 | $2.6366 |
| 2 | Profile | 3/3 | 956.1 | 33 | 43 | $2.5872 |

| User turn | Baseline 1 | Profile 1 | Baseline 2 | Profile 2 |
| --- | --- | --- | --- | --- |
| Initial durable queue | pass, 315.8 s | pass, 158.3 s | pass, 276.0 s | pass, 273.9 s |
| Recovery and fenced leases | timeout, 480.0 s | pass, 262.0 s | pass, 309.0 s | pass, 242.9 s |
| Batches and migration | not run | pass, 305.0 s | pass, 396.3 s | pass, 439.3 s |

The profile completed both workflows and all six checkpoints. Baseline completed
one workflow and four of six checkpoints. The timed-out baseline completed less
work; its raw time/cost is not successful-completion time/cost. Do not average it
with a completed run to claim a speedup or cost saving. Repetitions of one task
are not independent tasks, and this sample does not establish a reliability gain.

The first baseline's recovery process reached the fixed eight-minute limit.
Its final HTTP stream was locally cancelled before terminal usage arrived, so
the recorded cost omits that request's unknown billed tokens. The driver used
for this run labeled the process kill `model_error`; artifacts retain that
original status. The next driver correctly preserves nested deadline causes
as `timeout` and keeps failed-stage elapsed time.

As a separate diagnostic, the frozen recovery grader passed all seven groups
on a copy of the stopped workspace and its persisted grading data. This was
after termination, without changing the original artifacts or supplying model
feedback. It suggests a valid patch existed at the deadline; it does not change
the timeout score or supply the missing third stage.

### Why fewer rounds barely changed the complete-pair result

| Metered component, attempt 2 | Baseline | Profile |
| --- | --- | --- |
| Uncached input tokens | 62,118 | 76,161 |
| Cached input tokens | 1,877,006 | 1,491,437 |
| Output tokens, including reasoning | 46,251 | 48,689 |
| Reasoning output tokens | 26,522 | 29,414 |
| Upstream request seconds | 977.9 | 952.5 |

Cached input fell 20.5%, but uncached input and reasoning output increased.
Reasoning rose 10.9%, and the migration stage was slower with the supplement.
Fewer API rounds alone did not produce a comparable reduction in time or cost.
Almost all agent time was spent in metered API calls, which include network
and streamed delivery time; this does not isolate server computation or prove
translator CPU is responsible.

## Separate planner workflow

The held-out workflow extends deterministic dependency planning with resource
waves/dirty propagation, then atomic graph updates and canonical snapshots.
Every stage rechecks prior behavior and includes 24 fixed random DAGs, alongside
explicit rollback, dependency barrier, validation, and snapshot-isolation cases.

| Arm | Checkpoints | Agent seconds | Requests | Tool calls | Estimated USD |
| --- | --- | --- | --- | --- | --- |
| Baseline | 3/3 | 339.5 | 31 | 33 | $1.2965 |
| Profile | 3/3 | 411.0 | 20 | 27 | $1.1263 |

| User turn | Baseline | Profile |
| --- | --- | --- |
| Dependency planning | pass, 81.7 s | pass, 124.1 s |
| Resource scheduling and affected tasks | pass, 117.7 s | pass, 99.8 s |
| Atomic updates and snapshots | pass, 140.2 s | pass, 187.1 s |

Both final graders also passed. There were no API/candidate execution failures
or missing usage records. The profile reduced requests by 35.5% and cached input
from 836,737 to 484,214 tokens, while reasoning output rose from 10,242 to 12,140
tokens. Total output was 21,683 tokens for baseline and 22,532 for profile.
Metered upstream time was 337.3 versus 409.1 seconds. Lower token cost did not
translate into lower latency in this pair; fewer calls are not sufficient to
predict completion time.

## Context measurements and next experiment

The planner meter separates base instructions from the supplement and separates
function descriptions from parameter schemas. The initial request has 3,578
UTF-8 instruction bytes and 58,535 serialized JSON bytes of tool definitions
for 17 tools. Descriptions alone contain 42,267 UTF-8 bytes; parameter schemas
contain 13,782 JSON bytes. The supplement adds 1,388 instruction bytes including
its separator. These are byte counts, not token estimates or additive measures
with identical encoding. Tool-schema hashes and base-instruction hashes match
across both arms on every planner request; the supplement is stripped only for
the base-instruction hash. All completed responses report `all_turns` reasoning
context, and replay crosses both follow-up boundaries in every complete run.

A separate compact-description experiment is the next concrete target. Preserve
tool names, argument schemas, behavior, permission semantics, and necessary
instructions; freeze and evaluate it separately. The present PR does not shorten
tool descriptions or arbitrarily discard tool output/history.

## Reproduction and limits

Use [the benchmark commands](../scripts/gpt-bench/README.md) with the queue and
planner fixtures and `-baseline gremlord -mut gpt-efficient`. Profile
configuration and scope are in [the execution profile guide](gpt-execution-profile.md).

Private artifact directories:

- `.gremlord/evals/gpt-profile-queue-v1/`: two pairs, source `6ded2f0`, executable
  SHA-256 `a16e7b1a65d603065b9d674d3abad9a956a964305966a8bc5cca8619a4f4ef1d`.
- `.gremlord/evals/gpt-profile-planner-v1/`: one pair, source `83add6c`, executable
  SHA-256 `b17f0f3e7fa702d8c83063da086b99202acc068977305fde7a0f2fdde9dd3a3b`.

Both contain source archives, executable/source hashes, environment metadata,
per-turn patches/grades, metered requests, and reports. The profile SHA-256 is
`9f3df4fc6633f249e703920959b8a189e6478761076756715d490c161a05fc08` in both runs.
The held-out driver adds component telemetry and the timeout-classification fix;
the profile, model, effort, limits, and translation behavior are unchanged.
Audits reconcile per-request token totals and cost against candidate usage.
The meter stores counts and hashes, not private prompts or encrypted reasoning.

All costs use the same recorded local rates as the earlier comparison:
$5/M uncached input, $0.50/M cached input, and $30/M output. They are normalized
estimates, not invoices or a claim about current advertised prices. This older
meter did not record cache writes separately, so their partition cannot be
recovered from these artifacts. Cancelled requests without final usage make the
recorded spend a lower bound. Agent time
excludes the short external grading steps. The two-turn preflight is excluded
from scored performance results.

This is a small synthetic pilot with model and cache variance. The largest
metered request contained 75,412 input tokens, below the context limit. It does not
test compaction, router restart, broad repository quality, or current native
Codex parity. The earlier native comparison remains a historical reference,
not a matched control for these runs. Defaults remain unchanged.
