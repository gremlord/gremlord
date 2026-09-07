#!/usr/bin/env bash
# Builds a throwaway HOME for recording assets/hero.tape, so the demo GIF shows
# a synthetic usage log and a curated model lineup instead of the maintainer's
# real spend, keys, and internal provider URLs.
#
#   DEMO_HOME=$(assets/hero-demo-home.sh)
#   HOME="$DEMO_HOME" vhs assets/hero.tape
#
# ~/.claude is symlinked through to the real one, so the recorded `claude` looks
# like a normal install; only gremlord's own state (~/.gremlord) is synthetic.
# The router runs on its own port so it never disturbs live sessions.
set -euo pipefail

REAL_HOME="${REAL_HOME:-$HOME}"
DEMO="${1:-$(mktemp -d -t gremlord-hero)}"
mkdir -p "$DEMO/.gremlord"

cat > "$DEMO/.gremlord/config.yaml" <<'YAML'
version: 1
default_profile: main
router:
    port: 41199   # not the real 41100: recording must not fight a live leader
providers:
    anthropic:
        type: anthropic
        base_url: https://api.anthropic.com
        api_key_env: ANTHROPIC_API_KEY
    openai:
        type: openai
        base_url: https://api.openai.com/v1
        api_key_env: OPENAI_API_KEY
        max_tokens_param: max_completion_tokens
    xai:
        type: openai
        base_url: https://api.x.ai/v1
        api_key_env: XAI_API_KEY
    local:
        type: openai
        base_url: http://localhost:11434/v1
models:
    opus:   {provider: anthropic, id: claude-opus-5, context_window: 200000}
    sonnet: {provider: anthropic, id: claude-sonnet-5, context_window: 200000}
    haiku:  {provider: anthropic, id: claude-haiku-4-5, context_window: 200000}
    gpt:    {provider: openai, id: gpt-5.2, reasoning: effort, max_output: 16384, context_window: 400000}
    grok:   {provider: xai, id: grok-4.6, context_window: 500000}
    qwen:   {provider: local, id: "qwen3-coder:30b", context_window: 32768}
profiles:
    main:
        # A catalogued alias, not `auto`: Claude Code prints a five-line warning
        # for a model name its own catalog doesn't know, which lands in frame.
        model: sonnet
        small_fast: haiku
        tiers: {opus: opus, sonnet: sonnet, haiku: haiku}
    cheap:
        model: qwen
        small_fast: qwen
        budget: {daily: 5.00}
budgets:
    daily: 25
    monthly: 400
    warn_at: 0.8
    hard_stop: true
pricing:
    claude-opus-5:    {input: 5.0,  output: 25.0, cache_read: 0.5, cache_write: 6.25}
    gpt-5.2:          {input: 1.25, output: 10.0, cache_read: 0.125}
    grok-4.6:         {input: 2.0,  output: 6.0,  cache_read: 0.5}
    "qwen3-coder:30b": {input: 0.0, output: 0.0,  cache_read: 0.0}
routing:
    auto:
        classifier: haiku
        default: standard
        tiers: {deep: opus, standard: sonnet, light: qwen}
        tasks:
            implementation: grok
            architecture: opus
YAML

# Real provider keys, so the KEY column is honest. They never render on screen —
# `models list` prints only ✓/✗ — and this HOME is a throwaway outside the repo.
if [ -f "$REAL_HOME/.gremlord/env" ]; then
    cp "$REAL_HOME/.gremlord/env" "$DEMO/.gremlord/env"
else
    printf 'ANTHROPIC_API_KEY=demo\nOPENAI_API_KEY=demo\nXAI_API_KEY=demo\n' > "$DEMO/.gremlord/env"
fi
[ -f "$REAL_HOME/.gremlord/token" ] && cp "$REAL_HOME/.gremlord/token" "$DEMO/.gremlord/token"
chmod 600 "$DEMO/.gremlord/config.yaml" "$DEMO/.gremlord/env" "$DEMO/.gremlord/token" 2>/dev/null || true

# Claude Code keeps its real config, so the splash is a normal install — but
# with MCP servers dropped, since an unauthenticated one paints a warning across
# the hero frame that has nothing to do with gremlord.
ln -sfn "$REAL_HOME/.claude" "$DEMO/.claude"
if [ -e "$REAL_HOME/.claude.json" ]; then
    python3 - "$REAL_HOME/.claude.json" "$DEMO/.claude.json" <<'PYEOF'
import json, sys
src, dst = sys.argv[1], sys.argv[2]
with open(src) as f:
    d = json.load(f)
d.pop("mcpServers", None)
for proj in d.get("projects", {}).values():
    if isinstance(proj, dict):
        proj.pop("mcpServers", None)
        proj.pop("enabledMcpjsonServers", None)
with open(dst, "w") as f:
    json.dump(d, f)
PYEOF
fi

# A synthetic day: one aggregate row per model. Costs are computed from the
# pricing above, so the breakdown is internally consistent.
NOW=$(date +%s)
rm -f "$DEMO/.gremlord/agentic.db"
sqlite3 "$DEMO/.gremlord/agentic.db" <<SQL
CREATE TABLE usage_events (
  id                INTEGER PRIMARY KEY,
  ts                INTEGER NOT NULL,
  session_id        TEXT,
  profile           TEXT,
  provider          TEXT,
  model             TEXT,
  model_alias       TEXT,
  input_tokens      INTEGER,
  output_tokens     INTEGER,
  cache_read_tokens INTEGER,
  cache_write_tokens INTEGER,
  cost_usd          REAL,
  priced            INTEGER,
  request_id        TEXT,
  status            INTEGER,
  err_type          TEXT
);
CREATE INDEX idx_usage_ts ON usage_events(ts);
INSERT INTO usage_events
  (ts, session_id, profile, provider, model, model_alias,
   input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
   cost_usd, priced, status)
VALUES
  ($((NOW-5400)), 'demo-a', 'main',  'anthropic', 'claude-sonnet-5',   'sonnet', 380000, 60000, 900000, 60000, 2.535,  1, 200),
  ($((NOW-4200)), 'demo-a', 'main',  'xai',       'grok-4.6',          'grok',   240000, 45000, 620000,     0, 1.060,  1, 200),
  ($((NOW-3000)), 'demo-b', 'main',  'openai',    'gpt-5.2',           'gpt',    150000, 26000, 400000,     0, 0.4975, 1, 200),
  ($((NOW-1800)), 'demo-b', 'main',  'anthropic', 'claude-haiku-4-5',  'haiku',  106000, 14000, 210000,  8000, 0.207,  1, 200),
  ($((NOW-900)),  'demo-c', 'cheap', 'local',     'qwen3-coder:30b',   'qwen',   520000, 28000,      0,     0, 0.0,    1, 200);
SQL

echo "$DEMO"
