import argparse
import json
import os
from pathlib import Path
import tempfile
import unittest

from hybrid_worker import run


class WorkerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fake = self.root / 'fake-gremlord'
        self.fake.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
assert args[:4] == ['--harness', 'codex', '--model', 'astra']
assert not any(k.startswith('ANTHROPIC_') for k in os.environ)
prompt = sys.stdin.read()
if 'resume' in args:
    assert args[-2] == 'thread-123'
thread = 'wrong-thread' if prompt == 'wrong' else 'thread-123'
print(json.dumps(dict(type='thread.started', thread_id=thread)))
if prompt == 'fail':
    print(json.dumps(dict(type='turn.failed')))
    sys.exit(1)
pathlib.Path(args[args.index('-o') + 1]).write_text(prompt)
print(json.dumps(dict(type='turn.completed')))
''')
        self.fake.chmod(0o700)
        self.args = argparse.Namespace(gremlord=str(self.fake), model='astra', profile='',
            state_dir=str(self.root / 'state'), cwd=str(self.root),
            continue_after_failure=False, benchmark_isolation=True,
            dangerously_bypass_approvals_and_sandbox=False)

    def test_resume_keeps_identity_and_returns_only_final(self):
        first = run(self.args, 'first task')
        second = run(self.args, 'follow-up')
        self.assertFalse(first['resumed'])
        self.assertTrue(second['resumed'])
        self.assertEqual(first['thread_id'], second['thread_id'])
        self.assertEqual(second['result'], 'follow-up')
        self.args.model = 'other'
        with self.assertRaisesRegex(ValueError, 'another directory/model/profile'):
            run(self.args, 'cannot swap model in opaque history')

    def test_failure_is_not_silently_retried_or_fresh_started(self):
        with self.assertRaisesRegex(RuntimeError, 'native turn failed'):
            run(self.args, 'fail')
        with self.assertRaisesRegex(ValueError, 'previous work did not complete'):
            run(self.args, 'retry')
        self.args.continue_after_failure = True
        self.assertTrue(run(self.args, 'explicit recovery')['resumed'])

    def test_changed_native_thread_is_a_failure(self):
        run(self.args, 'first')
        with self.assertRaisesRegex(RuntimeError, 'changed thread identity'):
            run(self.args, 'wrong')
        state = json.loads((self.root / 'state/state.json').read_text())
        self.assertEqual(state['thread'], 'thread-123')
        self.assertEqual(state['status'], 'failed')


if __name__ == '__main__':
    unittest.main()
