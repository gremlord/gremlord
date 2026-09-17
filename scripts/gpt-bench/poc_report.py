#!/usr/bin/env python3
"""Reconcile native PoC usage with the independent meter; export safe metrics."""
import json
import math
from pathlib import Path
import sys

from sequence_report import report


def export(root):
    root = Path(root).resolve()
    report(root)
    metrics = json.loads((root / 'metrics.json').read_text())
    env = json.loads((root / 'environment.json').read_text())
    requests = [json.loads(line) for line in (root / 'requests.jsonl').read_text().splitlines()]
    summary = json.loads((root / 'summary.json').read_text())
    audits = []
    for pair in summary['pairs']:
        for label in ('baseline', 'mut'):
            candidate = pair[label]
            if candidate['model'] != 'codex-gremlord':
                continue
            path = root / 'tasks' / pair['task_id'] / f"attempt-{pair['attempt']:03d}" / label
            for directory in sorted(path.glob('turn-*')):
                turn = int(directory.name.split('-')[1])
                own = json.loads((directory / 'gremlord-usage.json').read_text())
                meter = [r for r in requests if r['session'] == candidate['session_id'] and r['turn'] == turn]
                assert len(own) == len(meter), (turn, 'request count mismatch')
                expected_input = sum(r['input_tokens'] for r in meter)
                assert sum(r['InputTokens'] + r['CacheReadTokens'] + r['CacheWriteTokens'] for r in own) == expected_input
                for stored, metered in [('OutputTokens', 'output_tokens'), ('CacheReadTokens', 'cached_input_tokens'), ('CacheWriteTokens', 'cache_write_tokens')]:
                    assert sum(r[stored] for r in own) == sum(r[metered] for r in meter), (turn, stored)
                prices = env['price_per_million']
                cost = sum(((r['input_tokens'] - r['cached_input_tokens'] - r['cache_write_tokens']) * prices['input']
                            + r['cached_input_tokens'] * prices['cache_read']
                            + r['cache_write_tokens'] * prices['cache_write']
                            + r['output_tokens'] * prices['output']) / 1e6 for r in meter)
                assert math.isclose(sum(r['CostUSD'] for r in own), cost, abs_tol=1e-9), (turn, 'cost mismatch')
                assert all(r['Status'] == 200 and not r['ErrType'] for r in own)
                audits.append(dict(task=pair['task_id'], attempt=pair['attempt'], turn=turn,
                                   requests=len(own), tokens_match=True, cost_matches=True,
                                   gateway_extra_ms=sum(r['DurationMS'] for r in own) - sum(r['duration_ms'] for r in meter)))
    assert audits, 'no native PoC ledgers found'
    safe_env = {k: env[k] for k in ('model', 'effort', 'context_budget', 'seed', 'attempts', 'codex_version',
                'max_output_tokens_per_request', 'max_requests_per_candidate', 'price_per_million',
                'manifest_sha256', 'executable_sha256', 'gremlord_binary_sha256')}
    safe_requests = [{k: v for k, v in r.items() if k not in ('session', 'error')} for r in requests]
    return dict(environment=safe_env, metrics=metrics, ledger_audits=audits, requests=safe_requests,
                scope='One paired three-turn synthetic planner workflow. Token cost estimates, not invoices; no general parity claim.')


if __name__ == '__main__':
    data = export(sys.argv[1])
    destination = Path(sys.argv[2])
    destination.write_text(json.dumps(data, indent=2) + '\n')
    print(json.dumps(dict(arms=data['metrics']['arms'], ledger_audits=data['ledger_audits']), indent=2))
