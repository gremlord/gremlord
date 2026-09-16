"""The report must not overwrite repetitions or hide incomplete workflows."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('sequence_report', Path(__file__).with_name('sequence_report.py'))
module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)


class ReportTests(unittest.TestCase):
    def test_repetitions_and_missing_checkpoints(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root/'environment.json').write_text(json.dumps(dict(model='gpt-test', effort='high')))
            (root/'sequence.json').write_text('[{},{}]')
            pairs, rows = [], []
            for attempt in (1, 2):
                pair = dict(task_id='workflow', attempt=attempt)
                for label, arm in [('baseline','gremlord'),('mut','gpt-efficient')]:
                    sid = f'{arm}-{attempt}'
                    broken = label == 'mut' and attempt == 2
                    pair[label] = dict(model=arm, session_id=sid, status='timeout' if broken else 'complete',
                        verifier=dict(passed=not broken), agent_ms=3000, usage=dict(cost_usd=.1))
                    for turn in ([1] if broken else [1,2]):
                        directory=root/'tasks'/'workflow'/f'attempt-{attempt:03d}'/label/f'turn-{turn:02d}'
                        directory.mkdir(parents=True)
                        (directory/'grade.json').write_text(json.dumps(dict(turn=turn,passed=True,agent_ms=1000)))
                        rows.append(dict(session=sid,arm=arm,request=turn,turn=turn,status=200,duration_ms=500))
                pairs.append(pair)
            (root/'summary.json').write_text(json.dumps(dict(pairs=pairs)))
            (root/'requests.jsonl').write_text('\n'.join(map(json.dumps,rows)))
            text=module.report(root)
            metrics=json.loads((root/'metrics.json').read_text())
            self.assertEqual(len(metrics['candidates']),4)
            self.assertEqual(metrics['arms']['gremlord']['checkpoints_passed'],4)
            self.assertEqual(metrics['arms']['gpt-efficient']['checkpoints_passed'],3)
            self.assertEqual(metrics['arms']['gpt-efficient']['checkpoints_expected'],4)
            self.assertEqual(metrics['arms']['gpt-efficient']['workflows_passed'],1)
            self.assertEqual(metrics['arms']['gpt-efficient']['agent_seconds'],5)
            self.assertIn('missing',text)
            (root/'EXCLUDED.json').write_text('{}')
            with self.assertRaises(ValueError): module.report(root)


if __name__ == '__main__': unittest.main()
