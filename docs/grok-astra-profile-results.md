# Grok 4.6 and GPT-6 Astra: multi-turn benchmark

Measured 2026-09-16. This extends the [Sol profile pilot](gpt-profile-results.md)
with the same frozen `gpt-efficient-v1` supplement and tasks, and adds a fresh
native Codex baseline for Astra. GPT stays inside Claude Code in the two
Gremlord arms. The native comparison has its own fresh Gremlord controls.

**Fresh native Astra comparison:** both queue repetitions passed every
checkpoint in both harnesses. Across the two repetitions, Gremlord used
**1.28× the time and 1.42× the estimated cost**, with 76 API requests
versus 38 for native Codex. These are new Astra measurements, not a
before/after improvement claim against the historical Sol result.

| Native comparison | Gremlord / Codex time | Gremlord / Codex cost |
| --- | --- | --- |
| Queue repetition 1 | 1.36× | 1.49× |
| Queue repetition 2 | 1.20× | 1.36× |
| Dependency planner | 1.67× | 1.31× |

Native Codex and Gremlord each passed all nine checkpoints across three
completed workflows.

The full matrix contains nine pairs and 18 workflow candidates, with
53/54 cumulative checkpoints passed. Exclusions are listed below and retained
separately from scored results.

The supplement remains experimental and off by default. On Grok's strictly
matched queue pair it was 13.0% faster and 24.6% cheaper; on the planner it was
15.7% faster but 12.9% more expensive. On Astra's completed queue pair it was
18.2% slower and 4.2% cheaper; on the planner it was 10.5% slower and 7.9% more
expensive. These results do not support enabling the supplement universally.

See [the aggregate data](grok-astra-profile-results.json) for exact values,
per-turn results, request telemetry, and provenance. Costs are API estimates,
not invoices. Failed workflows retain their time and spend but receive no
successful-completion speed/cost ratio.

## What was held constant

Each comparison has two paired repetitions of a durable SQLite queue and one
pair of a dependency planner. Each workflow has three cumulative user turns
in one resumed conversation and workspace:

1. Queue: durable idempotent claims; then lease recovery and fencing; then
   atomic batches and legacy migration. A database created by the first
   implementation is retained for later compatibility checks.
2. Planner: dependency closure/topological order; then resource scheduling and
   affected tasks; then atomic graph updates and snapshots. Every stage checks
   earlier contracts, explicit edge cases, and 24 fixed random DAGs.

All arms receive the same requirements and frozen external graders, without
grader feedback between turns. Reference implementations pass and broken
starters fail before live evaluation. The profile was frozen before these runs
and never tuned using their results. Repetitions are not independent tasks.

Scored workflows run serially, with seeded arm order and opposite orders in
the two queue repetitions. Each candidate starts with a fresh home/workspace.
Effort stays high; caps are 32,768 output tokens per response, 64 API requests
per candidate, eight minutes per user turn, and 25 minutes per workflow. Grok
has a 500,000-token context budget; Astra has 600,000. Neither forces compaction.

Grok uses an explicit benchmark-only Responses override; its configured Chat
Completions route is unchanged. Both Claude arms use standard Responses and
parallel function calls. Native Codex retains Responses Lite, native tools,
and protocol headers; it sends `parallel_tool_calls: false` on this path. Its
home is clean, user config/rules are ignored, and web/MCP/plugins/skills/subagents
are disabled. This is a full harness comparison, not an isolated protocol test.
The meter's ordinary instruction/tool byte counters do not decode Lite's native
representation; zero values there do not mean Codex has no instructions/tools.
Use returned token usage for the cross-harness input comparison.
Tool-call counts are upstream function/custom-call items. A native code-mode
call can execute several local tools, so these counts do not measure identical
units of local work across harnesses.

`gremlord` below means the current backend with no execution supplement.
`gpt-efficient` adds only the frozen supplement. `codex` means clean native
Codex on the same upstream Astra model and effort.

## Astra: clean Codex versus Gremlord

| Workflow | Attempt | Arm | Checkpoints | Agent seconds | API calls | API tool-call items | Estimated USD |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Queue | 1 | codex | 3/3 | 663.8 | 19 | 16 | $3.1274 |
| Queue | 1 | gremlord | 3/3 | 904.6 | 36 | 40 | $4.6514 |
| Queue | 2 | codex | 3/3 | 700.0 | 19 | 16 | $3.3363 |
| Queue | 2 | gremlord | 3/3 | 837.1 | 40 | 42 | $4.5275 |
| Planner | 1 | codex | 3/3 | 274.3 | 15 | 12 | $1.6330 |
| Planner | 1 | gremlord | 3/3 | 456.8 | 20 | 26 | $2.1426 |

| Workflow | Attempt | Arm | Turn 1 | Turn 2 | Turn 3 |
| --- | --- | --- | --- | --- | --- |
| Queue | 1 | codex | pass, 175.1 s | pass, 185.0 s | pass, 303.7 s |
| Queue | 1 | gremlord | pass, 183.1 s | pass, 365.7 s | pass, 355.8 s |
| Queue | 2 | codex | pass, 183.0 s | pass, 227.7 s | pass, 289.3 s |
| Queue | 2 | gremlord | pass, 158.8 s | pass, 307.0 s | pass, 371.3 s |
| Planner | 1 | codex | pass, 76.2 s | pass, 83.9 s | pass, 114.1 s |
| Planner | 1 | gremlord | pass, 113.0 s | pass, 200.1 s | pass, 143.7 s |

| Workflow | Attempt | Arm | Total input | Cache reads | Cache writes | Output | Reasoning (included) | Upstream seconds |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Queue | 1 | codex | 595,843 | 534,614 | 61,172 | 36,551 | 9,773 | 659.1 |
| Queue | 1 | gremlord | 1,582,292 | 1,510,695 | 70,768 | 44,956 | 18,355 | 886.0 |
| Queue | 2 | codex | 618,189 | 554,352 | 63,780 | 39,682 | 10,920 | 696.4 |
| Queue | 2 | gremlord | 1,663,810 | 1,599,034 | 63,867 | 42,420 | 17,686 | 818.9 |
| Planner | 1 | codex | 324,179 | 283,843 | 40,291 | 16,902 | 2,901 | 272.1 |
| Planner | 1 | gremlord | 538,862 | 503,482 | 34,870 | 23,963 | 8,966 | 454.7 |

## Where the native queue gap comes from

Across both queue repetitions, every checkpoint passed in both harnesses.
Gremlord used 1.28× the agent time and 1.42× the estimated cost of native Codex.
It made 76 API requests versus 38, processed 3,246,102 input tokens versus
1,214,032, and read 3,109,729 cached tokens versus 1,088,966. Its input cache-hit
rate was 95.8%, yet the repeated cached input still cost more.

Of Gremlord's $2.7152 additional estimated cost, $2.0208 (74.4%) came from extra
cache reads, $0.5572 (20.5%) from output, $0.1210 (4.5%) from cache writes, and
$0.0162 (0.6%) from ordinary input. Reasoning output was 36,041 tokens versus
20,693 (1.74×); non-reasoning output was lower at 51,335 versus 55,540. The
extra total output was therefore additional reasoning, despite less visible
output in aggregate.

This supports testing both smaller repeated context and fewer unnecessary
rounds. It does not prove which prompt or tool-protocol difference caused the
behavior. Both code-mode batching and different tool instructions are potential
contributors that need their own controlled experiment. Merely improving the
cache-hit percentage would miss the main measured input-cost difference.

Metered API time totaled 1,704.9 seconds for Gremlord and 1,355.5 for Codex.
The remaining agent time was 36.7 and 8.4 seconds respectively. Most of the
latency difference occurred within API requests, whose duration includes
networking and streamed delivery; this does not isolate model compute or
translator CPU.

The aggregate data also records retained patch line counts. These are final
patch footprints, not all tests performed or a quality ranking. For example,
the first Gremlord control retained 1,390 added test lines and 276 implementation
lines, versus Codex's 995 and 292. Both passed the same external cumulative
graders. API tool-call items are also not equivalent units of local work,
because a native code-mode call can batch several actions.

The separate planner shows a different mix: Gremlord made 20 API requests
versus Codex's 15 and generated 8,966 reasoning tokens versus 2,901 (3.09×).
Total output was 23,963 versus 16,902 tokens. Output added $0.3531 and cache
reads added $0.2196, partly offset by $0.0678 lower write cost. The result was
1.67× the time and 1.31× the cost, with all three checkpoints passed in both
arms. Reducing repeated input alone would not address its larger output bill.

## Grok: baseline versus profile

| Workflow | Attempt | Arm | Checkpoints | Agent seconds | API calls | API tool-call items | Estimated USD |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Queue | 1 | gremlord | 3/3 | 831.1 | 23 | 28 | $1.1268 |
| Queue | 1 | gpt-efficient | 3/3 | 765.1 | 23 | 26 | $1.1493 |
| Queue | 2 | gremlord | 3/3 | 850.1 | 23 | 30 | $1.4088 |
| Queue | 2 | gpt-efficient | 3/3 | 739.8 | 21 | 24 | $1.0617 |
| Planner | 1 | gremlord | 3/3 | 618.8 | 17 | 20 | $0.6955 |
| Planner | 1 | gpt-efficient | 3/3 | 521.6 | 19 | 24 | $0.7853 |

| Workflow | Attempt | Arm | Turn 1 | Turn 2 | Turn 3 |
| --- | --- | --- | --- | --- | --- |
| Queue | 1 | gremlord | pass, 214.7 s | pass, 194.1 s | pass, 422.3 s |
| Queue | 1 | gpt-efficient | pass, 211.6 s | pass, 201.7 s | pass, 351.8 s |
| Queue | 2 | gremlord | pass, 266.9 s | pass, 271.0 s | pass, 312.2 s |
| Queue | 2 | gpt-efficient | pass, 218.9 s | pass, 212.5 s | pass, 308.4 s |
| Planner | 1 | gremlord | pass, 156.2 s | pass, 236.9 s | pass, 225.7 s |
| Planner | 1 | gpt-efficient | pass, 155.7 s | pass, 198.7 s | pass, 167.3 s |

| Workflow | Attempt | Arm | Total input | Cache reads | Cache writes | Output | Reasoning (included) | Upstream seconds |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Queue | 1 | gremlord | 1,312,135 | 1,206,656 | not recorded | 52,080 | 30,311 | 826.8 |
| Queue | 1 | gpt-efficient | 1,354,379 | 1,245,184 | not recorded | 51,386 | 32,153 | 762.8 |
| Queue | 2 | gremlord | 1,334,720 | 1,059,968 | not recorded | 54,884 | 32,845 | 847.1 |
| Queue | 2 | gpt-efficient | 1,002,001 | 818,688 | not recorded | 47,626 | 28,224 | 737.6 |
| Planner | 1 | gremlord | 688,665 | 616,064 | not reported | 40,372 | 25,420 | 617.3 |
| Planner | 1 | gpt-efficient | 692,942 | 530,048 | not reported | 32,421 | 20,169 | 520.0 |

## Astra: baseline versus profile

| Workflow | Attempt | Arm | Checkpoints | Agent seconds | API calls | API tool-call items | Estimated USD |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Queue | 1 | gremlord | 3/3 | 711.4 | 27 | 35 | $3.5514 |
| Queue | 1 | gpt-efficient | 3/3 | 840.9 | 28 | 33 | $3.4007 |
| Queue | 2 | gremlord | 2/3 | 1132.8 | 42 | 52 | $5.6742 |
| Queue | 2 | gpt-efficient | 3/3 | 769.7 | 29 | 39 | $3.7826 |
| Planner | 1 | gremlord | 3/3 | 349.2 | 17 | 23 | $1.9046 |
| Planner | 1 | gpt-efficient | 3/3 | 386.0 | 20 | 26 | $2.0558 |

| Workflow | Attempt | Arm | Turn 1 | Turn 2 | Turn 3 |
| --- | --- | --- | --- | --- | --- |
| Queue | 1 | gremlord | pass, 150.2 s | pass, 220.6 s | pass, 340.6 s |
| Queue | 1 | gpt-efficient | pass, 146.4 s | pass, 320.6 s | pass, 374.0 s |
| Queue | 2 | gremlord | pass, 251.5 s | pass, 401.3 s | execution error, 480.0 s |
| Queue | 2 | gpt-efficient | pass, 139.1 s | pass, 282.7 s | pass, 348.0 s |
| Planner | 1 | gremlord | pass, 90.1 s | pass, 110.5 s | pass, 148.6 s |
| Planner | 1 | gpt-efficient | pass, 89.2 s | pass, 151.8 s | pass, 145.0 s |

| Workflow | Attempt | Arm | Total input | Cache reads | Cache writes | Output | Reasoning (included) | Upstream seconds |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Queue | 1 | gremlord | 1,007,339 | 941,102 | 65,587 | 35,680 | 13,376 | 708.0 |
| Queue | 1 | gpt-efficient | 957,706 | 901,388 | 55,649 | 35,941 | 14,886 | 837.6 |
| Queue | 2 | gremlord | 2,225,137 | 2,147,678 | 76,509 | 51,213 | 20,977 | 1093.1 |
| Queue | 2 | gpt-efficient | 1,159,831 | 1,093,569 | 65,573 | 37,249 | 14,384 | 764.9 |
| Planner | 1 | gremlord | 456,340 | 407,824 | 48,065 | 17,830 | 3,468 | 347.3 |
| Planner | 1 | gpt-efficient | 520,840 | 475,301 | 45,029 | 20,250 | 5,516 | 384.1 |

## Interpreting the profile results

Grok passed all nine checkpoints in each arm. Queue attempt 1 was 7.9% faster
and 2.0% more expensive with the profile, but crossed Claude versions and is
not a strict prompt-only comparison. In the same-version attempt 2, the profile
was 13.0% faster and 24.6% cheaper. Its planner result was 15.7% faster and
12.9% more expensive: output fell from 40,372 to 32,421 tokens, while cached
reads fell and input outside cached reads increased. Fewer output tokens reduced cost in
one component, but the input mix more than offset it. The profile made 19 API
calls versus 17 in that planner pair, so fewer requests do not explain its
lower elapsed time.

Astra's profile passed 9/9 checkpoints; baseline passed 8/9, with one validation
timeout. Among fully completed pairs, queue attempt 1 was 18.2% slower and
4.2% cheaper, and the planner was 10.5% slower and 7.9% more expensive. Do not
average the failed queue baseline into a successful-completion ratio.

The failed baseline's migration stage reached the eight-minute deadline while
two Bash test commands were pending. All 42 upstream requests had terminal
usage, so its $5.6742 estimate is complete for recorded API activity, unlike a
cancelled stream with missing usage. As a separate diagnostic, the frozen
phase-3 grader passed all ten groups in 0.15 seconds on a copy of the stopped
workspace and its persisted grading data. A valid patch existed at the deadline;
the workflow had not finished validation. Its original timeout score remains
unchanged. This small sample does not demonstrate a model-quality or reliability
gain from the supplement.

For Astra's first profile queue run, cache reads were 94.1% of input. Reported
writes were close to the final request's input size, rather than repeatedly
rewriting the entire history. Output accounted for 52.8% of estimated cost,
cached reads 26.5%, and writes 20.5%. A high cache-hit rate does not make repeated
context free, and improving cache hits alone cannot remove the output bill.

Most time in the completed profile pairs was spent in metered API requests.
Those durations include network and streamed delivery, not just model compute.
The measurements support reducing total generated work and repeated context;
they do not establish translator CPU as the bottleneck.

## Billing and provenance

All flat rates below are USD per million tokens. Ordinary input equals total
input minus cache reads and cache writes. Reasoning tokens are already included
in output and are not charged again.

| Model | Ordinary input | Cache read | Cache write | Output |
| --- | --- | --- | --- | --- |
| Grok 4.6 | $2 | $0.50 | $2 | $6 |
| GPT-6 Astra | $10 | $1 | $12.50 | $50 |

Rates were checked against the [Astra model page](https://developers.openai.com/api/docs/models/gpt-6-astra),
[OpenAI caching documentation](https://developers.openai.com/api/docs/guides/prompt-caching),
and [Grok model page](https://docs.x.ai/developers/models/grok-4.6). The meter uses
recorded flat rates, not an invoice API. All scored Astra responses report the
default service tier, with input below its 272,000-token higher-price threshold.
Grok requests stay below its 200,000-token threshold and use the standard endpoint.
Fresh client homes do not reset provider caches.

The corrected benchmark meter separates reported cache writes from ordinary
input and records service tiers. All scored Astra runs use it with an explicit
`-cache-write-price 12.5` override because local pricing omitted a write rate.
This does not rewrite local pricing or change production usage accounting.
Production Responses accounting still needs equivalent cache-write handling.
The earlier Grok queue meter did not record writes, but its numerical estimate
is unchanged because ordinary and write rates are equal. xAI did not report a
write counter in the later run; absence is not evidence of zero cache writes.
Historical Sol results retain their recorded normalized rates and cannot recover
an unrecorded write partition.

| Artifact | Revision or SHA-256 |
| --- | --- |
| Grok queue driver source | `1496db0beaa04ffbe33fccfc583d3da51c735668` |
| Grok queue executable | `9e6dad3abe32562f0ead4760ad7e8b8f226a183dc1e258ed50c12be5d8ffed2f` |
| Remaining driver source | `4d584adcc9ac7d6aa34566e63fc69a54a1b17b09` |
| Remaining executable | `c19710f56602261c56dc6254851ab309858498c28030b8bbbbf41678242a1b12` |
| Frozen profile | `9f3df4fc6633f249e703920959b8a189e6478761076756715d490c161a05fc08` |
| Queue grader | `133ac58e866b0da9330023cb01f20b90b8df20036203ac87cb17fbf0f30b2b9c` |
| Planner grader | `3999879c1046dc7d67a1903ee13be0761a0ea49e3a4ee5134a0d2c0d3e2155b6` |
| Pinned Claude Code 2.1.271 | `87d119eb46782a1369d79d6e8cc00557f1b51bc9db7a51bd01fa2f909d8fe3dc` |
| Pinned Codex 0.154.0 | `4f85982624b3898c8991cb80c0981b2aa71070e3537046c9a95950318a95afcc` |
| Pinned Codex code-mode host | `426d73aaeb2aeef45e98b5add99e8ef9594a31673d281489bb2cb06b38c27423` |

Claude's global symlink auto-updated during the Grok queue run. Attempt 1's
profile used 2.1.271; its baseline and both attempt 2 candidates used 2.1.273.
Local fake-server captures reproduced the exact instruction hashes: only the
attribution-version line differs, and tool-schema hashes match. Attempt 1 is
retained and labeled cross-version; attempt 2 is the strictly matched pair.
All subsequent scored runs use frozen binary copies of Claude 2.1.271 and
Codex 0.154.0. Within those Claude comparisons, base instructions and tool
schemas match; the profile adds 1,388 UTF-8 instruction bytes.

The first Grok planner run was interrupted during profile turn 2 when the
orchestrating agent stopped its scheduler; process cleanup also killed the
child. This was an infrastructure mistake, not a model failure. Its baseline
passed 3/3 stages and profile passed 1/3 before interruption. Original artifacts
remain in `grok46-profile-planner-v1`, with at least $1.010294 recorded spend and
an unknown in-flight charge. The entire pair was rerun in `-v2`, without changing
prompts or graders.

The first native Astra queue/planner directories were also excluded due to an
orchestration mistake: the pinned Codex executable initially lacked its required
companion `codex-code-mode-host`. Codex reported code mode unavailable and tools
failed closed. The scheduler briefly advanced to the planner when the stopped
driver returned a successful process exit. Both invalid directories are retained,
with at least $5.476761 and $0.1309105 recorded spend respectively; interrupted
requests have unknown additional usage. The complete pinned bundle is validated
with a resumed file-editing preflight before full replacements in `-v2`. The
successful Gremlord control from the invalid pair is not substituted into a
replacement pair. Four short preflights are retained and excluded from scored
performance. These setup failures are not model-quality scores.
The repair preflight's two-turn graders all passed; its reused one-shot final
check expected 42 although the sequence ended at 43. That mismatch is retained,
and the correct final sequence contract was checked directly for both arms.
No scored fixture or verifier was changed.

The first repaired native planner pair (`astra-native-planner-v2`) also remains
excluded. macOS suspended during its Gremlord control: wall-clock event gaps
were 9–17 minutes while Go's monotonic timer paused. Both candidates passed
3/3 checkpoints, but one response lacked terminal usage; at least $3.968255
was recorded for the pair. The full pair was replaced in `-v3` under a temporary
`caffeinate -i` assertion, with an explicit wall-versus-monotonic check. Original
APFS creation timestamps independently check every scored stage: turn-directory
creation immediately precedes the timer, and stdout-file creation immediately
follows it. All 54 scored checkpoint clocks passed, with a maximum wall-versus-recorded
difference of 0.406 seconds. The full replacement planner's wall and monotonic
durations differed by 0.009 seconds, with no new sleep/wake event.

Private artifacts are under `.gremlord/evals/`, using the directory names in
the aggregate JSON. They include exact source archives, environment and binary
hashes, patches, grades, request counts, and audits. The public dataset exports
counts/hashes, not private prompts, API credentials, or encrypted reasoning.
Reproduction commands are in [the benchmark guide](../scripts/gpt-bench/README.md).

## Context handling and the next experiment

Encrypted reasoning replay crosses follow-up boundaries in the completed
Gremlord workflows. Astra reports `all_turns`; xAI returns encrypted items but
does not report an effective reasoning-context field, so this study cannot
assert the same server-side policy for Grok. Continuity is working within these
live sessions; restart, edited history, and forced compaction remain untested.

Codex source research identifies a concrete difference: it
[bounds retained function/custom-tool output](https://github.com/openai/codex/blob/872fc22f9c41c3ba7f9fcc5dd8630b880ee713b8/codex-rs/core/src/context_manager/history.rs#L429)
using model/per-tool policies, and its
[shell tool has a default output budget](https://github.com/openai/codex/blob/872fc22f9c41c3ba7f9fcc5dd8630b880ee713b8/codex-rs/core/src/unified_exec/mod.rs#L79).
Gremlord's translator has no equivalent output cap, although Claude Code may
already bound results. These runs do not establish that oversized tool output
caused their performance gap: the Grok queue's largest result was below 29 KB.

The next controlled experiment should compact repeated tool descriptions while
preserving tool names, schemas, required instructions, behavior, and permissions.
Test bounded, retrievable tool output separately under actual context pressure.
Keep full output accessible and preserve errors, task state, and call/result
pairs. Arbitrarily dropping history or encrypted reasoning is not a measured fix.

This suite contains only two synthetic workflows and no forced compaction.
It does not establish broad repository quality, statistical reliability gains,
or universal Codex parity. API durations include network and streamed delivery;
they do not isolate server computation or translator CPU. Defaults stay unchanged.
