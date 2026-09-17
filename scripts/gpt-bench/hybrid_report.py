#!/usr/bin/env python3
"""Audit the hybrid experiment and export metrics without native transcripts."""
import json
import math
from pathlib import Path
import sqlite3
import sys

from sequence_report import report


def export(root):
    root = Path(root).resolve()
    report(root)
    environment = json.loads((root / 'environment.json').read_text())
    prices = environment['price_per_million']
    rows = [json.loads(line) for line in (root / 'requests.jsonl').read_text().splitlines()]
    summary = json.loads((root / 'summary.json').read_text())

    def cost(rr):
        total = 0
        for r in rr:
            rate = environment.get('coordinator_price_per_million', prices) if r.get('component') == 'coordinator' else prices
            total += ((r['input_tokens'] - r['cached_input_tokens'] - r['cache_write_tokens']) * rate['input']
                      + r['cached_input_tokens'] * rate['cache_read']
                      + r['cache_write_tokens'] * rate['cache_write']
                      + r['output_tokens'] * rate['output']) / 1e6
        return total

    audits = []
    for pair in summary['pairs']:
        for label in ('baseline', 'mut'):
            candidate = pair[label]
            if candidate['model'] != 'claude-codex':
                continue
            directory = root / 'tasks' / pair['task_id'] / f"attempt-{pair['attempt']:03d}" / label
            rr = [r for r in rows if r['session'] == candidate['session_id']]
            worker = [r for r in rr if r.get('component') == 'worker']
            coordinator = [r for r in rr if r.get('component') == 'coordinator']
            assert worker and coordinator and len(worker) + len(coordinator) == len(rr)
            state_dir = directory / 'home/codex-worker'
            state = json.loads((state_dir / 'state.json').read_text())
            threads, completions = [], 0
            for path in sorted(state_dir.glob('call-*/events.jsonl')):
                for line in path.read_text().splitlines():
                    try:
                        event = json.loads(line)
                    except ValueError:
                        continue
                    if event.get('type') == 'thread.started':
                        threads.append(event['thread_id'])
                    completions += event.get('type') == 'turn.completed'
            turns = len(json.loads((root / 'sequence.json').read_text()))
            assert len(threads) == state['calls'] >= turns
            assert set(threads) == {state['thread']}
            assert {r['turn'] for r in worker} == set(range(1, turns + 1))
            with sqlite3.connect(f"file:{directory / 'home/.gremlord/agentic.db'}?mode=ro", uri=True) as db:
                db.row_factory = sqlite3.Row
                own = [dict(r) for r in db.execute('SELECT * FROM usage_events')]
            assert len(own) == len(worker), 'worker requests missing from Gremlord ledger'
            assert sum(r['input_tokens'] + r['cache_read_tokens'] + r['cache_write_tokens'] for r in own) == sum(r['input_tokens'] for r in worker)
            for stored, measured in [('output_tokens', 'output_tokens'), ('cache_read_tokens', 'cached_input_tokens'), ('cache_write_tokens', 'cache_write_tokens')]:
                assert sum(r[stored] for r in own) == sum(r[measured] for r in worker), stored
            assert math.isclose(sum(r['cost_usd'] for r in own), cost(worker), abs_tol=1e-9)
            assert math.isclose(candidate['usage']['cost_usd'], cost(rr), abs_tol=1e-9)
            components = {}
            for name, selected in [('coordinator', coordinator), ('worker', worker)]:
                components[name] = dict(requests=len(selected), estimated_usd=cost(selected),
                                        upstream_seconds=sum(r['duration_ms'] for r in selected) / 1000,
                                        models=sorted({r.get('model', environment['model']) for r in selected}))
                for k in ('input_tokens', 'cached_input_tokens', 'cache_write_tokens', 'output_tokens', 'reasoning_tokens'):
                    components[name][k] = sum(r[k] for r in selected)
            audits.append(dict(task=pair['task_id'], attempt=pair['attempt'], components=components,
                               worker_calls=state['calls'], same_thread=True,
                               completed_worker_calls=completions, worker_final_status=state['status'],
                               native_worker_workflow_completed=(completions == state['calls'] and state['status'] == 'completed'),
                               unknown_usage_requests=sum(r['status'] == 200 and not r.get('response_status') for r in rr),
                               worker_ledger_matches=True, total_cost_includes_coordinator=True))
    assert audits, 'no completed hybrid workflows to audit'
    safe_environment = {k: v for k, v in environment.items() if k in (
        'model', 'effort', 'context_budget', 'seed', 'attempts', 'codex_version', 'claude_version',
        'max_output_tokens_per_request', 'max_requests_per_candidate', 'price_per_million',
        'manifest_sha256', 'executable_sha256', 'gremlord_binary_sha256', 'scope', 'baseline', 'mut',
        'coordinator_model', 'coordinator_price_per_million', 'coordinator_context_budget')
        or k.endswith('.go_sha256') or k.endswith('.py_sha256')}
    return dict(environment=safe_environment, metrics=json.loads((root / 'metrics.json').read_text()),
                hybrid_audits=audits, requests=[{k: v for k, v in r.items() if k not in ('session', 'error')} for r in rows])


if __name__ == '__main__':
    result = export(sys.argv[1])
    Path(sys.argv[2]).write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(dict(arms=result['metrics']['arms'], hybrid_audits=result['hybrid_audits']), indent=2))
