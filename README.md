# gremlord

```
                    \    /\  /\  /\    /
                     \  /  \/  \/  \  /
                  .-' \/ ()  ()  () \/ '-.
                .-'   [================]   '-.
       ,.      /    .-'~~~~~~~~~~~~~~~'-.    \      .,
      /  \    /   .'   .-----.   .-----.   '.  \   /  \
     /  /\\  |   /    / .---. \ / .---. \    \  |  //\  \
    /  /  \\ |  |    | ( (o) )-=-( (o) ) |    |  | //  \  \
   /  /    \\|  '-'   \ '---' / \ '---' /   '-'  |//    \  \
  /  /      \\_________'-----'   '-----'_________//      \  \
 |  /        \\  ~zzt~    \   ___   /    ~zzt~  //        \  |
 | /          \\      /    \ /   \ /    \      //          \ |
 |/            \\    |   .--'\/\/\/'--.   |    //            \|
 '              \\    \   \ /\/\/\/\ /   /    //              '
                 \\    '-._'-.___.-'_.-'    //
                  \\___.-'   '-...-'   '-.___//
                       ===[ >-o-< ]===
                    /  |               |  \
                  .'  /|   /\      /\   |\  '.
                 /  .' |  (  )    (  )  | '.  \
               _/  /   |   ~~      ~~   |   \  \_
              (__)/    '=======[]======='    \(__)
                  \      / /      \ \      /
                   \____/ /        \ \____/
                    (_o_)/          \(_o_)

   ██████╗ ██████╗ ███████╗███╗   ███╗██╗      ██████╗ ██████╗ ██████╗
  ██╔════╝ ██╔══██╗██╔════╝████╗ ████║██║     ██╔═══██╗██╔══██╗██╔══██╗
  ██║  ███╗██████╔╝█████╗  ██╔████╔██║██║     ██║   ██║██████╔╝██║  ██║
  ██║   ██║██╔══██╗██╔══╝  ██║╚██╔╝██║██║     ██║   ██║██╔══██╗██║  ██║
  ╚██████╔╝██║  ██║███████╗██║ ╚═╝ ██║███████╗╚██████╔╝██║  ██║██████╔╝
   ╚═════╝ ╚═╝  ╚═╝╚══════╝╚═╝     ╚═╝╚══════╝ ╚═════╝ ╚═╝  ╚═╝╚═════╝

          it's smart. it's in the wires. it's already in prod.
       gremlins break things. this one files the expense report.
             your tokens now answer to a higher authority.
```

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![GitHub stars](https://img.shields.io/github/stars/gremlord/gremlord)](https://github.com/gremlord/gremlord/stargazers)
[![Last commit](https://img.shields.io/github/last-commit/gremlord/gremlord)](https://github.com/gremlord/gremlord/commits/main)
[![Go](https://img.shields.io/github/go-mod/go-version/gremlord/gremlord)](go.mod)

**Run Claude Code on any model, with a budget.** Docs and demos: [gremlord.com](https://gremlord.com)

Formerly agentic — same tool, new name. See [Migrating from agentic](#migrating-from-agentic).

![gremlord demo](assets/hero.gif)

gremlord wraps Claude Code in a thin local router. Your sessions look and feel exactly like `claude` — same TUI, same tools, same updates — but the model behind them can be Anthropic, OpenAI, xAI, or anything OpenAI-compatible (Ollama, vLLM, OpenRouter, DeepSeek, Groq). Whole tasks can also be delegated to a locally logged-in Codex or Grok CLI under your own subscription. Every routed API token is metered, priced, and checked against budgets you set.

```bash
gremlord                  # Claude Code, tracked, on your default profile
gremlord -p cheap         # same session, cheaper models
gremlord --model grok     # one-off model override
gremlord cost             # where did today's $4.31 go?
```

## Why

Claude Code is a great harness, and it keeps getting better — forking it means losing that. But it only talks to one provider, and it doesn't answer two questions you eventually ask: *how much did that session cost?* and *can I run the cheap parts on a cheap model?*

gremlord answers both without touching Claude Code itself. Claude Code officially supports pointing at a gateway via `ANTHROPIC_BASE_URL`; gremlord is that gateway, plus the CLI around it.

## Compared to other options

There are three ways people solve "I want Claude Code but not locked to one provider":

| | gremlord | claude-code-router / claude-code-proxy forks / LiteLLM | OpenCode, Crush, Goose, Aider |
|---|---|---|---|
| **Harness** | Real Claude Code, unmodified, auto-updating | Real Claude Code, unmodified | Different harness entirely — own prompts, tools, TUI |
| **Runs as** | Static Go binary, no daemon (leader election over a fixed port) | Daemon / server process you deploy and administer | Standalone CLI you run instead of Claude Code |
| **Cost & budgets** | First-class CLI: `gremlord cost`, live statusline, hard-stop daily/weekly/monthly budgets | Usually a dashboard (LiteLLM) or not built in | Varies by tool, rarely budget-gated |
| **Model routing** | Aliases + a built-in LLM-classifier tier router (`auto`), sticky per turn | Rule-based routing configs; no classifier-based tiering | Manual model switch, no auto-routing |
| **Memory** | Composes with [clauder](https://github.com/MaorBril/clauder) — separate binary, optional | Not their concern | Varies |

The short version: gremlord doesn't try to be a better harness than Claude Code — it keeps Claude Code exactly as Anthropic ships it and only swaps what's behind `ANTHROPIC_BASE_URL`. If you want a different agent loop altogether, OpenCode/Crush/Goose/Aider are the right layer to look at instead. If you want a gateway you deploy and administer for a team, LiteLLM is a more mature choice for that. gremlord is for a single developer who wants `claude`, unmodified, with a budget and a cheap-model escape hatch, installed in one command and running with nothing to operate.

## How it works

```
gremlord (launcher) ──▶ claude (unmodified, auto-updating)
                          │  ANTHROPIC_BASE_URL
                          ▼
                   local router (127.0.0.1)
                   ├─ anthropic: byte-faithful passthrough
                   ├─ openai dialect: full request/stream translation
                   ├─ cli: whole-task delegation to local Codex/Grok
                   ├─ usage log (SQLite) + pricing
                   └─ budget gate
                          ▼
        Anthropic · OpenAI · xAI · Ollama · vLLM · OpenRouter · Codex CLI · Grok CLI · ...
```

There is no daemon. The first `gremlord` session binds the router port and serves everyone; when it exits, another running session takes over within a couple of seconds. The last session out turns off the lights.

Model names are aliases you define. Claude Code treats model IDs as opaque strings, so `ANTHROPIC_MODEL=grok` flows straight through and the router resolves it. Anything starting with `claude-` passes through to Anthropic untouched — background tasks keep working even when your main model is something else entirely.

## Migrating from agentic

Install gremlord and run it once. Your config, provider keys and full cost history
are **copied** from `~/.agentic` to `~/.gremlord` on that first run — the originals
are left untouched, so the old binary keeps working if you want to go back.

```bash
curl -fsSL https://raw.githubusercontent.com/gremlord/gremlord/main/install.sh | sh
gremlord setup          # repoints the statusline and the CLAUDE.md peers block
gremlord agents sync     # replaces agentic-* subagents with gremlord-*
gremlord doctor          # confirms the move and reports anything left behind
```

Already on the old binary? `agentic update` works for this release — it pulls the
renamed build, which migrates on its next run. You then still want `gremlord setup`
and `gremlord agents sync` from the list above.

**Restart your sessions.** This is the one step that is easy to miss and the only
one that loses anything. The router leader is whichever binary won the port, and an
agentic router that has been up for days keeps serving: new sessions health-check
it, find it alive, and follow it — so their spend keeps landing in `~/.agentic`
while `gremlord cost` reads `~/.gremlord` and shows nothing new. Nothing is
destroyed, but the two diverge until that old leader exits, and the copy has
already happened so it will not run again. `gremlord doctor` detects this and
prints the one-line `cp` that reconciles it. The old leader exits with its host
session.

What is deliberately **not** copied:

| Skipped | Why |
|---|---|
| `evals/`, `swebench-venv/` | Large, and both embed absolute paths — a Python venv does not survive being moved. Recreate the venv if you run SWE-bench. |
| `router.log*` | Rotates; regenerates on the next request. |
| `router.json` | Leader discovery for a router that is gone; a stale copy would point at a dead port. |

The spend database keeps the filename `agentic.db`. Renaming only the main file
orphans its `-wal` sidecar, which is exactly the history worth preserving.

Once `gremlord doctor` is happy and no old sessions remain, `rm -rf ~/.agentic`
reclaims the rest.

### Renamed surfaces

`AGENTIC_*` environment variables are now `GREMLORD_*`, and `AGENTIC_INSTALL_DIR`
is `GREMLORD_INSTALL_DIR`. The old names still work for one release, and the new
ones win when both are set. Request headers (`X-Agentic-*` → `X-Gremlord-*`) and
the router's control paths are written and served under both spellings this
release, so a gremlord launcher and an agentic router coexist on the port without
losing spend attribution.

`go install github.com/maorbril/agentic@latest` no longer works and cannot be
made to: GitHub redirects the path, but Go rejects the module because `go.mod`
declares `github.com/gremlord/gremlord`. Use that path instead.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/gremlord/gremlord/main/install.sh | sh
gremlord setup
```

Sixty seconds later: the same `claude` you know, now with `gremlord cost` telling you what today cost and a budget that stops the spend when you say so.

Or from source: `go install github.com/gremlord/gremlord@latest`

To update later, run `gremlord update` (or `gremlord update --check` to see if one's available without installing it). This updates gremlord itself; Claude Code keeps auto-updating on its own regardless.

## Configure

Everything lives in `~/.gremlord/config.yaml`, and everything is editable from the terminal:

```bash
gremlord providers add openai --type openai --base-url https://api.openai.com/v1 \
    --key-env OPENAI_API_KEY --max-tokens-param max_completion_tokens
gremlord models add gpt --provider openai --id gpt-5.2 --reasoning effort --max-output 16384
gremlord models test gpt          # 1-token probe: did I configure it right?

gremlord providers add codex --type cli --dialect codex --sandbox workspace-write
gremlord models add codex --provider codex   # subscription login; no API key

gremlord budget set --daily 25
```

Edits apply to live sessions immediately — the CLI hot-reloads the running router.

Launching `gremlord` holds the Gremlord banner for ten seconds before Claude Code
takes over. `splash_seconds: 0` in the config turns it off permanently, or
`GREMLORD_SPLASH_SECONDS=0` for one shell. It writes to stderr and is skipped
entirely when stderr is not a terminal, so pipes, CI and scripted launches never
wait on it.

A profile bundles a main model, a small/fast model for background tasks, tier mappings (so `/model opus` resolves inside the profile), and optional budgets:

```yaml
profiles:
  main:  {model: sonnet, small_fast: haiku, tiers: {opus: opus, sonnet: sonnet, haiku: haiku}}
  cheap: {model: gpt, small_fast: gpt, budget: {daily: 5.00}}
  local: {model: qwen, small_fast: qwen}
  subscription: {passthrough: true}   # plain claude, subscription billing, no tracking
```

Aliases backed by a `cli` provider are subagent-only: they cannot be a profile model, `small_fast`, or tier.

## Dynamic routing

Instead of picking models by hand, let a cheap LLM triage every task:

```bash
gremlord routing set auto --classifier haiku \
    --deep opus --standard sonnet --light qwen
```

`auto` now behaves like a model (`/model auto`, or `profiles: {model: auto}`). On each new user turn, the classifier reads the request and assigns a tier: planning and hard debugging go `deep`, ordinary coding goes `standard`, and mechanical edits and verification go `light`. The decision sticks for the rest of the turn, so tool results do not trigger another classification or flip models mid-flight. Classification failures fall back to `--default` (standard).

Task overrides add a second routing dimension without another classifier request. They are useful when two tasks need similar capability but perform better on different models. The supported labels are `implementation`, `sql_data`, `debugging`, `code_review`, `architecture`, `security_review`, and `critical_review`:

```yaml
models:
  opus:   {provider: anthropic, id: claude-opus-5}
  fable:  {provider: anthropic, id: claude-fable-5}
  grok:   {provider: xai,       id: grok-4.6, context_window: 500000}

routing:
  auto:
    classifier: haiku
    default: standard
    tiers: {deep: opus, standard: sonnet, light: qwen}
    tasks:
      implementation: grok
      sql_data: grok
      debugging: grok
      code_review: grok
      architecture: fable
      security_review: fable
      critical_review: opus
```

Configure the same policy from the CLI with repeatable task flags; later `routing set` calls preserve existing task mappings unless a matching flag updates one:

```bash
gremlord routing set auto --classifier haiku \
    --deep opus --standard sonnet --light qwen \
    --task implementation=grok --task sql_data=grok \
    --task debugging=grok --task code_review=grok \
    --task architecture=fable --task security_review=fable \
    --task critical_review=opus
gremlord routing list
```

A recognized task mapping takes precedence over the selected tier. Unrecognized or unmatched work uses the tier normally. If the mapped model cannot hold the request, the router falls back to the smallest eligible capability tier. Pinned sessions (`pin_tiers` / `X-Gremlord-Pin-Model`) bypass task and tier classification entirely. A `cli` alias cannot appear as a classifier, tier, or task mapping; validation rejects it so auto-routing can never silently start an independent agent run in your repository. Task classification is a model-selection hint, not a security boundary; enforce sensitive-work policy separately.

Every decision is logged, and task choices appear in the existing reason field:

```
$ grep autoroute ~/.gremlord/router.log
... alias=auto tier=standard model=grok reason=task:implementation
... alias=auto tier=deep model=fable reason=task:security_review
```

`gremlord cost --by model` then shows how spend actually distributed. Each turn uses one tiny tier-only or combined tier-and-task request to the classifier model (~$0.0005 with haiku). Because config parsing is strict, a config containing `tasks:` requires an gremlord binary that supports task routing; older binaries reject the field instead of silently ignoring it.

## Auto Goal

Whenever a `routing:` alias like `auto` is in play, a second, independent classifier pass looks at each new user turn and asks a different question: not "which tier?" but "does this look like it needs a persistent loop rather than a single reply?" — monitoring a long build, retrying until a condition holds, babysitting a deploy, polling for external state.

When the answer is yes, gremlord doesn't (and can't) start a loop itself — the router only ever sees request and response bodies, it has no execution context inside the Claude Code process. Instead it appends a system-reminder to the request naming the harness's own mechanisms directly:

```
<system-reminder>
gremlord: this task looks well suited to a recurring goal loop rather than a
single reply (polling a long build). If a persistent loop would help —
checking back on progress, retrying until a condition holds, babysitting a
long-running process — call ScheduleWakeup with prompt
"<<autonomous-loop-dynamic>>" and a reason, or invoke the /loop skill. This
is a suggestion, not a requirement: ignore it for tasks that finish in one
pass.
</system-reminder>
```

Claude Code decides whether to act on it — the reminder is a nudge, not a command. Decisions are logged (`grep autogoal ~/.gremlord/router.log`) and, like tier decisions, surfaced in the statusline (`⟳ goal (polling a long build)`).

This rides on the same classifier alias as dynamic routing, so it's on wherever `routing: auto` is configured, at the cost of one extra cheap classifier call per new turn alongside the tier call.

## Context scaling

Claude Code sizes its auto-compact against the ~200K window of the Claude model it thinks it's talking to. Routed models rarely match that: a local qwen holds 32K, GPT holds 400K, and many models get unreliable well before their advertised limit. Declare what a model really holds and the router scales every token count it reports so the client's context gauge — and therefore auto-compact — tracks the *real* window:

```bash
gremlord models add qwen --provider local --id qwen3-coder-30b --context-window 32768
gremlord models add glm  --provider z --id glm-4.7 --context-window 200000 --effective-context 60000
```

`effective_context` is the attention knob: the client compacts at 60K real tokens even though the window is nominally 200K, keeping the model in its coherent range. Pricing and budgets always record true usage; `gremlord context` shows a session's true-vs-reported trajectory for tuning these numbers.

Under `model: auto` the gauge is anchored to **one** budget for the whole rule — by default the largest window the rule can route to — so it means the same thing on every turn. Scaling per serving model instead made a conversation read 30% full on a 1M model and 95% on a 200K one, and since Claude Code compacts on whatever the last turn reported, a single cheap turn could throw away context the big model still had room for. Turns that outgrow a smaller tier are remapped up to one that fits, which the router already did for size. Set `context_gauge: min` on the rule to keep every tier reachable at any conversation length instead, or `context_gauge: model` for the old per-request behavior.

Details, the cost trade-off, and research methodology: [docs/context-scaling.md](docs/context-scaling.md).

## Token utilization

Three more things keep a session from spending window on nothing:

- **Estimator calibration.** Token counts for translated models are a character heuristic, deliberately biased high. The router measures that guess against what upstream actually billed (per model, from its own usage log) and corrects it, so an over-count stops being subtracted from every context budget it checks. `gremlord context` prints the measured accuracy; a model needs 20 requests before its correction is trusted, and corrections are clamped so a bad sample can never shrink an estimate into a window it will not fit.
- **Deferred tool loading.** Claude Code can hold back tool schemas — sending the deferred tools' names up front and a full schema only once the model pulls one in — but it switches that off whenever `ANTHROPIC_BASE_URL` is not a first-party Anthropic host, which the router never is. Sessions were paying every builtin and MCP schema on every request: measured at 186K of tool schemas inside a 200K frame on a session with 165 MCP tools, over the limit before the first turn. Sessions now ask for it back (`ENABLE_TOOL_SEARCH=true`). Deferral does need a model that calls `ToolSearch` when it sees a tool withheld, so a profile pinned to one that doesn't can set `tool_search: false`; an explicit value in your shell outranks both — `false` for the old eager behavior, `auto:N` to sample it. `gremlord eval` pins it off so stored runs stay comparable wherever they were launched from.
- **Cache-hit reporting.** `gremlord cost` shows what share of each model's input was served from an upstream prefix cache. Prompt caching is the single largest lever on the input bill, and it was previously invisible — worth checking before reaching for anything cleverer.

`gremlord context` also breaks down where a session's context goes — system prompt, tool schemas, conversation — since the first two are re-sent in full on every request whatever the turn is about.

## Model evaluations

`gremlord eval` compares a baseline model with a model under test on the same coding tasks. Each arm runs non-interactive Claude Code in isolation, records router usage and route decisions under its own session ID, and produces a patch. An optional judge sees blinded patches and verifier evidence; it never sees model names, cost, or execution order.

There are two executor types. A local manifest supplies its repository, setup command, and verifier directly:

```yaml
version: 1
name: local-sample
tasks:
  - id: django-11001
    repo: /path/to/prepared/django
    base: main
    prompt: Fix the issue described in this task. Run relevant tests.
    verifier:
      run: [python, -m, pytest, tests/example_test.py]
      timeout: 10m
```

A SWE-bench manifest delegates repository checkout, dependency setup, official task images, test-patch application, and FAIL_TO_PASS/PASS_TO_PASS grading to the pinned official harness:

```yaml
version: 1
name: swebench-smoke
dataset:
  type: swebench
  source: princeton-nlp/SWE-bench_Verified
  split: test
  tasks:
    - astropy__astropy-14309
sandbox:
  type: docker
```

The same manifest is in [`examples/swebench-smoke.yaml`](examples/swebench-smoke.yaml). SWE-bench runs require Docker and Python 3.10 or newer with the exact supported package version:

```bash
python3 -m venv ~/.gremlord/swebench-venv
~/.gremlord/swebench-venv/bin/pip install 'swebench==4.1.0'

gremlord eval run examples/swebench-smoke.yaml \
  --python ~/.gremlord/swebench-venv/bin/python \
  --baseline opus --mut auto --judge sonnet \
  --attempts 1 --timeout 45m --output ~/.gremlord/evals/swebench-smoke
gremlord eval report ~/.gremlord/evals/swebench-smoke
```

A real one-task run comparing `kimi-k3` against `opus` produced:

```text
swebench-smoke: baseline=opus mut=kimi-k3 judge=sonnet pairs=1
wins: baseline 1 · mut 0 · ties 0 · judge errors 0 · infra pairs 0
verifier passes: baseline 1 · mut 1 — run failures: baseline 0 · mut 0 · infra failures 0
TASK                    ATTEMPT  WINNER    BASELINE       MUT
astropy__astropy-14309  1        baseline  complete/pass  complete/pass
```

Both patches passed the official SWE-bench grader. The blinded judge preferred the baseline because it exactly matched the upstream fix and was narrower, while `kimi-k3` used a broader but still correct defensive fix. This is one smoke-test data point, not a statistically meaningful model ranking; use multiple tasks and attempts for comparisons you intend to act on.

The adapter checks Python, the exact SWE-bench API, and Docker before making a model request. It asks the official harness to build or reuse each instance image, starts a fresh candidate container, installs the Linux Claude Code native binary there, extracts the patch, and submits it to the official grader in a separate clean container. SWE-bench 4.1.0 builds x86_64 images; Docker Desktop runs them under emulation on Apple Silicon. Initial image builds can take a while.

Docker candidates reach the normal loopback-only router through a temporary relay. The relay binds an ephemeral host port for the eval duration, accepts only `/v1/*`, and requires the existing per-install router token. It shuts down when the run ends. Use `--keep-containers` only while debugging because it leaves candidate containers behind.

Use `--task id` to select instances, `--seed` to reproduce launch order and judge blinding, `--resume` to skip completed pairs, and `--json` for machine-readable output. Infrastructure failures are unscored and never sent to the judge; `--resume` retries those pairs. `--judge none` decides only from verifier pass/fail, with equal outcomes recorded as ties.

A local verifier reports its verdict through its exit code: `0` passed, `2` "I could not judge this candidate" — a missing toolchain, an image that is not present, a container that would not start — and any other non-zero means the candidate's work is wrong. Exit `2` is recorded as `verifier_error`, an infrastructure status, so the pair is unscored, hidden from the judge, and retried by `--resume`. Without that distinction a machine missing a Docker image scores as a model loss, which is worse than no measurement at all.

Artifacts include raw Claude output, patches, container logs, official SWE-bench reports, FAIL_TO_PASS/PASS_TO_PASS details, blinded judge mappings, per-candidate usage and route traces, pair results, the resolved dataset metadata/fingerprint, and `summary.json`. Local setup and verifier commands execute on the host, so review third-party local manifests before running them.

The router writes `~/.gremlord/router.log`. It is capped at 8 MiB and keeps three older generations (`router.log.1` … `.3`), so the log costs at most 32 MiB on disk. An oversized log left by an earlier version is rotated on the next write rather than truncated.


## Budgets

Daily, weekly, and monthly caps — global and per profile. When a cap is hit, the router refuses the *next* request with a clear message that shows up right in the Claude Code TUI; in-flight responses are never cut. Warnings surface in the statusline (`gremlord setup` registers it), which shows live session and daily spend:

```
main · sonnet · sess $0.84 · day $4.31/$25 [██░░░░]
```

`gremlord cost` breaks spend down by model, profile, or session, and `--json` gives you the raw rows.

## The fine print

Two things you should understand before routing through gremlord:

- **Billing.** Normal traffic through the router is billed to **API keys**, not your Claude Pro/Max subscription. OAuth credentials are never proxied. For Claude subscription billing, use a `passthrough: true` profile — normal claude, no tracking. [CLI delegation](#delegating-to-another-cli) instead bills the peer CLI's own subscription (ChatGPT or SuperGrok/X Premium+) through its cached local login, which gremlord invokes but never reads. That spend happens outside the router, so delegated runs appear in `gremlord cost` as $0 unpriced rows with estimated token counts and budgets cannot gate them.
- **Fidelity.** Non-Anthropic models work through translation, but Claude Code's prompts and tool patterns are tuned for Claude, so expect them to be clunkier in the main loop. They shine as cheap workhorses for background tasks and subagents. Specific gaps: no `cache_control` breakpoints on OpenAI-dialect backends (provider-side implicit caching still shows up as cache reads, measurable with `gremlord cost`), thinking blocks are display-only, Anthropic server tools (web search, code execution) are unavailable on translated models, `top_k` is dropped, stop sequences truncate to four (and are dropped entirely on the Responses flavor), and token counting for translated models is a deliberate ~15% overestimate so auto-compact fires early instead of overflowing context. OpenAI-compatible providers default to `/v1/chat/completions`; set `api: responses` on a provider or model (`gremlord models add … --api responses`) for OpenAI's `/v1/responses` — required for models like gpt-6-astra that reject function tools combined with `reasoning_effort` on chat completions. xAI, vLLM, and other compatible endpoints stay on chat completions unless they actually implement Responses. Set `max_output` on models whose output cap is below what Claude Code requests (it asks for 32K), and `context_window` on models whose window differs from the ~200K Claude Code assumes (see [Context scaling](#context-scaling)).

## Subagents on any model

Claude Code's built-in Agent tool takes a fixed `model` parameter (`sonnet | opus | haiku | fable`), so a routed alias like `qwen` can't be picked through it — subagents are stuck on the Claude tiers even when your best tool for the job is something else. A subagent *definition's* `model:` frontmatter has no such limit, and behind gremlord's local endpoint Claude Code passes that string straight through, so gremlord generates one subagent per configured model alias:

```bash
gremlord agents sync     # writes ~/.claude/agents/gremlord-<alias>.md, one per model alias
gremlord agents list     # what's implied by your config, and what's pending
```

Every model you've configured becomes selectable by name — `subagent_type: "gremlord-qwen"`, `"gremlord-grok"`, `"gremlord-gpt-5-6-sol"` — and its traffic routes, prices, and budgets like any other gremlord request. The set is derived from your own `models:` map, so it's whatever *you* configured; nothing is hardcoded.

When your aliases change, the next `gremlord` launch offers to refresh them (once — decline and it stays quiet until the aliases change again; `GREMLORD_NO_AGENT_SYNC=1` opts out entirely). Only files prefixed `gremlord-` are ever written or removed, so your own subagents are never touched.

Aliases backed by a `cli` provider get a deliberately different description: the generated definition tells the orchestrator that it is handing the entire task to an independent agent with filesystem access, so it writes a self-contained prompt and expects minutes of latency instead of a completion.

## Delegating to another CLI

If you already pay for ChatGPT or SuperGrok, a `cli` provider can delegate a whole task to the vendor's official, locally installed coding CLI under your own login:

```bash
# Install the CLI first, then authenticate it directly:
codex login                       # ChatGPT subscription
# or: grok login                  # SuperGrok / X Premium+ subscription

gremlord providers add codex --type cli --dialect codex --sandbox workspace-write
gremlord models add codex --provider codex
gremlord agents sync               # writes ~/.claude/agents/gremlord-codex.md
```

Invoking `subagent_type: "gremlord-codex"` hands that task prompt verbatim to `codex exec` (or `grok -p`) in the session's working directory. The CLI authenticates itself from its own cached login; gremlord neither extracts nor reuses its OAuth token. Set `--id` on the model alias only if you want to pass a specific model to the peer CLI. Use provider `--command` to override the binary and `--timeout-ms` to override the 20-minute default.

This is a whole agent loop, not a model completion: expect minutes of latency, no incremental output, and real file modifications. The delegated CLI sees only the one task prompt — no conversation history, system prompt, or Claude Code tool definitions — so make it self-contained. Its final message becomes the subagent result. Failures return as final message text instead of retryable stream errors, because silently repeating a filesystem-mutating run would be unsafe.

Delegation is deliberately explicit-only. A `cli` alias cannot be a profile model, `small_fast`, tier, auto classifier, or task mapping. Bulk `gremlord models test` also skips CLI aliases; name one explicitly (`gremlord models test codex`) only when you intend to launch a real delegation.

For Codex, choose the blast radius with `--sandbox read-only`, `workspace-write`, or `danger-full-access`. Delegation requires an `gremlord`-launched session so the router receives and validates its absolute working directory; a bare `claude` session is refused rather than running the CLI in the router leader's unrelated directory.

## Finding another session

Claude Code sessions can message each other, but addressing one is awkward: session names are auto-derived from whatever that session is doing (`daily-case-runtime`), so the name you'd actually say is the project directory — and `ListAgents`, the only thing that can mint the `[ref]` a cross-session `SendMessage` needs, doesn't show directories.

`gremlord peers` closes that gap by matching on both:

```
$ gremlord peers labs-service-secondlife-be
Best match for "labs-service-secondlife-be":
  daily-case-runtime             busy  ~/code/secondlife/labs-service         started 12h ago

To message it: call ListAgents for its [ref], then SendMessage to
"daily-case-runtime [ref]" — the bare name works after first contact.
```

With no argument it lists every session you can reach. Sessions on a build older than Claude Code 2.1.224 register no socket and are unreachable until restarted — they're reported as a count rather than silently omitted. When a query matches several sessions equally well, it says so instead of picking one.

`gremlord setup` writes this workflow into `~/.claude/CLAUDE.md`, between `<!-- gremlord:peers:start -->` markers, so every session — gremlord-launched or plain `claude` — knows to resolve names this way. Re-running setup refreshes that block and leaves the rest of the file alone.

## Works with clauder

gremlord spawns `claude` directly and grants each session an auto-approved tool set for autonomous operation (`Read Write Edit Glob Grep Bash(*) WebFetch WebSearch mcp__clauder__*`). `--name` becomes claude's own session name.

Cross-instance messaging is native to Claude Code — sessions register under `~/.claude/sessions` and reach each other over a peer socket — so gremlord no longer launches through `clauder wrap`. See [Finding another session](#finding-another-session) for addressing them. [clauder](https://github.com/MaorBril/clauder) remains a useful companion for **persistent memory**, which it provides over its own MCP server registration and therefore works no matter how the session was started. The two tools are independent; each works without the other.

`--no-clauder` is accepted but deprecated: every session is a bare claude now, so there is no wrap layer to opt out of.

## Commands

| Command | What it does |
|---|---|
| `gremlord [-p profile] [--model alias] [-- args]` | launch Claude Code (args after `--` go to claude) |
| `gremlord setup` | first-run config, token, statusline + peer-guidance registration |
| `gremlord peers [name]` | find another Claude Code session to message |
| `gremlord cost [--week\|--month] [--by model\|profile\|session]` | spend report |
| `gremlord context [session-id]` | context-fullness trajectory (true vs reported tokens) |
| `gremlord eval run/report` | paired model evaluation and artifact report |
| `gremlord agents list/sync` | subagent definitions for your model aliases |
| `gremlord models add/list/remove/test/update-prices` | model aliases (`test` skips CLI aliases unless one is named explicitly) |
| `gremlord providers add/list/remove` | API providers and official CLI delegates (`--type cli`) |
| `gremlord profiles list/show` · `gremlord budget set` | profiles and caps |
| `gremlord config get/set` | any config key |
| `gremlord router run/status` | headless router / who's leader |
| `gremlord doctor` | diagnose the installation |
| `gremlord update [--check]` | update gremlord itself to the latest release |

## Keys

Provider keys are referenced by environment variable name. They resolve in order: process environment → `~/.gremlord/env` (a `KEY=value` file, mode 0600, created by `setup`). Put keys in `~/.gremlord/env` — the router reads it directly, so sessions work no matter which shell launched them, and the config file never holds a secret.

`cli` providers have no gremlord key: `providers list` shows `· (subscription login)`, and `base_url` / `api_key_env` are rejected. Authentication belongs entirely to the official CLI.

## Security notes

The router binds `127.0.0.1` only and requires a per-install token (created by `setup`, mode 0600), so other local processes can't spend on your keys.

CLI delegation starts a local subprocess with filesystem access in the launching session's working directory. The launcher carries that directory in `X-Gremlord-Cwd`; the backend requires an absolute existing directory before spawning anything. Codex's `--sandbox` setting controls its write scope. Treat `danger-full-access` accordingly.

## License

MIT
