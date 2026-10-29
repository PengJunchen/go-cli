import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import eval_core
import run_trial

NEW_PROFILE = 'main-bash300-personal-memory-off-v2'
FIXTURE = json.loads((Path(__file__).parent / 'fixtures/profile-v1.json').read_text())


class ProfileContract(unittest.TestCase):
    def test_default_profile_remains_exactly_compatible(self):
        self.assertEqual(eval_core.PROFILE_ID, FIXTURE['profile_id'])
        self.assertEqual(eval_core.go_config(FIXTURE['endpoint']), FIXTURE['go_config'])
        self.assertEqual(eval_core.pi_config(FIXTURE['endpoint']), FIXTURE['pi_config'])
        for arm in ('go', 'pi'):
            self.assertEqual(eval_core.cli_argv(arm, FIXTURE['prompt']), FIXTURE[arm + '_argv'])

    def test_new_go_config_changes_only_host_bash_window(self):
        expected = copy.deepcopy(FIXTURE['go_config'])
        expected['tools'] = {'bash': {'timeout': '300s'}}
        self.assertEqual(eval_core.go_config(FIXTURE['endpoint'], profile_id=NEW_PROFILE), expected)
        self.assertEqual(eval_core.pi_config(FIXTURE['endpoint']), FIXTURE['pi_config'])
        self.assertEqual(eval_core.cli_argv('pi', FIXTURE['prompt']), FIXTURE['pi_argv'])
        with patch.dict(os.environ, {'GO_CLI_TOOLS_BASH_TIMEOUT': '999s', 'GO_CLI_LINUX_SUBREAPER': '1'}):
            self.assertEqual(eval_core.go_config(FIXTURE['endpoint']), FIXTURE['go_config'])
            self.assertEqual(eval_core.go_config(FIXTURE['endpoint'], profile_id=NEW_PROFILE), expected)

    def test_resolver_is_finite_and_returns_fresh_metadata(self):
        old = eval_core.resolve_profile(eval_core.PROFILE_ID)
        new = eval_core.resolve_profile(NEW_PROFILE)
        self.assertEqual(old['configured_go_bash_execution_window_seconds'], 30)
        self.assertEqual(new['configured_go_bash_execution_window_seconds'], 300)
        self.assertEqual(new['go_supervisor_mode'], 'off')
        new['configured_go_bash_execution_window_seconds'] = 999
        self.assertEqual(eval_core.resolve_profile(NEW_PROFILE)['configured_go_bash_execution_window_seconds'], 300)
        for invalid in ('', '300s', 'custom', None, [], {}):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                eval_core.resolve_profile(invalid)

    def test_invalid_profile_rejected_before_any_output_task_or_docker(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / 'must-not-exist'
            with patch.object(run_trial, 'docker') as docker, patch.object(Path, 'read_text', side_effect=AssertionError('task read before validation')):
                with self.assertRaises(ValueError):
                    run_trial.run_trial(Path('/missing-task'), 'go', output, profile_id='custom')
            self.assertFalse(output.exists())
            docker.assert_not_called()

    def test_environment_evidence_rejects_unknown_on_or_duplicates(self):
        self.assertEqual(run_trial.validate_go_supervisor_environment(['OTHER=x', 'GO_CLI_LINUX_SUBREAPER=0'])['effective_value'], '0')
        for invalid in (None, [], ['GO_CLI_LINUX_SUBREAPER=1'], ['GO_CLI_LINUX_SUBREAPER='],
                        ['GO_CLI_LINUX_SUBREAPER=0', 'GO_CLI_LINUX_SUBREAPER=0'],
                        ['GO_CLI_LINUX_SUBREAPER'], [None]):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                run_trial.validate_go_supervisor_environment(invalid)

    def test_cli_choices_and_default_do_not_execute_invalid_profile(self):
        for arm in ('go', 'pi'):
            for profile in (None, NEW_PROFILE):
                args = ['run_trial.py', '--task', '/missing-task', '--out', '/missing-output', '--arm', arm]
                if profile:
                    args += ['--profile', profile]
                with patch('sys.argv', args), patch.object(run_trial, 'run_trial') as execute:
                    run_trial.main()
                    self.assertEqual(execute.call_args.kwargs['profile_id'], profile or eval_core.PROFILE_ID)
        with patch('sys.argv', ['run_trial.py', '--task', '/missing-task', '--out', '/missing-output', '--arm', 'go', '--profile', '300s']), patch.object(run_trial, 'run_trial') as execute, contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as rejected:
                run_trial.main()
            self.assertEqual(rejected.exception.code, 2)
            execute.assert_not_called()


class ProfileDriverContract(unittest.TestCase):
    def run_fake_trial(self, arm, profile, environment=None):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        task, runner, output = root / 'synthetic-task', root / 'runner', root / 'output'
        task.mkdir(); runner.mkdir()
        (task / 'instruction.md').write_text(FIXTURE['prompt'])
        (task / 'task.toml').write_text('[task]\nname="synthetic"\n[environment]\ndocker_image="repo:tag"\n[agent]\ntimeout_sec=3000\n')
        entry = {'task_id': task.name, 'instruction_sha256': run_trial.sha(task / 'instruction.md'), 'task_toml_sha256': run_trial.sha(task / 'task.toml')}
        (runner / 'dataset-lock.json').write_text(json.dumps({'tasks': [entry]}))
        payload = runner / 'artifacts/payload'
        (payload / 'node/bin').mkdir(parents=True)
        (payload / 'pi/node_modules/@earendil-works/pi-coding-agent').mkdir(parents=True)
        (payload / 'go-cli').write_bytes(b'synthetic fixed binary')
        (payload / 'node/bin/node').write_bytes(b'synthetic fixed node')
        (payload / 'pi/node_modules/@earendil-works/pi-coding-agent/package.json').write_text('{}')
        (payload / 'manifest.json').write_text(json.dumps({'go_cli_source_revision': 'a' * 40, 'go_cli_sha256': run_trial.sha(payload / 'go-cli')}))
        calls = []
        def docker(*args, **kwargs):
            calls.append(args)
            stdout = ''
            if args[:2] == ('image', 'inspect'):
                stdout = json.dumps([{'Id': 'sha256:' + '1' * 64}])
            elif args[:2] == ('network', 'inspect'):
                stdout = json.dumps([{'IPAM': {'Config': [{'Gateway': '172.20.0.1'}]}}])
            elif args[0] == 'create':
                stdout = 'relay' if '--name' in args else 'task'
            elif args[0] == 'inspect':
                stdout = json.dumps([{'Config': {'Env': environment if environment is not None else ['GO_CLI_LINUX_SUBREAPER=0']}}])
            elif args[:3] == ('exec', 'task', 'python3') and args[4] == run_trial.PROBE:
                stdout = json.dumps({'relay': True, 'github': False, 'pypi': False, 'npm': False, 'host_gateway': False, 'public_ip': False, 'internal_host_gateway': False})
            elif args[:3] == ('exec','task','python3') and args[4] == run_trial.CONFIG_IDENTITY_PROBE:
                stdout=json.dumps({'identity':{'uid':0,'gid':0,'euid':0,'egid':0},'paths_safe':True})
            elif args[:3] == ('exec','task','python3') and args[4] == run_trial.CONFIG_READBACK_PROBE:
                stdout=args[5]
            elif args[:4] == ('exec', 'task', 'git', 'rev-parse'):
                stdout = 'b' * 40
            return subprocess.CompletedProcess(['docker', *args], 0, stdout, '')
        with patch.object(run_trial, 'copy_native_config_archive'), patch.object(run_trial, 'ROOT', runner), patch.object(run_trial, 'docker', side_effect=docker), patch.object(run_trial, 'resolve_locked_image', return_value=('repo@sha256:' + '1' * 64, {'Id': 'sha256:' + '1' * 64})), patch.object(run_trial, 'run_cli_once', return_value={'exit_code': 0, 'elapsed_seconds': 0.0, 'cli_invocations': 1}) as execute, contextlib.redirect_stdout(io.StringIO()):
            error = None
            try:
                run_trial.run_trial(task, arm, output, profile_id=profile)
            except ValueError as rejected:
                error = rejected
        result = json.loads((output / 'result.json').read_text())
        return output, result, calls, execute, error

    def test_both_arms_share_profile_but_pi_configuration_and_budget_stay_native(self):
        for arm in ('go', 'pi'):
            output, result, calls, execute, error = self.run_fake_trial(arm, NEW_PROFILE)
            self.assertIsNone(error)
            self.assertEqual(result['profile_id'], NEW_PROFILE)
            self.assertEqual(result['configured_go_bash_execution_window_seconds'], 300)
            self.assertEqual(result['go_bash_execution_window_source'], 'host_profile_declaration')
            self.assertEqual(result['go_supervisor_mode'], 'off')
            self.assertEqual(result['go_supervisor_environment_validation']['effective_value'], '0')
            self.assertEqual(result['task_timeout_seconds'], 3000)
            self.assertEqual(result['max_upstream_requests'], 80)
            self.assertEqual(json.loads((output / 'configs/go.json').read_text())['tools'], {'bash': {'timeout': '300s'}})
            self.assertEqual(json.loads((output / 'configs/models.json').read_text()), FIXTURE['pi_config'])
            self.assertEqual(execute.call_args.args[1], FIXTURE[arm + '_argv'])
            task_create = next(call for call in calls if call[0] == 'create' and '--name' not in call)
            self.assertIn('GO_CLI_LINUX_SUBREAPER=0', task_create)
            for filename, expected in result['config_sha256'].items():
                self.assertEqual(hashlib.sha256((output / 'configs' / filename).read_bytes()).hexdigest(), expected)

    def test_old_profile_has_no_new_environment_override_or_inspect(self):
        output, result, calls, execute, error = self.run_fake_trial('go', eval_core.PROFILE_ID)
        self.assertIsNone(error)
        self.assertEqual(json.loads((output / 'configs/go.json').read_text()), FIXTURE['go_config'])
        task_create = next(call for call in calls if call[0] == 'create' and '--name' not in call)
        self.assertNotIn('GO_CLI_LINUX_SUBREAPER=0', task_create)
        self.assertFalse(any(call[0] == 'inspect' for call in calls))
        self.assertEqual(result['configured_go_bash_execution_window_seconds'], 30)
        self.assertIsNone(result['go_supervisor_environment_validation'])

    def test_new_profile_wrong_effective_mode_stops_before_container_start_or_cli(self):
        output, result, calls, execute, error = self.run_fake_trial('go', NEW_PROFILE, ['GO_CLI_LINUX_SUBREAPER=1'])
        self.assertIsNotNone(error)
        execute.assert_not_called()
        self.assertNotIn(('start', 'task'), calls)
        self.assertEqual(result['infrastructure_error'], 'ValueError')


if __name__ == '__main__':
    unittest.main()
