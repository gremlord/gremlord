# Plan: evaluating Codex vs Claude Code as the base harness

Status: proposal, 2026-09-08. Phase 0 implemented (uncommitted) 2026-09-08; Phases 1–5 not started.

## The question

gremlord wraps Claude Code and swaps what is behind `ANTHROPIC_BASE_URL`.
OpenAI's Codex CLI is now Apache-2.0, Rust, and has first-class custom
providers. Is its agent loop a better *harness* than Claude Code's — better
patches per dollar on the same model — and should gremlord front it too?

Nothing we have today answers this. `gremlord eval` holds Claude Code
constant and varies the model (`internal/eval/eval.go:624`,
`internal/eval/docker.go:212`). CLI aliases are rejected as candidates
(`cmd/evalcmd.go:136-146`), and the `codex` provider in `internal/backend/clibe`
is whole-task delegation from *inside* a Claude Code session — a composed
system, not native Codex.

The answer has to be measured. This plan makes the harness a factor in the
existing evaluator and describes the experiment that isolates it.

## Design principles

1. **Reuse the evaluator.** Pairing, seeded launch order, blinded judge,
   infra-vs-model failure taxonomy, official SWE-bench grading, artifacts,
   and `--resume` all carry over. Only the candidate's *launch* is
   harness-specific.
2. **Harness × model is a crossed design, not a single pair.** A single
   "GPT on Codex vs GPT on Claude Code" run confounds the harness with the
   translation hop: Claude Code reaches OpenAI through gremlord's
   Messages→Responses translation (which drops replayed thinking, sets
   parallel tool calls false, omits server tools —
   `internal/backend/openaibe/responses_request.go`), Codex speaks Responses
   natively.
3. **Both harnesses go through the router.** Cost attribution is router
   telemetry keyed by session id (`eval.go:690-732`). A Codex arm pointed
   straight at OpenAI shows $0 and cannot be compared on cost.
4. **Everything not under test is pinned and recorded.** Harness version,
   install command, model id, reasoning effort, context budget, sandbox
   policy, tool restrictions — all in `environment.json` and the resume
   fingerprint.

## The experiment

### Cells

```
                  Claude model          GPT model             open-weight (vLLM)
Claude Code       native passthrough    Messages→Responses    Messages→Chat
Codex             Responses→Messages    native passthrough    Responses→Chat
```

The **open-weight column is the primary signal.** There, both harnesses
sit exactly one translation hop from the model through the same router
code, so the harness main effect is estimable without a native/translated
asymmetry. The diagonals are "harness-native" and the off-diagonals carry
one translation hop; report them as secondary cells that estimate the
harness×model interaction.

### Decision rule

- Codex beats Claude Code on resolve rate *or* cost-per-resolve in the
  open-weight column, CI excluding zero → ship a Codex front-end
  (`gremlord --harness codex`).
- No difference → keep Claude Code only; `clibe` delegation stays as-is.
- Strong harness×model interaction (each harness wins with its own vendor's
  model) → the product answer is **routing harness per model family**, not
  migration.

### Metrics

Primary: official SWE-bench `resolved`, paired per (instance, attempt),
McNemar or paired bootstrap CI, task-clustered.

Secondary, from the same run: cost per resolved instance (router
telemetry), agent wall-clock (separate from provisioning), request count,
tool-call count and error rate, patch size vs gold patch, empty/invalid
patch rate, blinded judge score.

### Power

SWE-bench Verified resolve rates for frontier models sit in the 60–80%
band. Detecting a ~5-point harness effect at 80% power needs on the order
of 300+ paired instance-attempts per cell. Plan for a 10–20 instance
integration pilot first, then a preregistered held-out set of ~100
instances × 3 attempts. The one-instance smoke in the README is a smoke
test, not a ranking.

### Controls, per harness

| Confound | Claude Code | Codex |
|---|---|---|
| Subagents | `--disallowedTools Task` (already) | `-c` under `[agents]` to disable; verify exact key against the pinned release |
| Web tools | add `WebSearch,WebFetch` to `--disallowedTools` (not done today) | feature flag off |
| User config / MCP / hooks / memories | fresh `HOME`, `--strict-mcp-config` with no servers | `--ignore-user-config --ignore-rules --ephemeral`, fresh `CODEX_HOME` |
| Repo instruction files | no `CLAUDE.md` in `/testbed` | no `AGENTS.md` in `/testbed` |
| Sandbox / approvals | `--permission-mode bypassPermissions` (already) | `--dangerously-bypass-approvals-and-sandbox` — the container *is* the sandbox; Codex's landlock inside Docker is a known failure mode and a confound if left on |
| Reasoning effort | Claude Code sends thinking budgets | Codex sends `reasoning.effort` — **pin at the router per alias and ignore what the harness asks**; log what was pinned |
| Model id, provider, max output | `X-Gremlord-Pin-Model` header (already) | same header via `http_headers` in the provider block; router owns the upstream id |
| Declared context budget | virtual scaling (`docs/context-scaling.md`) | `model_context_window` / `model_auto_compact_token_limit` set to the same budget. Compaction *policy* is harness and stays in; the *declared budget* must match |
| Tool schema deferral | `ENABLE_TOOL_SEARCH=false` pinned (`eval.go:948`) | n/a; record in fingerprint |
| Harness version | `ClaudeCodeContainerVersion` (`docker.go:32`) | pin a Codex release tarball (linux x86_64 musl); record both |
| Wall clock, attempts, seed | shared | shared |

A second, later track relaxes the isolation and runs each harness with its
native defaults (instruction files, subagents, web). That answers "which
complete developer system is better", which is a different question and
must be reported separately.

## Implementation

Phases are ordered so each one improves the evaluator we already have,
even if the Codex work stops.

### Phase 0 — fix the evaluator's existing fairness gaps

These affect model-vs-model results today and would silently bias any
harness comparison.

- **Patch capture.** Both paths use `git diff --binary --no-ext-diff`
  (`eval.go:644`, `docker.go:229`), which misses untracked files,
  staged-only changes, and anything the agent committed. Capture the full
  submission relative to the recorded base: `git add -A -N` + `git diff
  <base>` or equivalent, in a way that excludes the harness's own
  scratch files.
- **Per-candidate isolation.** Local eval inherits `os.Environ()` and the
  user's real `HOME` (`eval.go:922`). Give each candidate a throwaway
  `HOME`/`CODEX_HOME`, no MCP, no hooks, no memories, and strip
  instruction files from the workspace.
- **Web tools off.** Add `WebSearch,WebFetch` to `--disallowedTools` for
  Claude candidates so a task isn't solved by finding the upstream fix.
- **Separate provisioning from agent time.** `RunDockerCandidate` folds
  pull + install + turn into one `DurationMS` (`docker.go:163-167`).
  Record `agent_ms` separately.
- **Judge independence.** `runJudge` spawns `claude --print`
  (`eval.go:743`). Replace with a direct Messages call to the router under
  a `judge` session id so the judge isn't one of the arms under test.

Acceptance: existing tests pass; a re-run of the smoke manifest produces
identical verdicts with the new fields populated.

Done 2026-09-08. Local and Docker paths now: intent-to-add + diff against the
recorded pre-agent commit (excluding stripped `CLAUDE.md`/`AGENTS.md`);
throwaway `HOME`/`CLAUDE_CONFIG_DIR`, `--strict-mcp-config` with empty
servers, `--setting-sources ''`, instruction-file strip; `WebSearch,WebFetch`
alongside `Task`; `agent_ms` separate from Docker provisioning; judge is a
direct `/v1/messages` request. Tests and `go vet` pass; Claude CLI smoke
accepted the new flag combination.

### Phase 1 — a `Harness` seam in `internal/eval`

Replace `Options.ClaudeBin` and the two hard-coded argv/env sites with an
interface:

```go
type Harness interface {
    Name() string                                  // "claude" | "codex"
    Version() string                               // pinned
    InstallCmd() []string                          // container install
    Argv(bin, model, prompt string) []string
    Env(e CandidateEnv) []string                   // base URL, token, session, pin, isolation
    FinalText(stdout []byte) string
    Usage(stdout []byte) (Usage, bool)             // harness-reported, if any
    Fingerprint() map[string]string                // goes into environment.json + resume check
}
```

Call sites: `eval.go:624,641-643,952`, `docker.go:85-91,108-133,203-215,501-502`.

`CandidateResult` gains `Harness` and `HarnessVersion`. `sessionID`
(`eval.go:1001`) and `swebenchFingerprint` (`docker.go:451`) must include
the harness, or two harness runs at the same seed merge their spend — the
same bug PR #19 fixed for models.

`ClaudeHarness` is the current behaviour extracted verbatim, plus the
Phase 0 isolation flags. Tests in `eval_test.go` and `docker_test.go`
that match on `"claude"` in argv become table-driven over both harnesses.

Acceptance: `--harness claude` (default) is byte-identical in behaviour to
today; `go test ./internal/eval/...` passes with the seam.

### Phase 2 — `CodexHarness`

Local invocation:

```
codex exec --json --ephemeral --ignore-user-config --ignore-rules \
  --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox \
  -C <workspace> --model <alias> -o <final.md> <prompt>
```

- `Env`: `CODEX_HOME=<tmp>` containing a generated `config.toml` with one
  provider block pointing at the router:

  ```toml
  model_provider = "gremlord"
  [model_providers.gremlord]
  name = "gremlord"
  base_url = "http://127.0.0.1:PORT/v1"     # relay URL in Docker
  env_key = "GREMLORD_TOKEN"
  wire_api = "responses"
  http_headers = { "X-Gremlord-Session" = "...", "X-Gremlord-Pin-Model" = "...", "X-Gremlord-Profile" = "..." }
  ```

  plus `model_context_window` / `model_auto_compact_token_limit` set from
  the alias's configured budget, and `[agents]` / web features disabled.
- `FinalText`: read `-o` file; fall back to the last `item.*` agent
  message in the JSONL.
- `Usage`: sum `turn.completed.usage` events. This is the harness's view;
  the router's telemetry remains authoritative for cost. Record both and
  flag disagreement.
- `InstallCmd`: pinned release tarball URL for `rust-vX.Y.Z` linux
  x86_64 musl, unpacked to `/usr/local/bin/codex`. Verify with
  `codex --version`.
- Terminal-event validation: treat a run that ends without
  `turn.completed` or `turn.failed` as `model_error`, not `complete`.
- Process-tree cleanup on timeout (Codex spawns shell children).

Acceptance: a local-manifest smoke task runs end-to-end under
`--harness codex` against the current router with an **OpenAI Responses
alias** (see Phase 3 — until then the router will not accept the request;
this phase's tests use a fake executor).

### Phase 3 — Responses inbound on the router

Codex only speaks `wire_api = "responses"`. gremlord listens on
`/v1/messages` and forwards unknown `/v1/*` to Anthropic
(`internal/router/server.go:83-87`). Without this phase no Codex arm can be
metered or routed.

- `POST /v1/responses` handler beside `handleMessages`. Auth, session /
  pin / profile headers, budget gate, usage log, pricing, and the
  pin-remap backstop (`server.go:141-154`) are shared.
- Dispatch:
  - resolved provider is OpenAI/Responses → passthrough, byte-faithful
    where possible (mirror of the anthropic passthrough).
  - resolved provider is Anthropic → new `responses→messages` request
    translator and `messages→responses` response/stream translator.
  - resolved provider is Chat Completions (vLLM, xAI, Ollama) →
    `responses→chat` and back.
- Hard parts, mirrored from the existing translator: reasoning items
  round-trip (Codex expects encrypted reasoning content back; Anthropic
  gives signed thinking blocks), `previous_response_id` / `store`
  semantics (reject `store=true`, require full input each turn), no
  `cache_control` equivalent so Claude prompt caching must be synthesised
  at the router for Codex→Claude, tool-call id formats.
- Scope guard: this is enough for **eval traffic** — one process, full
  input each turn, no `previous_response_id`, no server tools. Interactive
  Codex sessions may need more; do not block the eval on that.

Size estimate: the existing outbound translator is ~1,600 lines including
tests (`internal/backend/openaibe/`). Expect the same again.

Acceptance: contract tests for both translators (tool call, tool result,
multi-turn, streaming, reasoning round-trip); a Codex smoke run against a
Claude alias and an Anthropic-key-free run against a vLLM alias both
produce priced usage rows keyed to the eval session id.

### Phase 4 — cell orchestration and reporting

- Manifest or flags express cells: `--baseline-harness`, `--mut-harness`
  alongside the existing `--baseline` / `--mut` model flags; or a `cells:`
  block that expands to pairs. Keep the pair abstraction — a cell
  comparison is still baseline-vs-mut, it's just that harness may differ.
- `Summary` gains per-cell aggregates and an interaction table
  (harness × model, resolve rate with CI).
- `gremlord eval report` prints the table; `--json` emits it.
- Resume checks compare the full fingerprint (harness, version, install,
  model resolution, pinned effort, budget), not just model + seed
  (`eval.go:483-486`).

### Phase 5 — run it

1. Pilot: 10–20 SWE-bench Verified instances × 2 attempts, open-weight
   column only, to shake out infra. Expect infra failures; that's what
   `--resume` is for.
2. Preregister: fix the instance list, attempts, seed, pinned versions,
   pinned effort, and the decision rule above before the main run.
3. Main run: ~100 instances × 3 attempts × 6 cells. Budget time for x86_64
   emulation on Apple Silicon or run on a Linux box.
4. Report with the decision rule applied. If the answer is "route per
   model family", that's a config change in the router, not a rewrite.

## Out of scope for this plan

- `gremlord --harness codex` for interactive sessions (statusline, tier
  env vars, `gremlord peers`, `agents sync`, Auto Goal). Gated on the
  eval result.
- Forking Codex. Stock CLI for measurement; app-server for deeper
  integration if ever needed; fork only for a demonstrated requirement.
- Cross-harness parity of `CLAUDE.md` vs `AGENTS.md` semantics. Both are
  excluded in the controlled track and left native in the realistic track.

## Open questions

- Which open-weight model(s) for the primary column? glm53 and kimi-k3 are
  configured; pick the one with the more stable vLLM endpoint and a
  Responses *or* Chat surface we can translate to without new work.
- Codex `[agents]` config key for disabling subagents in the pinned
  release — confirm against that release's `config.md`, not the docs site.
- Does the pinned Codex build honour `http_headers` for non-OpenAI
  providers on every request, including retries? If not, session
  attribution needs `env_http_headers` or a proxy-side fallback.
- Two claims surfaced during research that would affect the strategic
  weight but were not verified: a mid-2026 Claude Code proxy-fingerprinting
  incident, and an Anthropic policy change on subscription use via
  third-party harnesses. Check the primary sources before either
  influences a decision.
