# Matched GPT harness pilot

This opt-in Unix driver compares **Claude Code through the current Gremlord OpenAI
Responses backend** with **native `codex exec`**, using the same configured
upstream model, `high` effort, and a shared metering proxy. It makes billable API
requests and lets both coding CLIs edit fresh fixture workspaces and execute
commands. Run only trusted manifests on a development machine.

```sh
python3 scripts/gpt-bench/fixtures.py prepare .gremlord/evals/fixtures-RUN
go build -o /tmp/gremlord-gpt-bench ./scripts/gpt-bench
/tmp/gremlord-gpt-bench \
  -model gpt-5.6-sol \
  -manifest .gremlord/evals/fixtures-RUN/manifest.yaml \
  -out .gremlord/evals/results-RUN -attempts 2 -timeout 6m
python3 scripts/gpt-bench/report.py .gremlord/evals/results-RUN
```

Use new directories each time. There is no resume path that could mix different
code or harness versions. API credentials are read from the local Gremlord
configuration in the parent process. Child CLIs receive only an ephemeral
loopback proxy token. The proxy logs usage and counts of encrypted reasoning
items, never ciphertext or request/response bodies. CLI artifacts can contain
task data; the artifact root is private and gitignored.

The fixture generator freezes a copy of the external grader and records hashes,
rejects all three broken starters, and checks all three reference solutions.
The tasks cover incremental byte-stream parsing, out-of-order transactional
state updates, and extracting a policy from a 193 KB initial-context dossier.
Candidate patches and exact verifier output are retained. Repeated attempts use
the same tests and tasks; they are repetitions, not six independent problems.

The driver reuses Gremlord's local eval workspace, pairing, grading, and usage
pipeline. It invokes the native CLI through an injected executor; it does not
add inbound Responses support or a Codex mode to the production router. Both
harnesses have fresh homes, no user customizations/MCP/web/subagents, a 600,000
token declared context budget, and a 32,768-token per-response output cap.
Claude keeps its standard prompt in safe mode; native Codex keeps its own
prompt/tools and OpenAI provider identity. Native request and response protocol
headers (including Responses Lite and turn state) pass through the meter.
Limits: 64 API requests and the specified wall time per candidate.
The runner kills the candidate's process group on timeout.

This pilot can detect protocol failures and compare task results, latency,
tokens, and estimated spend. It cannot establish general Codex parity, isolate
which translator change improved quality, or evaluate native compaction. The
long-context task is below the configured compaction threshold. For stronger
quality evidence, run a larger held-out repository benchmark with the official
graders, repeated trials, and matched hardware/tool access.

## Multi-turn development scenario

The queue scenario resumes the **same conversation and workspace** for three
separate user turns. Later prompts only supply changed requirements. Each turn
has an external cumulative verifier, its own patch, and request/usage telemetry.
No grading feedback is fed to either model between turns. A SQLite database
created by the first implementation is retained outside the workspace and
checked again after later changes.

1. Build a durable, idempotent SQLite job queue with atomic claims.
2. Add expired-lease recovery, fenced tokens, retry limits, renew/release/cancel.
3. Add atomic batches and migration from a specified legacy schema.

The grader tests concurrent connections, stale-owner rejection, expiry
boundaries, all-or-nothing batch validation, priority order, duplicate keys,
reopening the database, and preservation of nonconsecutive legacy IDs.

```sh
python3 scripts/gpt-bench/complex.py prepare .gremlord/evals/queue-fixtures-RUN
go build -o /tmp/gremlord-gpt-bench-multiturn ./scripts/gpt-bench
/tmp/gremlord-gpt-bench-multiturn \
  -manifest .gremlord/evals/queue-fixtures-RUN/manifest.yaml \
  -sequence .gremlord/evals/queue-fixtures-RUN/sequence.json \
  -out .gremlord/evals/queue-results-RUN -attempts 1 -timeout 25m
python3 scripts/gpt-bench/sequence_report.py .gremlord/evals/queue-results-RUN
```

Each user turn has an additional eight-minute cap. Native histories are saved
inside the private artifact home to support resume; Claude history and the
translator's in-memory replay cache remain live across turns. Inspect every
`turn-*/grade.json`, not just the final candidate score: a later fix must not
erase an earlier failed stage. This scenario still does not force compaction.

## Execution profile A/B test inside Claude Code

Both arms below run Claude Code through the current backend. The only treatment
is the versioned `gpt-efficient-v1` supplement. The baseline explicitly disables
any execution profile configured on the selected model. Model and effort stay
identical; native Codex remains available as a separate reference arm.

```sh
/tmp/gremlord-gpt-bench-multiturn \
  -baseline gremlord -mut gpt-efficient \
  -manifest .gremlord/evals/queue-fixtures-RUN/manifest.yaml \
  -sequence .gremlord/evals/queue-fixtures-RUN/sequence.json \
  -out .gremlord/evals/profile-results-RUN -attempts 2 -timeout 25m
python3 scripts/gpt-bench/sequence_report.py .gremlord/evals/profile-results-RUN
```

Generate the separate planner workflow with
`python3 scripts/gpt-bench/planner.py prepare .gremlord/evals/planner-fixtures-RUN`,
then use that directory's manifest and sequence in the same command. Freeze
the profile before running it. Do not tune on its results and still call it
held out. `sequence_report.py` aggregates repeated attempts and retains missing
and failed stages, including time spent on execution failures.

The meter records instruction/tool hashes, serialized byte sizes, response-header
time, and first output-delta time in addition to usage. It asserts the selected
profile reached the API. The profile text/hash and executable hash are saved in
the run directory. See [the profile documentation](../../docs/gpt-execution-profile.md).

For another configured model, pass `-model ALIAS`. If its normal route is Chat
Completions, explicitly add `-api responses` for this run; the user's configuration
is not rewritten. `-context-budget` defaults to 600,000 and is capped at the
configured model window (500,000 for Grok 4.6). Both arms use that same budget;
the artifact records the configured/effective API and requested/effective budget.
High effort, the response limit, and the exact profile text stay unchanged.

For a fresh native reference, run another pair with `-model gpt-6-astra
-baseline codex -mut gremlord` on the same manifest/sequence. Each pair gets fresh
homes and workspaces; do not reuse an existing native conversation. Retain that
pair's additional Gremlord baseline rather than silently averaging it with the
separate profile comparison.

The meter records `input_tokens_details.cache_write_tokens` separately when the
provider returns it. Ordinary input is total input minus cache reads and writes;
do not charge writes twice. If local pricing omits a write rate, supply a
benchmark-only override, such as `-cache-write-price 12.5` for GPT-6 Astra at
the published September 2026 standard rates. The recorded rate and override are
saved in `environment.json`; the user's pricing configuration is unchanged.
Requested/returned service tiers are recorded to detect billing-mode differences.
The meter uses the recorded flat rates; check context-size tiers before treating
any long-context result as an invoice estimate. Historical runs without the
write counter priced non-cached input as one bucket and cannot recover its split.
