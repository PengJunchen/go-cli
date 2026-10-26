import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

root = Path(__file__).resolve().parents[5]
target = sys.argv[1]
if target not in ('verify', 'test-e2e'):
    raise SystemExit('unsupported verification target')
out = root / 'docs/qa/2026-10-14/live/gates'
out.mkdir(parents=True, exist_ok=True)
log = out / (target + '.log')
env = dict(os.environ)
env['PATH'] = '/Users/yinyuepeng/go/bin:' + env.get('PATH', '')
started = time.time()
package_result = subprocess.run(['rtk', 'proxy', 'go', 'list', './...'], cwd=root, env=env, text=True, capture_output=True)
packages = package_result.stdout.splitlines()
qa_packages = [name for name in packages if '/docs/qa/' in name or '/scripts/e2e_live/' in name]
if package_result.returncode or qa_packages:
    raise SystemExit('verification package inventory failed or includes temporary QA generator modules')
workspace_module = root / 'docs/qa/2026-10-14/live/workspaces/go.mod'
workspace_module_hash = hashlib.sha256(workspace_module.read_bytes()).hexdigest() if workspace_module.exists() else None
source_paths = ['internal', 'cmd', 'tests', 'go.mod', 'go.sum', 'Makefile', '.golangci.yml']
source_before = subprocess.check_output(['rtk', 'proxy', 'git', 'diff', 'HEAD', '--binary', '--', *source_paths], cwd=root)
head_before = subprocess.check_output(['rtk', 'proxy', 'git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
untracked_before = subprocess.check_output(['rtk', 'proxy', 'git', 'ls-files', '--others', '--exclude-standard', '--', *source_paths], cwd=root, text=True).splitlines()
if source_before or untracked_before:
    raise SystemExit('final verification requires clean committed source; commit or freeze/review source first')
# Preserve earlier results, including failures, rather than overwriting history.
if log.exists():
    old_json = out / (target + '.json')
    old = json.loads(old_json.read_text()) if old_json.exists() else {}
    stamp = old.get('startedCommit', 'unknown')[:8] + '-' + str(log.stat().st_mtime_ns)
    archived_log = target + '-' + stamp + '.log'
    log.rename(out / archived_log)
    if old_json.exists():
        old['originalLogName'] = old.get('log')
        old['log'] = archived_log
        old['archiveNote'] = 'Only log path updated when preserving previous run; underlying log bytes and logSHA256 remain unchanged.'
        old_json.write_text(json.dumps(old, ensure_ascii=False, indent=2) + '\n')
        old_json.rename(out / (target + '-' + stamp + '.json'))
with log.open('w') as stream:
    result = subprocess.run(['rtk', 'proxy', 'make', target], cwd=root, env=env, stdout=stream, stderr=subprocess.STDOUT)
body = log.read_text(errors='replace')
source_after = subprocess.check_output(['rtk', 'proxy', 'git', 'diff', 'HEAD', '--binary', '--', *source_paths], cwd=root)
head_after = subprocess.check_output(['rtk', 'proxy', 'git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
untracked_after = subprocess.check_output(['rtk', 'proxy', 'git', 'ls-files', '--others', '--exclude-standard', '--', *source_paths], cwd=root, text=True).splitlines()
stable_source = source_before == source_after and head_before == head_after and untracked_before == untracked_after
lines = body.splitlines()
signals = [line for line in lines if re.search(r'go test observed|go test event failure|ERROR:|0 issues|Verification complete|Scan Report|Files scanned|Findings:|^make:|^exit status|^go test .*failed|^go test timed out', line)]
summary = {
    'target': target,
    'projectPackages': packages,
    'temporaryQAPackages': qa_packages,
    'qaWorkspaceModuleSHA256': workspace_module_hash,
    'command': 'make ' + target,
    'exitCode': result.returncode,
    'durationSeconds': round(time.time() - started, 3),
    'baselineCommit': head_after,
    'startedCommit': head_before,
    'sourceDiffBeforeSHA256': hashlib.sha256(source_before).hexdigest(),
    'sourceDiffAfterSHA256': hashlib.sha256(source_after).hexdigest(),
    'sourceUnchangedDuringRun': stable_source,
    'untrackedSourceBefore': untracked_before,
    'untrackedSourceAfter': untracked_after,
    'accepted': result.returncode == 0 and stable_source,
    'worktreeDiffSHA256': hashlib.sha256(subprocess.check_output(['rtk', 'proxy', 'git', 'diff', '--binary'], cwd=root)).hexdigest(),
    'log': log.name,
    'logSHA256': hashlib.sha256(log.read_bytes()).hexdigest(),
    'signals': signals,
}
(out / (target + '.json')).write_text(json.dumps(summary, ensure_ascii=False, indent=2) + '\n')
print(json.dumps(summary, ensure_ascii=False, indent=2))
raise SystemExit(result.returncode if stable_source else 3)
