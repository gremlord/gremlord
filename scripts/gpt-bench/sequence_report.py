#!/usr/bin/env python3
"""Report every checkpoint of a resumed multi-user-turn benchmark."""
import json
import sys
from pathlib import Path

root=Path(sys.argv[1]).resolve()
summary=json.loads((root/'summary.json').read_text())
env=json.loads((root/'environment.json').read_text())
rows=[json.loads(s) for s in (root/'requests.jsonl').read_text().splitlines()]
lines=['# Multi-turn durable queue benchmark','',
       f"Model: `{env['model']}`, effort `{env['effort']}`. One workflow per harness, three separate user turns in the same session and workspace.",'',
       '| Harness | Checkpoints passed | Final cumulative grader | Agent seconds | API requests | Estimated USD |',
       '| --- | --- | --- | --- | --- | --- |']
detail=[];metrics={}
for pair in summary['pairs']:
    for label in ['baseline','mut']:
        c=pair[label];arm=c['model']
        directory=root/'tasks'/pair['task_id']/f"attempt-{pair['attempt']:03d}"/label
        grades=[json.loads(p.read_text()) for p in sorted(directory.glob('turn-*/grade.json'))]
        request_rows=[r for r in rows if r['session']==c['session_id']]
        passed=sum(g['passed'] for g in grades)
        agent=sum(g['agent_ms'] for g in grades)/1000
        lines.append(f"| {arm} | {passed}/{len(json.loads((root/'sequence.json').read_text()))} | {c['verifier']['passed']} ({c['status']}) | {agent:.1f} | {len(request_rows)} | {c['usage']['cost_usd']:.4f} |")
        metrics[arm]=dict(checkpoints_passed=passed,checkpoints_recorded=len(grades),agent_seconds=agent,requests=len(request_rows),estimated_usd=c['usage']['cost_usd'],status=c['status'],final_passed=c['verifier']['passed'])
        for g in grades:
            rr=[r for r in request_rows if r['turn']==g['turn']]
            first=rr[0] if rr else {}
            detail.append(f"| {arm} | {g['turn']} | {'pass' if g['passed'] else 'FAIL'} | {g['agent_ms']/1000:.1f} | {len(rr)} | {first.get('encrypted_reasoning_input_items', 'missing')} | {first.get('effective_reasoning_context','missing')} |")
lines+=['','| Harness | User turn | Cumulative checks | Seconds | Requests | Encrypted reasoning items on first request | Effective context |',
        '| --- | --- | --- | --- | --- | --- | --- |']+detail
lines+=['','The three checkpoints are: durable queue and atomic claims; crash recovery with fenced leases/retries; atomic batches and legacy-schema migration. Each cumulative grader exercises multiple connections. A database written by the first implementation is retained and checked after later changes. Legacy migration checks preserve IDs, dedupe keys, payloads, priority, availability and terminal state.',
        '', 'Follow-up prompts contain only changed requirements. The same native session IDs are resumed. Grader results are saved but are not sent back as hints; both arms receive the identical scripted follow-ups. Per-turn patches, output and grader logs are retained. Encrypted reasoning counts on the first request of turns 2 and 3 measure replay across user-turn boundaries.',
        '', 'This is one complex workflow, not three independent benchmark tasks. It does not force context compaction and does not prove general parity. Token costs use the recorded local prices. See requests.jsonl and the per-turn artifacts for exact evidence.','']
(root/'report.md').write_text('\n'.join(lines))
(root/'metrics.json').write_text(json.dumps(metrics,indent=2)+'\n')
print('\n'.join(lines))
