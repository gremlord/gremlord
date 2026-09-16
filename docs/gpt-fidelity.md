# GPT fidelity and context handling

This branch closes specific translation losses between Claude Code and OpenAI
Responses. It does **not** establish task-quality parity with native Codex.
Correct protocol replay is necessary evidence. The local
[matched harness pilot](../scripts/gpt-bench/README.md) measures quality, time,
and cost on three synthetic tasks and a three-user-turn development workflow.
Both harnesses passed all checks, but Gremlord took 2.29× the agent time on the
longer workflow. See [measured results](gpt-benchmark-results.md); larger held-out
evaluations remain needed.

## Implemented

- Parallel function calls default on. Each item has its own argument buffer,
  including providers that supply the item ID late. The caller's
  `disable_parallel_tool_use` remains authoritative.
- Requests use `store: false` and include `reasoning.encrypted_content`.
  Original reasoning, assistant messages (including `phase`), and function calls
  are restored on subsequent matching turns. Ciphertext is never substituted
  for an Anthropic signature or exposed as a thinking summary.
- Replay works for JSON and SSE, using completed output items and terminal
  response output. It preserves ordering in the native output, handles parallel
  tools completing in a different order, and restores upstream call IDs when
  the Anthropic transcript contains sanitized IDs.
- Premature Responses EOF produces a retryable error instead of a successful
  `end_turn`. Failed, incomplete, or inconsistent outputs are not cached.
- The context gauge and `/count_tokens` use the latest matched upstream
  input-plus-output usage plus estimated visible growth, with the existing
  calibrated estimate as a floor. This accounts for hidden reasoning without
  treating base64 ciphertext as ordinary text. Billing continues to use actual
  upstream usage.
- Interrupted calls receive an explicit missing-result marker. Orphaned tool
  results become labeled user context, preserving their text while avoiding an
  invalid Responses call/output pair. Nested result images remain hoisted.
- Function tools explicitly send `strict: false`, retaining optional arguments
  in Claude Code's schemas rather than relying on Responses schema normalization.
- `output_config.effort` is honored for effort-based translated models.
  `low`, `medium`, `high`, and `xhigh` pass through; Anthropic `max` maps to
  `xhigh`. A model's explicit `reasoning_effort` overrides the client and the
  legacy thinking-budget mapping. Model-specific `max`/`ultra` are available
  through that override; use only levels the selected upstream supports.

### Replay boundaries

The cache belongs to the OpenAI backend instance. Entries are scoped to launch
session, provider, endpoint, credential, model, system instructions, and an
exact normalized visible-history prefix. Matching tolerates split assistant
messages and JSON argument whitespace/key order, while preserving large
integer arguments exactly. Thinking summaries are not used as hidden state.

The cache has a 64 MiB payload/accounting limit, 512-response limit, and 24-hour
idle expiry, with LRU eviction. It is memory-only. Router restart, eviction,
missing session headers, edited history, or client compaction can cause a miss;
the request then proceeds with ordinary translated context. Compaction does
not resurrect reasoning from the history it removed. A reasoning-only response
has no reliable visible anchor and is not cached. Legacy launch-session headers
are supported.

This is a deliberately conservative first implementation. It does not persist
state across restart, provide OpenAI-native compaction, or change the router's
pre-dispatch tier-selection estimates. Those estimates still cannot see the
backend's hidden context; pin a model when evaluating fidelity.

## Findings from Codex source

Reviewed `openai/codex` on 2026-09-15, at main revision
`f2b5b81f39fba7d1172e4a5e65a427f029a39479`. These are source observations,
not measurements of their individual effect on task success.

| Codex behavior | Gremlord implication |
| --- | --- |
| [Client](https://github.com/openai/codex/blob/f2b5b81f39fba7d1172e4a5e65a427f029a39479/codex-rs/core/src/client.rs) sends encrypted reasoning inclusion, stateless requests, parallel tools, model-aware reasoning/verbosity, and a session cache key. | Replay and parallel calls are implemented here; explicit effort avoids relying on a Claude budget. Do not assume every client setting is a quality improvement. |
| [History manager](https://github.com/openai/codex/blob/f2b5b81f39fba7d1172e4a5e65a427f029a39479/codex-rs/core/src/context_manager/history.rs) uses measured last-response usage plus estimates of newly added items, and distinguishes hidden reasoning from visible text. | Implemented a measured context floor for the Responses gauge/count endpoint. The heuristic remains an estimate, especially if the service stops rendering old reasoning. |
| [History normalization](https://github.com/openai/codex/blob/f2b5b81f39fba7d1172e4a5e65a427f029a39479/codex-rs/core/src/context_manager/normalize.rs) pairs missing calls with aborted results and removes orphan outputs. | Implemented pairing repair. Gremlord preserves orphan text as user context because client-side compaction may have removed its call. |
| [Model catalog](https://github.com/openai/codex/blob/f2b5b81f39fba7d1172e4a5e65a427f029a39479/codex-rs/models-manager/models.json) supplies per-model defaults and tool-output truncation policies; reviewed GPT entries use a 10,000-token tool-output policy. | Bounded tool-output retention is a valuable next experiment. Preserve full logs outside the model window, mark truncation, retain head/tail and call pairs, and measure task failures before enabling it globally. |
| [Remote compaction](https://github.com/openai/codex/blob/f2b5b81f39fba7d1172e4a5e65a427f029a39479/codex-rs/core/src/compact_remote_v2.rs) installs a native compaction item, rebuilds selected initial context, retains selected messages, and recomputes usage. | Native compaction requires a persistent translator-owned history/checkpoint, synchronized with Claude Code compaction and retry/fork boundaries. Adding a `/responses/compact` request alone would not safely preserve the client's state. |
| [Compaction history trimming](https://github.com/openai/codex/blob/f2b5b81f39fba7d1172e4a5e65a427f029a39479/codex-rs/core/src/compact_remote_history.rs) can rewrite oversized tool outputs to fit while keeping item groups consistent. | Add context-pressure tests covering large logs, screenshots, interrupted tools, and model switches before changing trimming policy. Never drop arbitrary history items independently of their tool partners. |
| [Tool construction](https://github.com/openai/codex/blob/f2b5b81f39fba7d1172e4a5e65a427f029a39479/codex-rs/core/src/tools/spec_plan.rs) is capability-aware and includes native namespaces, tool search, and model-specific tool choices. | Claude Code owns tool execution and its schemas. Copying Codex's tool list or instructions into the proxy would promise capabilities the client cannot execute. A GPT prompt/profile experiment must retain the actual tools and permissions. |

The catalog's context settings are harness defaults, not authoritative API
limits. Keep `context_window` and `effective_context` explicit for the model and
endpoint being tested. Leave room for generation/reasoning; Gremlord's existing
gauge scaling and Claude Code's compaction threshold remain the policy owners.
This change does not silently install a new universal window limit.

The original handoff also needs a factual update: current
[OpenAI reasoning documentation](https://developers.openai.com/api/docs/guides/reasoning)
says stateless responses include encrypted reasoning by default and still accept
the legacy `include` value. The missing replay was the essential loss. That
documentation also distinguishes reasoning from the current turn versus all
turns; replay does not imply the API renders every old reasoning token forever.
[Phase guidance](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.5)
requires preserving assistant phases during manual replay.
[Function-calling guidance](https://developers.openai.com/api/docs/guides/function-calling)
documents Responses' strict-schema normalization and the explicit opt-out.

There is another model-dependent detail: the reviewed Codex catalog enables
Responses Lite for GPT-5.6 Sol and GPT-6 Astra. On that path the client serializes
tool declarations into input, sends a dedicated protocol header, sets
`parallel_tool_calls: false`, and explicitly requests `reasoning.context:
all_turns`. Thus “Codex always sends parallel=true” is not accurate for these
models. Gremlord uses standard Responses function tools and supports parallel
calls there. The protocols differ even when the model is the same.

The current reasoning documentation says GPT-5.6 defaults to `all_turns` when
`reasoning.context` is omitted; earlier families may default to `current_turn`.
Encrypted replay supplies available state, while the effective context mode
determines which prior reasoning the API renders. The pilot records both the
requested and returned context modes rather than assuming they are equal.

### Changes not justified by the evidence yet

Gremlord removed `prompt_cache_key` in commit `cd68a1a` after local measurements
showed 84–95% provider cache hits, including approximately 89% for GPT without
the key. Codex uses a key, but that alone does not overturn those measurements.
Cache affinity primarily concerns latency/cost; adding it should be an A/B test.

Likewise, increasing reasoning effort, copying a longer system prompt, or
maximizing the context window is not automatically a task-quality improvement.
Choose a supported effort explicitly for matched evaluations and report cost
and latency alongside success.

## Configuration and validation

Use `api: responses` on an OpenAI provider or model to enable this path.
For an existing model entry, an example configuration is:

```yaml
api: responses
reasoning: effort
reasoning_effort: high # optional; pin a level supported by this model
```

The CLI equivalent for the new setting is `--reasoning-effort high` alongside
`--reasoning effort` in `gremlord models add`. Omit the override to honor client
effort, falling back to the legacy thinking budget when absent.

Regression coverage includes interleaved calls, late IDs, authoritative final
arguments, three-turn reasoning/phase replay, JSON/SSE/terminal-only providers,
session and route isolation, compaction reset, byte/entry/TTL eviction,
concurrent branches, large integer arguments, context estimates, interrupted
histories, refusals, and premature EOF.

```sh
go test -race ./internal/backend/openaibe ./internal/config
go test ./...
go vet ./...
go build ./...
```

An opt-in live test reads an existing alias from `~/.gremlord/config.yaml`,
overrides effort to `high` for that test only, and makes two requests capped at
4,096 output tokens each. It executes no external tools and changes no config:

```sh
GREMLORD_LIVE_RESPONSES_MODEL=gpt-6-astra \
  go test ./internal/backend/openaibe -run '^TestResponsesLiveContinuity$' -v -count=1
```

On 2026-09-15 this passed against the configured `api.openai.com` route:
first response 147 input / 141 output tokens, including encrypted reasoning and
two calls; continuation 310 input / 5 output tokens, accepting replayed
ciphertext and returning the correct sum. Earlier arithmetic-only probes
produced no reasoning items, so they did not test encrypted continuity.

For GPT sessions, check existing model entries as well as installing the binary:
an entry on Chat Completions with `reasoning: none` does not enable Responses
continuity. An existing router remains in its host Claude session until that
session restarts; replacing the executable does not hot-swap a running process.
The benchmark starts its own server using the updated backend.

The next broader quality evaluation should use the existing
[harness evaluation design](harness-eval-plan.md): identical task revisions,
model IDs, effort, context budgets, and permissions; repeated attempts; blinded
grading; cost/latency and compaction counts. Include long-horizon tasks that
force multiple tool loops and at least one compaction. Compare current Gremlord,
this implementation, and native Codex. Protocol tests do not substitute for
that experiment, and the synthetic pilot does not establish broad quality parity.
