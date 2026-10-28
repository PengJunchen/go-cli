#!/usr/bin/env python3
"""Official-asset black-box preflight; no model requests and no gold-content output."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / 'third_party/swe-bench-pro/v2/tasks'
OUT = ROOT / 'artifacts/preflight'

def call(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)

IMAGE_DIGESTS = {
    'instance_flipt-io__flipt-b2170346dc37cf42fda1386cd630f24821ad2ac5': 'sha256:2f7f1326835b2423e79b7225a9e384c25ff38ada8ea8b3c10da3b0223865b7ee',
    'instance_navidrome__navidrome-1e96b858a91c640fe64e84c5e5ad8cc0954ea38d': 'sha256:b45146e8c3ce77bdc2294fdaa8fa1d7ae2eb8680b3c832e5c41b24de87910d6f',
    'instance_future-architect__vuls-436341a4a522dc83eb8bddd1164b764c8dd6bc45': 'sha256:726bd27db30702f31a1b21a4914c4bdec072b0d0abde686fcabb21013f866146',
}

def image(task):
    return 'ghcr.io/scaleapi/swe-bench_pro-v2@' + IMAGE_DIGESTS[task]

def probe(task):
    fix = re.search(r'-([0-9a-f]{40})(?:-|$)', task)[1]
    script = r'''set -u
cd /app 2>/dev/null || cd /testbed
printf 'HEAD='; git rev-parse HEAD
printf 'COMMITS='; git rev-list --all --count
printf 'REFS='; git for-each-ref --format='%(refname)' | wc -l
printf 'STASHES='; git stash list | wc -l
printf 'FIXING_COMMIT_VISIBLE='; if git cat-file -e "$1^{commit}" 2>/dev/null; then echo true; else echo false; fi
printf 'TESTS_DIR='; test -e /tests && echo present || echo absent
printf 'SOLUTION_DIR='; test -e /solution && echo present || echo absent
printf 'HOOKS_CUSTOM='; git config --get core.hooksPath || printf '\n'
printf 'WORKTREE_DIRTY_COUNT='; git status --porcelain | wc -l
printf 'TIMEOUT_PATH='; command -v timeout || true
printf 'PYTHON3_PATH='; command -v python3 || true
printf 'CONTEXT_FILES='; find . -path './.git' -prune -o -type f \( -name AGENTS.md -o -name CLAUDE.md \) -print | tr '\n' '|'; printf '\n'
printf 'PARENT_CONTEXT_FILES='; for f in /AGENTS.md /CLAUDE.md /root/AGENTS.md /root/CLAUDE.md; do if test -f "$f"; then printf '%s|' "$f"; fi; done; printf '\n'
'''
    result = subprocess.run(['docker', 'run', '--rm', '--platform', 'linux/amd64',
        '--network', 'none', '--cpus', '1', '--memory', '4g', image(task),
        'bash', '-c', script, 'preflight', fix], capture_output=True, text=True, timeout=90)
    data = dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)
    report = {'task': task, 'exit_code': result.returncode, 'observed': data,
              'stderr': result.stderr, 'fixing_sha_checked': fix,
              'note': 'Commit visibility only; no fixing source or patch was read.'}
    OUT.mkdir(parents=True, exist_ok=True)
    (OUT / (task + '-integrity.json')).write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report), flush=True)

def control(task, kind):
    task_dir = SOURCE / task
    output = OUT / task / kind
    output.mkdir(parents=True, exist_ok=False)
    report = {'task': task, 'control': kind, 'expected_reward': 1 if kind == 'oracle' else 0,
              'platform': 'linux/amd64', 'image': image(task), 'cpus': 1, 'memory_bytes': 4 * 1024**3,
              'network': 'bridge (official verifier public phase)', 'model_requests': 0,
              'gold_contents_exposed': False, 'timeout_seconds': 3000}
    cid = call(['docker', 'create', '--name', 'eval-control-' + uuid.uuid4().hex[:10],
                '--platform', 'linux/amd64', '--network', 'bridge', '--cpus', '1',
                '--memory', '4g', image(task), 'sleep', 'infinity'],
                capture_output=True, text=True).stdout.strip()
    report['container_id'] = cid
    started = time.monotonic()
    try:
        call(['docker', 'cp', str(task_dir / 'tests') + '/.', cid + ':/tests'])
        if kind == 'oracle':
            call(['docker', 'cp', str(task_dir / 'solution') + '/.', cid + ':/solution'])
        call(['docker', 'start', cid], capture_output=True)
        call(['docker', 'exec', cid, 'mkdir', '-p', '/logs/verifier'])
        if kind == 'oracle':
            with (output / 'oracle-apply.log').open('wb') as logs:
                result = subprocess.run(['docker', 'exec', cid, 'bash', '/solution/solve.sh'],
                                        stdout=logs, stderr=subprocess.STDOUT, timeout=300)
            report['oracle_apply_exit'] = result.returncode
            if result.returncode != 0:
                report['status'] = 'oracle_apply_environment_error'
                return
        with (output / 'verifier.log').open('wb') as logs:
            result = subprocess.run(['docker', 'exec', cid, 'timeout', '-k', '10', '3000',
                                     'bash', '/tests/test.sh'], stdout=logs,
                                    stderr=subprocess.STDOUT, timeout=3050)
        report['verifier_exit'] = result.returncode
        reward = subprocess.run(['docker', 'exec', cid, 'cat', '/logs/verifier/reward.txt'],
                                 capture_output=True, text=True, timeout=20)
        report['reward'] = int(reward.stdout.strip()) if reward.returncode == 0 and reward.stdout.strip() in ('0', '1') else None
        report['matched_expected'] = report['reward'] == report['expected_reward']
        report['status'] = 'completed'
        subprocess.run(['docker', 'cp', cid + ':/logs/verifier/.', str(output / 'verifier')],
                       capture_output=True, timeout=30)
        parsed = output / 'verifier/output.json'
        if parsed.is_file():
            import collections
            payload = json.loads(parsed.read_text())
            tests = payload.get('tests', [])
            report['parsed_test_count'] = len(tests)
            report['test_status_counts'] = dict(collections.Counter(t.get('status', 'unknown') for t in tests))
    except subprocess.TimeoutExpired:
        report['status'] = 'control_timeout'
        report['matched_expected'] = False
    finally:
        report['elapsed_seconds'] = round(time.monotonic() - started, 2)
        state = subprocess.run(['docker', 'inspect', '--format', '{{json .State}}', cid],
                               capture_output=True, text=True, timeout=20)
        if state.returncode == 0:
            report['container_state'] = json.loads(state.stdout)
        subprocess.run(['docker', 'rm', '-f', cid], capture_output=True, timeout=30)
        (output / 'result.json').write_text(json.dumps(report, indent=2) + '\n')
        # Only scalar outcomes, never reference contents or full test outputs.
        print(json.dumps(report), flush=True)

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['probe', 'control'])
    parser.add_argument('--task', required=True)
    parser.add_argument('--kind', choices=['empty', 'oracle'])
    args = parser.parse_args()
    if not (SOURCE / args.task / 'task.toml').is_file():
        parser.error('Unknown frozen task')
    if args.action == 'probe':
        probe(args.task)
    else:
        if args.kind is None:
            parser.error('--kind required')
        control(args.task, args.kind)
