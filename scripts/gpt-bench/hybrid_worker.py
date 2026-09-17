#!/usr/bin/env python3
"""Experimental resumable Codex worker, metered by the Gremlord native PoC.

Read one task from stdin. Each state directory owns one native conversation.
No subscription fallback, shell interpolation, or automatic retry of failed work.
"""
import argparse
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys


def save(path, state):
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(state))
    temporary.replace(path)


def run(args, task):
    if not task.strip():
        raise ValueError('task on stdin is empty')
    root = Path(args.state_dir).resolve()
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    cwd = str(Path(args.cwd).resolve())
    with (root / 'lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        state_path = root / 'state.json'
        identity = dict(cwd=cwd, model=args.model, profile=args.profile)
        state = json.loads(state_path.read_text()) if state_path.exists() else dict(identity, calls=0)
        if any(state.get(k) != v for k, v in identity.items()):
            raise ValueError('worker state belongs to another directory/model/profile; use a new state directory')
        if state.get('status') in ('running', 'failed') and not args.continue_after_failure:
            raise ValueError('previous work did not complete; inspect artifacts and use --continue-after-failure to resume deliberately')
        if state.get('calls') and not state.get('thread'):
            raise ValueError('previous launch has no thread ID; inspect artifacts and use a new state directory')
        previous_thread = state.get('thread')
        state['calls'] += 1
        call_dir = root / f"call-{state['calls']:03d}"
        call_dir.mkdir(mode=0o700)
        command = [args.gremlord, '--harness', 'codex', '--model', args.model]
        if args.profile:
            command += ['--profile', args.profile]
        command += ['--', 'exec']
        if previous_thread:
            command.append('resume')
        final_path = call_dir / 'final.txt'
        command += ['--json', '--skip-git-repo-check', '-o', str(final_path)]
        if args.benchmark_isolation:
            command += ['--ignore-user-config', '--ignore-rules']
            for setting in ['web_search="disabled"', 'agents.enabled=false',
                            'features.multi_agent=false', 'features.multi_agent_v2=false',
                            'features.skills=false', 'features.apps=false', 'features.plugins=false',
                            'shell_environment_policy.inherit="all"']:
                command += ['-c', setting]
        if args.dangerously_bypass_approvals_and_sandbox:
            command.append('--dangerously-bypass-approvals-and-sandbox')
        else:
            command += ['-c', 'sandbox_mode="workspace-write"', '-c', 'approval_policy="never"']
        if previous_thread:
            command.append(previous_thread)
        command.append('-')
        # Claude routing credentials are not needed by the native child. Gremlord
        # obtains its local gateway token itself; provider secrets stay in its router.
        env = {k: v for k, v in os.environ.items() if not k.startswith('ANTHROPIC_')
               and k not in ('CLAUDE_CODE_SUBAGENT_MODEL', 'OPENAI_API_KEY', 'CODEX_API_KEY',
                            'GREMLORD_CODEX_TOKEN', 'GREMLORD_SESSION_ID', 'GREMLORD_PROFILE')}
        state['status'] = 'running'
        save(state_path, state)
        completed = False
        failure = None
        with (call_dir / 'events.jsonl').open('w') as events, (call_dir / 'stderr.log').open('w') as errors:
            # Inherit the caller's process group. The caller owns cleanup; tools
            # that detach descendants need additional lifecycle handling.
            process = subprocess.Popen(command, cwd=cwd, env=env, stdin=subprocess.PIPE,
                                       stdout=subprocess.PIPE, stderr=errors, text=True)
            process.stdin.write(task)
            process.stdin.close()
            for line in process.stdout:
                events.write(line)
                events.flush()
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if event.get('type') == 'thread.started':
                    thread = event.get('thread_id')
                    if not thread or (previous_thread and thread != previous_thread):
                        failure = 'native resume changed thread identity'
                    else:
                        state['thread'] = thread
                        save(state_path, state)
                if event.get('type') == 'turn.completed':
                    completed = True
                if event.get('type') == 'turn.failed':
                    failure = 'native turn failed'
            process.stdout.close()
            code = process.wait()
        if code or not completed or not state.get('thread') or failure or not final_path.exists():
            state['status'] = 'failed'
            save(state_path, state)
            raise RuntimeError(f'{failure or "worker did not complete"}; exit={code}; inspect {call_dir}; partial edits may exist')
        state['status'] = 'completed'
        save(state_path, state)
        return dict(status='completed', thread_id=state['thread'], resumed=bool(previous_thread),
                    result=final_path.read_text(), artifacts=str(call_dir))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--gremlord', default='gremlord')
    parser.add_argument('--model', required=True)
    parser.add_argument('--profile', default='')
    parser.add_argument('--state-dir', required=True)
    parser.add_argument('--cwd', default=os.getcwd())
    parser.add_argument('--continue-after-failure', action='store_true')
    parser.add_argument('--benchmark-isolation', action='store_true')
    parser.add_argument('--dangerously-bypass-approvals-and-sandbox', action='store_true')
    args = parser.parse_args()
    os.umask(0o077)
    try:
        print(json.dumps(run(args, sys.stdin.read())))
    except (OSError, ValueError, RuntimeError) as error:
        print(json.dumps(dict(status='failed', error=str(error))))
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
