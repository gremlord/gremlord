#!/usr/bin/env python3
"""Report resumed workflows, keeping every attempt and failed/missing stage."""
import json
import statistics
import sys
from pathlib import Path


def report(root):
    root = Path(root).resolve()
    if (root / "EXCLUDED.json").exists():
        raise ValueError("Run excluded; see EXCLUDED.json")
    summary = json.loads((root / "summary.json").read_text())
    env = json.loads((root / "environment.json").read_text())
    rows = [json.loads(s) for s in (root / "requests.jsonl").read_text().splitlines()]
    turns = len(json.loads((root / "sequence.json").read_text()))
    candidates, metrics, detail = [], {}, []
    for pair in summary["pairs"]:
        for label in ["baseline", "mut"]:
            c = pair[label]
            arm = c["model"]
            directory = root / "tasks" / pair["task_id"] / f"attempt-{pair['attempt']:03d}" / label
            grades = {g["turn"]: g for p in sorted(directory.glob("turn-*/grade.json"))
                      for g in [json.loads(p.read_text())]}
            rr = sorted((r for r in rows if r["session"] == c["session_id"]), key=lambda r: r["request"])
            passed = sum(g["passed"] and not g.get("execution_error") for g in grades.values())
            agent = sum(g["agent_ms"] for g in grades.values()) / 1000
            # Older drivers did not save failed-turn durations.
            if c["status"] != "complete" and not any(g.get("execution_error") for g in grades.values()):
                agent = c["agent_ms"] / 1000
            item = dict(task=pair["task_id"], attempt=pair["attempt"], arm=arm,
                        checkpoints_passed=passed, checkpoints_expected=turns,
                        checkpoints_recorded=len(grades), agent_seconds=agent,
                        requests=len(rr), estimated_usd=c["usage"]["cost_usd"],
                        status=c["status"], final_passed=c["verifier"]["passed"],
                        api_errors=sum(bool(r.get("error")) or r["status"] != 200 for r in rr),
                        unknown_usage_requests=sum(r["status"] == 200 and not r.get("response_status") for r in rr))
            for field in ["input_tokens", "cached_input_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens", "tool_calls"]:
                item[field] = sum(r.get(field, 0) for r in rr)
            item["upstream_seconds"] = sum(r["duration_ms"] for r in rr) / 1000
            outputs = [r["first_output_ms"] / 1000 for r in rr if r.get("first_output_ms") is not None]
            item["median_first_output_seconds"] = statistics.median(outputs) if outputs else None
            candidates.append(item)
            m = metrics.setdefault(arm, dict(attempts=0, workflows_passed=0, checkpoints_passed=0,
                    checkpoints_expected=0, agent_seconds=0, estimated_usd=0, requests=0,
                    api_errors=0, unknown_usage_requests=0, input_tokens=0, cached_input_tokens=0, cache_write_tokens=0, output_tokens=0,
                    reasoning_tokens=0, tool_calls=0, upstream_seconds=0))
            m["attempts"] += 1
            m["workflows_passed"] += c["status"] == "complete" and c["verifier"]["passed"] and passed == turns
            for field in m.keys() - {"attempts", "workflows_passed"}:
                m[field] += item[field]
            for turn in range(1, turns + 1):
                g = grades.get(turn)
                tr = [r for r in rr if r["turn"] == turn]
                first = tr[0] if tr else {}
                verdict = "missing" if g is None else "execution error" if g.get("execution_error") else "pass" if g["passed"] else "FAIL"
                seconds = f"{g['agent_ms']/1000:.1f}" if g else "—"
                detail.append(f"| {arm} | {pair['task_id']} | {pair['attempt']} | {turn} | {verdict} | {seconds} | {len(tr)} | {first.get('encrypted_reasoning_input_items', '—')} |")
    lines = ["# Multi-turn GPT benchmark", "",
             f"Model: `{env['model']}`, effort `{env['effort']}`. Each workflow has {turns} user turns in the same session and workspace.", "",
             "| Arm | Workflows passed | Checkpoints | Agent seconds | API requests | Tool calls | Estimated USD |",
             "| --- | --- | --- | --- | --- | --- | --- |"]
    if env.get('coordinator_model'):
        lines[3:3] = [f"The hybrid coordinator uses `{env['coordinator_model']}`; its worker uses `{env['model']}`. Costs include both models at their recorded rates.", ""]
    for arm, m in metrics.items():
        lower_bound = "≥ " if m['unknown_usage_requests'] else ""
        lines.append(f"| {arm} | {m['workflows_passed']}/{m['attempts']} | {m['checkpoints_passed']}/{m['checkpoints_expected']} | {m['agent_seconds']:.1f} | {m['requests']} | {m['tool_calls']} | {lower_bound}{m['estimated_usd']:.4f} |")
    lines += ["", "| Arm | Task | Attempt | User turn | Cumulative checks | Seconds | Requests | Encrypted reasoning items on first request |",
              "| --- | --- | --- | --- | --- | --- | --- | --- |"] + detail
    lines += ["", "All repetitions are included above. A later successful turn cannot erase a failed earlier checkpoint. Follow-up prompts contain changed requirements; grader feedback is not sent to the models. Model/effort settings are recorded above; isolated homes/workspaces and frozen external graders are used for both arms.",
              "", "Input includes cache reads and writes once; output includes reasoning. Cache writes are priced separately when the meter records them. Costs use the recorded local rates and are estimates, not invoices. First-output time means the first nonempty upstream delta (text, summary, or tool arguments), not an invisible reasoning token. API durations include networking and streaming. Instruction/description bytes count decoded UTF-8; tool schemas, parameter schemas, and visible input count serialized JSON. None are token estimates.",
              "", "A ≥ cost has incomplete metering: an interrupted successful HTTP stream returned no terminal usage. The recorded spend omits that request's unknown billed tokens. Timed-out workflows also completed less work, so their raw time/cost cannot be compared as successful-completion latency/cost. Cancellations are retained in request-error counts.",
              "", "Repeated attempts of a workflow are not independent tasks. No general parity or compaction claim follows from this run. Inspect requests.jsonl, per-turn grades/patches, and environment.json for evidence.", ""]
    (root / "report.md").write_text("\n".join(lines))
    (root / "metrics.json").write_text(json.dumps(dict(arms=metrics, candidates=candidates), indent=2) + "\n")
    return "\n".join(lines)


if __name__ == "__main__":
    print(report(sys.argv[1]))
