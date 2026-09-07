# Virtual context scaling

Different models have different context windows — and different *usable*
windows, since attention quality often degrades well before the advertised
limit. Claude Code manages its own memory (auto-compact, tool-result
trimming) but sizes everything against the model id it was launched with: a
`claude-*` name means a ~200K window. It cannot be told the real window of
whatever the router actually dispatched to.

So the router lies to it — proportionally.

## How it works

Every model can declare its real context size in `~/.gremlord/config.yaml`:

```yaml
models:
  qwen:  {provider: local,  id: qwen3-coder-30b, context_window: 32768}
  gpt:   {provider: openai, id: gpt-5.2,         context_window: 400000}
  glm:   {provider: z,      id: glm-4.7,         context_window: 200000, effective_context: 60000}
```

The model's **budget** is `min(context_window, effective_context)`. Every
token count the router reports to the client — the `count_tokens` estimate
and the input-side `usage` fields on responses — is multiplied by

```
factor = 200_000 / budget
```

A 32K model that is half full reports ~100K tokens; Claude Code's gauge
reads 50% and its auto-compact fires at the same *relative* fullness it
would on a real Claude model — which is exactly when the 32K window is
nearly exhausted. The same math runs in reverse for big windows: a 400K
model reports half its true count, so Claude Code doesn't throw away
context at 200K it didn't need to.

`effective_context` is the attention knob. A model with a nominal 200K
window that gets unreliable past 60K should be configured with
`effective_context: 60000` — the client then compacts at 60K real tokens
and the model always operates in its coherent range.

Rules of the lie:

- **Only the client is lied to.** Budgets, pricing, the SQLite usage log,
  and the router log all record true upstream usage. The scaled number is
  stored *alongside* it (`reported_input`, `ctx_budget` columns) so the
  gap is auditable.
- **Only input-side counts scale** (`input_tokens`, cache read/write).
  Output tokens stay true — they roll into the next request's input count
  anyway.
- **Rounding is always up**, preserving the deliberate bias-high property
  of the token estimator: compacting early is harmless, blowing the real
  window is fatal.
- **Unset means untouched.** Models without `context_window` /
  `effective_context` get factor 1, and an unset budget is treated as
  *equal to* the assumed window wherever budgets are compared.
- **The Anthropic passthrough is byte-faithful until a budget asks
  otherwise.** At factor 1 — the common case — it forwards every byte
  unchanged, cache_control and signed thinking blocks included. When a
  factor applies (an `effective_context` on a Claude model, or a routing
  rule whose shared gauge is anchored somewhere other than 200K), it
  rewrites the three input-side usage counters in `message_start` and in
  non-streaming responses, through a generic decode so neighbouring
  fields round-trip as written. A payload it cannot parse is forwarded
  unchanged: stale numbers beat an unreadable response.

## Interaction with dynamic routing

With `model: auto`, different turns land on models with different budgets.

**One anchor per rule.** Scaling against whichever model served the last
turn makes the gauge mean something different every turn: the same
conversation is 30% of a 1M model and 95% of a 200K one. Claude Code
compacts on the last reported number, so a single light-tier turn
triggers a compaction the big model never needed — the effective window
of the whole session collapses to the smallest tier it happens to touch.

So the gauge is anchored to one budget for the rule, chosen by
`context_gauge`:

```yaml
routing:
  auto:
    classifier: haiku
    context_gauge: max      # default; also: min, model
    tiers: {deep: opus, standard: sonnet, light: qwen}
```

- `max` (default) — the largest budget the rule can route to, tier and
  task targets alike. The session grows until the biggest tier is full;
  turns that outgrow a smaller tier are filtered out of it and remapped
  up by the size-aware routing below, which is machinery that already
  exists. **The trade-off:** past a smaller tier's window, that tier
  stops being eligible, so a long session concentrates on the big model.
  That is the intended exchange — usable window for tier mix — but it is
  a real cost change on a rule that mixes a 200K tier with a 1M one.
- `min` — the smallest budget. Every tier stays reachable at any
  conversation length, at the cost of compacting as early as the
  smallest window demands.
- `model` — the original per-request behavior, gauge jumps included.

An undeclared window counts as the assumed 200K in this comparison
rather than being skipped, so an all-Anthropic rule anchors at 200K and
scales by exactly 1, exactly as before.

The usage log records both numbers: `ctx_budget` is the serving model's
own window, `gauge_budget` what the client was scaled against.

**Size-aware tier selection.** Before classifying, the router estimates the
request's input size and filters out any tier whose budget can't hold input
plus reserved output headroom. The classifier then runs over the survivors;
if its pick was filtered out, it's remapped upward to the smallest tier that
still fits. A mid-turn continuation that outgrew its tier is remapped the
same way and pinned, so the rest of the turn stays on the larger tier. When
only one tier fits, the classifier call is skipped entirely. Tiers without a
declared `context_window` are treated as infinite (always eligible) — so a
mix of sized and unsized models works, and all-anthropic routing is
unaffected.

**Dispatch guard.** Whatever the router settles on, a pre-dispatch check
catches the hard-overflow case: if the resolved model has a known budget and
the estimated input exceeds it (plus headroom), the request is refused with a
`400 invalid_request_error` ("request too large for model context budget")
before it reaches upstream — instead of a mangled, provider-specific failure.
Claude Code treats the 400 as a terminal error (no retry-spin), which is why
it's used over 413/429. The guard is skipped for `count_tokens` and for
models with an unknown budget.

**Task overrides.** A task-aware rule classifies task and tier in the same
request. A configured task model takes precedence over the tier target, but it
must pass the same context-budget and request-byte checks. If it cannot hold the
request, the router uses the smallest eligible capability tier instead of trying
another task model. The chosen task and any tier remap remain sticky for tool
results in that turn. Pinned sessions bypass both task and tier routing.

Both behaviors are observable: the router logs `autoroute_size` (Debug) with
the estimate, required tokens, excluded tiers, and the remap; `route_decisions`
gains a `reason` column (`size:light→standard`, `size:sticky:light→standard`,
`task:implementation`, or `task:sql_data:size-ineligible`) visible via the
statusline and `gremlord context`.

**Request-body byte cap.** Separate from the token budget: some upstreams cap
the raw request body size (e.g. an nginx `client_max_body_size`), and a body can
blow that cap while still being token-small — most often with accumulated
base64 images/attachments. Declare it on the provider:

```yaml
providers:
  glm52: {type: openai, base_url: …, api_key_env: VLLM_API_KEY, max_request_bytes: 33554432} # 32 MiB
```

The router treats it like the token budget: a tier whose provider cap the body
exceeds is filtered out before routing (so an image-heavy turn routes away from
the capped provider), and a pre-dispatch guard refuses a body over the resolved
provider's cap with a `400 invalid_request_error` ("request body too large … run
/compact or remove images/attachments") instead of a mangled upstream 413 retry
loop. Unknown cap (`0`) means no guard. `gremlord providers add --max-request-bytes`
sets it.

## Calibrating the estimator

`count_tokens` for translated models is a 3.5-chars-per-token heuristic
plus a 10% margin, and measured against real tokenizers it runs 15-25%
high. The bias is deliberate — compacting early is harmless, overflowing
is fatal — but it is not free: the over-count is subtracted from every
budget the router checks, so on a 1M-token model it can strand six
figures of usable window and force needless tier remaps.

Every request now records its own raw estimate alongside what upstream
billed (`est_input`, `est_system`, `est_tools` in `usage_events`). From
that history the router derives a correction per upstream model — the
ratio of summed true input to summed estimate over successful requests in
the last 14 days — and multiplies the raw estimate by it before any
budget comparison.

Guards, because this feeds itself:

- **Only raw estimates are stored.** Recording the corrected number would
  feed the correction back into its own measurement.
- **20 requests minimum** before a model's ratio is trusted; below that
  the raw estimate stands.
- **Clamped to [0.6, 1.5]**, and the clamp is asymmetric on purpose:
  correcting *upward* restores the safety bias, correcting downward
  spends it, so the floor is tighter than the ceiling.
- **Ratio of sums, not mean of ratios** — a handful of tiny requests
  should not outvote the large ones, and it is the large requests whose
  fit against a budget actually matters.

`gremlord context` prints the measured accuracy per model. A ratio of 0.80
means the estimator over-counts by 25% and a fifth of every budget check
was going to a number that was never there.

## Where the context actually goes

The same per-request record splits the estimate three ways — system
prompt, tool schemas, conversation — because the first two are re-sent in
full on every request whatever the turn is about. `gremlord context` shows
the average split and the fixed share; a session where tool schemas are
40% of every request is one where trimming MCP servers buys more than any
amount of routing cleverness.

## Deferring the tool schemas

Tool schemas turned out to be the whole ballgame. Claude Code can defer
them — the deferred tools' names go up in a system reminder, the model
pulls a schema in with `ToolSearch` when it wants one, and only then does
the full definition ride on the wire — but the client switches that off
when it does not recognize the endpoint:

```
[ToolSearch:optimistic] disabled: ANTHROPIC_BASE_URL=http://127.0.0.1:41100
is not a first-party Anthropic host. Set ENABLE_TOOL_SEARCH=true (or auto /
auto:N) if your proxy forwards tool_reference blocks.
```

The router is never a first-party host, so every session fell back to
eager schemas. On a session with 165 MCP tools that was 47K of builtins
plus 139K of MCP definitions — 186K of a 200K frame, spent before the
first user turn, and re-sent on every request after it. The same machine
running Claude Code directly paid 11K for the same tools.

Sessions now set `ENABLE_TOOL_SEARCH=true`, which costs the router almost
nothing because the deferral is the *client's* work, not the API's.
Measured on the wire: the first request carries 12 tools instead of 28,
the withheld 18 arrive as names, and when the model calls `ToolSearch` the
client answers its own call with `tool_reference` blocks and then puts the
bound tool's real schema in the next request's `tools` array. So the
router has to do exactly one thing: not lose those blocks. Passthrough
forwards them untouched along with the `advanced-tool-use-2025-11-20` beta
value; translation, which has no server-side expansion to hand them to,
renders them as text so the tool result is never empty — an empty
`role:"tool"` message is something OpenAI-dialect servers reject outright,
and the schema was already coming on the next request anyway.

What deferral does ask for is a model that plays along: it has to notice
the withheld-tool announcement and call `ToolSearch` before reaching for
one of those tools. Claude models are trained on that protocol. A model
that is not simply never loads them and quietly runs without them — no
error, just a smaller toolbox — so a profile pinned to such a model can
set `tool_search: false` and get eager schemas back:

```yaml
profiles:
    kimi:
        model: kimi-k3
        tool_search: false
```

Precedence runs most-immediate-first: `ENABLE_TOOL_SEARCH` in the
launching shell (`false` for eager, `auto:N` to sample), then the
profile's `tool_search`, then on.

`gremlord eval` pins the variable to `false` rather than inheriting it.
Interactive sessions export `ENABLE_TOOL_SEARCH=true` and every command
they run inherits it, so an eval launched from inside a session would
defer while the same command from a plain terminal would not — and
whether a run is comparable to the one stored beside it must not depend
on where it was invoked from. Flipping that is a deliberate re-baseline.

## Watching the prefix cache

Prompt caching decides most of the input bill, so `gremlord cost` reports
the share of each model's input tokens that upstream served from cache.
Translated backends get no `cache_control` breakpoints — that is an
Anthropic-API concept — but provider-side implicit caching is prefix-based
and shows up here as cache reads. Check this number before reaching for
anything cleverer: on a healthy setup it sits in the high eighties or
nineties, and there is simply no headroom to reclaim.

## Evaluating it

Three layers, from cheap to real:

1. **Invariant tests** (`internal/tokens/scale_test.go`): proportionality
   and round-up bias across budgets from 8K to 1M.
2. **Simulation eval** (`internal/router/ctxscale_eval_test.go`): plays a
   growing Claude-Code-shaped conversation against the real router with a
   fake upstream, for each budget tier, and asserts (a) the reported
   fraction of the assumed window always equals the true fraction of the
   real budget, and (b) the simulated auto-compact trigger (92% of gauge)
   lands past 92% but within one turn-increment of the real budget —
   never after it. Run with:

   ```bash
   go test ./internal/router/ -run TestContextScalingEval -v
   ```

3. **Live sessions** (`gremlord context [session-id]`): per-request
   trajectory of true tokens vs reported tokens vs budget, with compaction
   points marked. This is the research surface.

## Researching `effective_context` for a model

The nominal window is in the model card; the *effective* window is an
empirical property you measure. Suggested loop:

1. Start with `context_window` only (nominal). Run real sessions.
2. Watch `gremlord context` and the router log's `ctx_pct` field. Correlate
   quality failures — wrong edits, forgotten instructions, tool-call
   flailing, upstream `4xx` at high fullness — with the fullness percentage
   at which they happened. The `err_type` column in the trajectory makes
   hard failures visible; soft degradation you judge from the session.
3. Set `effective_context` a comfortable margin below the fullness where
   degradation starts, and re-run. The client now compacts before the
   model enters its mushy zone.
4. For a sharper measurement, run a needle-in-a-haystack probe: seed a fact
   early in a session, pad the context to N% fullness with real work, then
   ask for the fact. The fullness where recall breaks is the effective
   window. (Published long-context benchmarks — RULER, NIAH variants — give
   a starting point per model family, but local quantized builds often
   underperform their upstream numbers, so verify locally.)

Queries against `~/.gremlord/agentic.db` for aggregate research:

```sql
-- error rate by context fullness decile, per model
SELECT model,
       CAST(10.0 * (input_tokens+cache_read_tokens+cache_write_tokens) / ctx_budget AS INT) AS decile,
       COUNT(*) AS requests,
       SUM(CASE WHEN err_type != '' THEN 1 ELSE 0 END) AS errors
FROM usage_events
WHERE ctx_budget > 0
GROUP BY model, decile ORDER BY model, decile;
```

## Known limitations

- **Auto-compact only engages for model ids Claude Code recognizes.**
  Measured live (Claude Code 2.x, July 2026): with an unknown alias name
  in `ANTHROPIC_MODEL` (`glm`, `qwen`), the client applies *no* context
  limit at all — it neither compacts nor refuses, at any reported
  fullness. With a recognized ~200K id it compacts automatically when the
  last reported input-side total crosses the window, and refuses
  ("Prompt is too long") rather than sending past it. To get client-side
  compaction on a scaled model, alias it *as* a 200K Claude id — an
  explicit model entry overrides the built-in `claude-*` passthrough:

  ```yaml
  models:
    claude-sonnet-5: {provider: local, id: qwen3-coder-30b, context_window: 32768}
  ```

  Avoid ids Claude Code treats as 1M (`[1m]` variants). Unknown aliases
  still get correct *reporting* (gauge, statusline, `gremlord context`) —
  they just won't trigger the client's compaction machinery.
- **Leave real headroom under the trigger.** The observed trigger is the
  full assumed window (~200K reported), not the ~92% warning line — so at
  trigger time the model is at ~100% of its *budget*. Two things keep that
  safe: the estimator's deliberate over-count (observed ~+15–25% vs real
  tokenizers) means the trigger fires while true usage is still below
  budget, and `effective_context` set below the nominal window keeps the
  budget itself short of the hard limit. Live run: budget 55K on a 128K
  window compacted at 48K true (88% of budget, 37% of the real window).
- `count_tokens` for translated models is an estimate (~3.5 chars/token,
  +10% margin), not a tokenizer. Scaling preserves the bias but also
  scales the estimation error; on very small budgets (≤8K) the margin can
  cost a few hundred usable tokens. Calibration (above) narrows this once
  a model has history, but never replaces a real tokenizer.
- The `max` anchor concentrates long sessions on the largest tier, since
  smaller tiers stop being eligible once the conversation outgrows them.
  `context_gauge: min` trades window for tier mix.
- Claude Code's compact threshold (~92%) is its own moving target; the
  scaling is threshold-agnostic (pure proportionality), so threshold
  changes upstream don't break correctness, only the eval's simulated
  trigger constant.
