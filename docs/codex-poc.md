# Native Codex PoC

`gremlord --harness codex` launches the installed, unmodified Codex CLI with
a fixed GPT model. Its native Responses requests pass through Gremlord's
router, usage database and budget gate. Claude Code remains the default.

```text
Codex CLI → Gremlord /v1/responses → configured GPT Responses provider
                        ↓
                 usage + pricing + budgets
```

This differs from the existing `cli` provider, which delegates a whole task
from inside Claude Code. Here Codex owns the conversation, tools, saved
history, reasoning continuity and compaction.

## Try it

Build this branch with Codex installed on `PATH`:

```sh
go build -o /tmp/gremlord-codex-poc .
/tmp/gremlord-codex-poc --harness codex --model gpt-6-astra
```

The model argument is an existing Gremlord alias whose provider type is
`openai`, effective API is `responses`, and upstream model ID is `gpt-*`.
If your default profile is subscription passthrough, select a routed profile
with `--profile`.

Arguments after `--` go to Codex:

```sh
/tmp/gremlord-codex-poc --harness codex --model gpt-6-astra -- exec "Inspect this repo and explain the test setup."
/tmp/gremlord-codex-poc --harness codex --model gpt-6-astra -- resume
/tmp/gremlord-codex-poc --harness codex --model gpt-6-astra -- exec resume THREAD_ID "Continue with the next requirement."
```

The launcher keeps your Codex home, rules and permission settings. It does
not enable approval bypass or grant Claude Code's tool allowlist. Existing
Claude Code conversations cannot be resumed in Codex. Choose Gremlord's
model/profile before `--`; native model/provider/profile overrides are
rejected to avoid accidentally bypassing the gateway.

An older router on the configured port produces an explicit restart error.
This build does not silently use the old router or terminate another session.

## Fidelity and context

- Codex receives the real GPT ID for native model instructions and tools.
  A separate header pins the configured Gremlord alias.
- Native fields and SSE events survive, including Responses Lite, custom
  tools, encrypted reasoning, assistant phases and turn-state headers.
  No Anthropic conversion or execution-profile supplement is applied.
- Generation `reasoning_effort` and `max_output` remain router-enforced alias settings.
  Without an effort pin, Codex chooses effort normally.
- The configured context budget becomes Codex's context window. Automatic
  compaction defaults to 90% of it; returned token counts stay unscaled.
- Both `/v1/responses` and `/v1/responses/compact` are routed and metered.
  Compaction contents stay opaque. Model-specific compaction may also use
  ordinary Responses calls.
- Astra's native `compaction_trigger` requires an output allowance of at
  least 20,000 when supplied. Set the alias's `max_output` to 32,768 or more
  when using that compactor; Gremlord does not silently raise a lower cap.
- The PoC uses HTTP streaming; WebSockets and compressed requests are
  disabled. Codex owns retries, with one request/stream retry.

The launcher uses documented
[custom provider support](https://developers.openai.com/codex/config-advanced#custom-model-providers).
It preserves the OpenAI provider identity for native protocol capabilities
and restricts the launch path to GPT aliases.

## Metering and scope

Usage is recorded before the terminal SSE event is released, so the next
tool request sees updated spend. Cached reads and reported cache writes are
separated from ordinary input without double counting. The database and
`gremlord cost` use existing prices and global/profile budgets.

Costs remain token-price estimates: configure accurate rates, including
cache-write prices when those tokens are reported. Server-tool fees, external
MCP services and subscription billing are outside this meter. Budgets block
the next request; they do not reserve concurrent spend or interrupt a running
stream. Disconnects without terminal usage are recorded as errors with
unknown usage. Each resumed launch gets a new Gremlord cost session while
retaining its Codex thread history.

Automatic tier routing, switching model families, reverse translation to
Anthropic/Chat, stored/background responses and standalone server-tool
endpoints are outside this PoC. Subagent/provider overrides have not been
qualified for complete budget coverage. Claude Code hooks, plugins, peers
and agent files are not migrated.

The Astra benchmark uses these USD-per-million rates, including an explicit
write price. A configuration copied from an older pilot may omit
`cache_write`; Gremlord treats an omitted rate as zero, which understates
spend when the provider reports cache writes. Add the applicable write rate
to your existing pricing entry before relying on its budget accounting.
These are the experiment's recorded rates; verify your provider's prices.

```yaml
pricing:
  gpt-6-astra:
    input: 10
    output: 50
    cache_read: 1
    cache_write: 12.5
```

## Verification

The [three-turn live comparison](codex-poc-results.md) passed all checkpoints
in both harnesses and reconciled the PoC's usage against an independent meter.

Contract tests cover native headers and opaque history, byte-preserved SSE
including multiline events, cache accounting, model/effort/output pins,
compaction, global/profile budget stops, recording before terminal delivery,
duplicate terminal events, truncated streams, upstream errors, cancellation,
credential isolation, redirects and old-router detection.

An optional installed-CLI test verifies `exec` and `exec resume` against a
fake local API without charges:

```sh
GREMLORD_CODEX_BIN=/path/to/codex go test ./internal/launch -run TestCodexCLIGatewaySmoke -v
```

An explicitly opt-in live smoke uses the configured GPT provider, triggers
manual compaction via Codex app-server, then checks that two requirements
survive and that all calls were metered. It makes billable API requests:

```sh
GREMLORD_CODEX_LIVE=1 go test ./internal/launch -run TestCodexLiveCompaction -v -count=1
```

The [benchmark driver](../scripts/gpt-bench/README.md) accepts
`-baseline codex -mut codex-gremlord -gremlord-bin /tmp/gremlord-codex-poc`.
The new arm executes the actual Gremlord binary and router. Both arms use
isolated trusted fixtures, clean homes and identical permission bypass for
the benchmark only. Production launch does not set that bypass.

Per-turn `gremlord-usage.json` records reconcile against the independent
shared meter in `requests.jsonl`. The two ledgers must not be added together.
