# Gremlord harness direction

Decision: **keep Claude Code as the default today, and develop native Codex as
a first-class GPT execution path with Gremlord-owned routing, messaging and
accounting.** Keep headless Codex delegation available as an explicit option.
Do not make a second coordinating agent the default GPT performance fix, and
do not enable the generic GPT execution supplement by default.

This is a product direction supported by small integration experiments, not a
claim of general benchmark superiority. A default migration is not approved by
these results: the native Codex PoC still lacks important Gremlord features.

## What the experiments establish

| Experiment | Observation | Consequence |
| --- | --- | --- |
| Sol initial coding/context pilot | Both harnesses passed 6/6; Claude/Gremlord used 1.26× the time and 1.06× estimated cost | One-shot success hid a larger gap on longer work |
| Sol three-turn queue | Both passed 3/3; Claude/Gremlord used 2.29× the time and 2.07× cost | The reported 2× gap was real on this workflow |
| GPT execution supplement | Sol, Astra and Grok results varied by task; no consistent joint speed/cost improvement | Keep it experimental; more generic prompt tuning is not the next main investment |
| Astra native comparison | Across two queue repetitions, Claude/Gremlord used 1.28× time and 1.42× cost; planner used 1.67× time and 1.31× cost; both passed all nine checkpoints | The gap persists with Astra, but its magnitude is workload-dependent |
| Native Codex through Gremlord | Native launcher/gateway, usage reconciliation, resumed reasoning and a separate compaction smoke passed | Gremlord can retain API metering while letting GPT use its native execution loop |
| Astra coordinator + Astra worker | All 3/3 stages passed; hybrid used 1.42× time and 1.88× cost versus Astra inside Claude Code | A mandatory native worker did not fix this workflow's performance |
| Grok coordinator + Astra worker | All 3/3 stages passed; hybrid used 1.48× time and 1.38× cost versus clean Astra Codex | Cheap coordination was feasible, but changed worker behavior erased savings |

Sources: [Sol harness comparison](gpt-benchmark-results.md),
[Sol profile pilot](gpt-profile-results.md),
[Grok/Astra matrix](grok-astra-profile-results.md),
[native gateway results](codex-poc-results.md), and
[hybrid results](claude-codex-worker-results.md).

These are paired, synthetic local workflows, with repeated tasks and cumulative
external graders. They do not establish quality parity on arbitrary repositories.
Historical prices/meter versions differ; use within-pair comparisons rather than
pooling dollar totals across models and experiments. Infrastructure-interrupted
runs and unknown-usage streams must not become successful-performance evidence.

Most measured latency was in API requests, including network and streamed
delivery. The studies do not identify translator CPU as the main bottleneck.
Extra calls, repeated context and additional generated reasoning/output matter.
High cache-hit rates alone do not remove the cost of repeatedly sending context.

## What we keep

- Claude Code remains the working default for its existing tools, peer messaging,
  classifier routing and generated model subagents.
- Protocol fixes remain: parallel tool calls, encrypted reasoning replay,
  assistant phases and the related context/usage corrections.
- Native Codex remains opt-in for fixed GPT aliases. Its own context and tool
  loop avoid maintaining a second translated representation of that loop.
- Headless Codex remains an explicit whole-task worker. Give it a specific job
  and preserve its native thread on follow-ups. A GPT model alias in a Claude
  subagent definition alone does not select the Codex harness.

## What we build next

The feature boundary today is:

| Capability | Claude Code today | Native Codex PoC | Claude + Codex worker PoC |
| --- | --- | --- | --- |
| Classifier routing | Existing Messages routing | Fixed GPT alias | Coordinator routing remains; worker selection is explicit and its alias is fixed |
| Gremlord peer messaging | Existing integration | No Gremlord adapter | Outer Claude integration only; no direct worker peer |
| Cost and budget controls | Existing session/profile/global accounting | Native API usage metered with profile/global gates | Each component metered; combined parent total is benchmark-only |
| Context continuity | Translator reasoning replay and existing Claude context handling | Native resume; separate compaction smoke verified | Same native thread resumed through all three user turns; busy-worker steering unimplemented |

The outer Claude features were disabled in this isolated benchmark, so their
behavior during delegation still needs an integration test.

1. **A shared session/job identity and cost record.** Track parent, worker,
   harness, model, profile, status and native thread ID. Aggregate child spend
   into the parent view while retaining individual usage rows and budget gates.
   Include classifier and coordinator spend. Validate configured cache-write
   prices; the Astra experiments supply an explicit benchmark override.
2. **Messaging adapters with explicit delivery semantics.** Retain the Claude
   integration and add a supported Codex control path. Test messages to running
   and idle sessions, acknowledgements, interruption and resume. Native Codex
   subagent messaging is not a replacement for Gremlord's cross-session registry.
3. **Classifier routing that selects an execution engine as well as a model.**
   Reuse tier/task policy at a defined task boundary and keep a native worker's
   model stable through its tool loop. Preserve session context deliberately
   when a later decision changes models. Do not assume encrypted reasoning or
   compacted history is portable between models/providers. Validate compatibility
   or perform an explicit handoff; fail visibly rather than silently losing state.
4. **A feature-inclusive repository evaluation.** After the adapters work, test
   representative repository tasks with follow-up requirements, forced compaction,
   restart/resume, cross-session messages and budget limits. Compare native GPT,
   GPT inside Claude Code, and optional workers with the actual integrations on.

The intent is to choose a suitable native execution engine for each model family
while Gremlord owns the policies users care about. We have not measured a full
Anthropic-on-Codex reverse adapter, so there is no basis for making Codex the
single universal harness now. This direction starts with Codex's documented
interfaces rather than a fork; any deeper integration needs its own feasibility
check.

## Integration research

The inspected Codex CLI (0.154.0) already includes `queue --thread`, and its
source implements this through `thread/queue/add` on a local or remote app
server. It also supports attaching its TUI to that server. This is a promising
adapter route, not proof that Gremlord peer messaging works today. The native
Gremlord PoC currently rejects arbitrary remote endpoints to prevent bypassing
its gateway; an integration must keep the server on Gremlord's metered route.
See the inspected [queue implementation](https://github.com/openai/codex/blob/872fc22f9c41c3ba7f9fcc5dd8630b880ee713b8/codex-rs/tui/src/session_queue_commands.rs).

The [app-server documentation](https://learn.chatgpt.com/docs/app-server)
provides thread resume, turn start, steering, interruption and streamed events.
That is enough surface area to investigate progress, cancellation and delivery
adapters. Some methods/fields require experimental opt-in; pin versions and
verify delivery semantics. OpenAI's [migration notice](https://learn.chatgpt.com/docs/mcp-server)
says the old `codex mcp-server` command was removed and the app server remains
experimental and unsupported for production workloads. Do not build the next
adapter around old MCP-hosting examples or claim production readiness yet.

## Gate for changing the default

Require the user's messaging and classifier workflows to pass on the proposed
default, accurate parent/child accounting, reliable cancellation, and preserved
context after resume/compaction/model transitions. Then require repeated matched
repository results showing a useful cost/latency improvement at comparable task
success. Report failures and incomplete usage, and keep harness/model effects
separate when interpreting mixed-model workflows.

The immediate deliverable is an opt-in experiment and a clear migration gate.
The next implementation priority is routing/messaging/accounting parity for
native execution, rather than another universal prompt supplement or mandatory
coordinator layer.
