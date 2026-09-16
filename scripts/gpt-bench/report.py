#!/usr/bin/env python3
"""Summarize paired pilot results; never silently turn infrastructure errors into losses."""
import json
import statistics
import sys
from pathlib import Path

root = Path(sys.argv[1]).resolve()
if (root / "EXCLUDED.json").exists():
    raise SystemExit("Run was excluded during validation; see EXCLUDED.json")
summary = json.loads((root / "summary.json").read_text())
env = json.loads((root / "environment.json").read_text())
requests = [json.loads(line) for line in (root / "requests.jsonl").read_text().splitlines()]
out = ["# GPT harness pilot", "", f"Model: `{env['model']}`; reasoning: `{env['effort']}`; declared context budget: {env['context_budget']:,} tokens.",
       f"Claude Code {env['claude_version']}; native {env['codex_version']}.", "",
       "Three synthetic tasks with repeated attempts. This is a small local pilot, not evidence of general Codex parity, a before/after ablation, or a compaction benchmark.", "",
       "| Harness | Passed | Median seconds | Total seconds | API requests | Input tokens (incl. cache) | Cached input | Output tokens | Estimated USD |",
       "| --- | --- | --- | --- | --- | --- | --- | --- | --- |"]
results = {}
for label, arm in [("baseline", "codex"), ("mut", "gremlord")]:
    candidates = [p[label] for p in summary["pairs"]]
    rows = [r for r in requests if r["arm"] == arm]
    problems = [c for c in candidates if c["status"] != "complete"]
    problems += [r for r in rows if r.get("error") or r["status"] != 200]
    good = sum(c["status"] == "complete" and c["verifier"]["passed"] for c in candidates)
    times = [c["agent_ms"] / 1000 for c in candidates]
    cost = sum(c["usage"]["cost_usd"] for c in candidates)
    out.append(f"| {arm} | {good}/{len(candidates)} | {statistics.median(times):.1f} | {sum(times):.1f} | {len(rows)} | {sum(r['input_tokens'] for r in rows):,} | {sum(r['cached_input_tokens'] for r in rows):,} | {sum(r['output_tokens'] for r in rows):,} | {cost:.4f} |")
    results[arm] = dict(passed=good, attempts=len(candidates), median_seconds=statistics.median(times), total_seconds=sum(times), requests=len(rows), estimated_usd=cost, errors=len(problems), encrypted_replay_requests=sum(r["encrypted_reasoning_input_items"] > 0 for r in rows), encrypted_output_items=sum(r["encrypted_reasoning_output_items"] for r in rows))
    if problems:
        out.extend(["", f"**{arm}: {len(problems)} execution/API problems. Inspect artifacts before interpreting the comparison.**", ""])
out += ["", "| Task | Attempt | Native Codex | Gremlord | Codex seconds | Gremlord seconds |", "| --- | --- | --- | --- | --- | --- |"]
for p in summary["pairs"]:
    a, b = p["baseline"], p["mut"]
    def verdict(c):
        return ("pass" if c["verifier"]["passed"] else "fail") if c["status"] == "complete" else c["status"]
    out.append(f"| {p['task_id']} | {p['attempt']} | {verdict(a)} | {verdict(b)} | {a['agent_ms']/1000:.1f} | {b['agent_ms']/1000:.1f} |")
out += ["", "Wire measurements:", ""]
for arm in ["codex", "gremlord"]:
    r = results[arm]
    rows = [x for x in requests if x["arm"] == arm]
    out.append(f"- {arm}: encrypted reasoning present on {r['encrypted_replay_requests']}/{r['requests']} input requests; {r['encrypted_output_items']} encrypted reasoning output items; {sum(x['reasoning_tokens'] for x in rows):,} reasoning output tokens; {r['errors']} execution/API problems.")
    out.append(f"- {arm} protocol: Responses Lite on {sum(x.get('responses_lite', False) for x in rows)}/{len(rows)} requests; requested reasoning contexts {sorted(set(x.get('requested_reasoning_context') or 'model default' for x in rows))}; effective contexts {sorted(set(x.get('effective_reasoning_context') or 'not returned' for x in rows))}.")
    assert all(x["effort"] == "high" for x in rows), "effort mismatch"
out += ["", "Input includes cached tokens exactly once. Output includes reasoning tokens. Dollar amounts use the recorded local per-million rates, not a billing invoice. Repeated requests benefit from provider caching, so request order and cache rates matter.",
        "", "The shared proxy pins the model and effort and caps each response at 32,768 output tokens. Both harnesses get fresh homes/workspaces, no MCP/web/subagents, identical task prompts and frozen external graders. Seeded paired launch order reduces order bias; native prompts/tool schemas remain different. Claude uses its default prompt in safe mode (not bare mode).",
        "", "The long-context task supplies a 193 KB synthetic dossier in the initial prompt, with tenant isolation, superseded revisions, unsigned records, and boundary cases. It measures extraction and application of rules below the compaction threshold; it does not test model retention after compaction or router restart.",
        "", "Artifacts include frozen manifest/source, runtime versions, request metadata, exact candidate patches, verifier logs, candidate output, and SQLite usage. Grading is deterministic; there is no LLM judge. No benchmark p-value or broad quality ranking is justified by three tasks.", ""]
(root / "report.md").write_text("\n".join(out))
(root / "metrics.json").write_text(json.dumps(results, indent=2) + "\n")
print("\n".join(out))
