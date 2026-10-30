"""Finite task-init profile contracts. Docker/native/model are fully mocked."""
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import run_trial
import eval_core
import test_profiles

PROFILE='main-bash300-personal-memory-off-usage-observe-init-v4'

class TaskInitContracts(unittest.TestCase):
    def setUp(self):
        self.helper=test_profiles.ProfileDriverContract(methodName='test_old_profile_has_no_new_environment_override_or_inspect')
        self.addCleanup(self.helper.doCleanups)
        temp=tempfile.TemporaryDirectory();self.addCleanup(temp.cleanup)
        self.alias=Path(temp.name)/'alias.json'
        self.alias.write_text(json.dumps({'aliases':{eval_core.MODEL:eval_core.MODEL},'evidence_sha256':'a'*64}))

    def test_new_profile_requires_same_frozen_alias_before_output_or_docker(self):
        out=self.alias.parent/'not-created'
        with patch.object(run_trial,'docker')as docker:
            with self.assertRaises(ValueError):run_trial.run_trial(Path('/no-task'),'go',out,profile_id=PROFILE)
        docker.assert_not_called();self.assertFalse(out.exists())

    def test_only_explicit_v4_task_create_requests_init_and_records_actual_true(self):
        _,v,calls,native,error=self.helper.run_fake_trial('go',PROFILE,alias_manifest=self.alias,task_init=True)
        self.assertIsNone(error)
        task=next(x for x in calls if x[0]=='create'and'--name'not in x)
        relay=next(x for x in calls if x[0]=='create'and'--name'in x)
        self.assertIn('--init',task);self.assertEqual(task.count('--init'),1);self.assertNotIn('--init',relay)
        self.assertEqual(v['profile_id'],PROFILE);self.assertEqual(v['native_config_profile_id'],test_profiles.NEW_PROFILE)
        self.assertEqual(v['task_docker_init_requested'],True)
        self.assertEqual(v['task_docker_init_observation']['observed_init'],True)
        self.assertTrue(v['task_docker_init_observation']['before_container_start'])
        self.assertEqual(v['reasoning_effort'],'high');self.assertEqual(v['max_output_tokens'],16384)
        self.assertEqual(v['max_upstream_requests'],80);self.assertEqual(v['task_timeout_seconds'],3000)
        self.assertEqual(v['cpus'],1);self.assertEqual(v['memory_bytes'],4*1024**3)
        self.assertEqual(v['go_supervisor_environment_validation']['effective_value'],'0')
        self.assertEqual(native.call_count,1);self.assertEqual(native.call_args.args[1],test_profiles.FIXTURE['go_argv'])
        capture=next(x for x in calls if x[:1]==('exec',)and len(x)>4 and x[4]==run_trial.KEEPER_CAPTURE)
        collection=next(x for x in calls if x[:1]==('exec',)and len(x)>4 and x[4]==run_trial.COLLECT)
        native_event=next(x for x in calls if x[0]=='native')
        self.assertLess(calls.index(capture),calls.index(native_event))
        self.assertLess(calls.index(native_event),calls.index(collection))
        self.assertEqual(v['collector_keeper_binding']['container_id'],'1'*64)
        self.assertEqual(json.loads(collection[6]),v['collector_keeper_binding'])
        self.assertEqual(collection[7],'1'*64)


    def test_unknown_or_false_init_rejected_before_start_and_native(self):
        for value in (None,False,'true',1):
            with self.subTest(value=value):
                _,v,calls,native,error=self.helper.run_fake_trial('go',PROFILE,alias_manifest=self.alias,task_init=value)
                self.assertIsNotNone(error);native.assert_not_called();self.assertNotIn(('start','task'),calls)
                self.assertIs(v['task_docker_init_requested'],True)
                self.assertEqual(v['task_docker_init_observation']['observed_init'],False if value is False else None)

    def test_all_old_profiles_keep_task_and_native_argv_without_init(self):
        for profile in (*eval_core.PROFILE_IDS,run_trial.FUTURE_PROFILE_ID):
            with self.subTest(profile=profile):
                _,v,calls,native,error=self.helper.run_fake_trial('go',profile,alias_manifest=self.alias if profile==run_trial.FUTURE_PROFILE_ID else None)
                self.assertIsNone(error)
                self.assertTrue(all('--init'not in call for call in calls if call[0]=='create'))
                self.assertIs(v['task_docker_init_requested'],False)
                self.assertNotIn('collector_keeper_binding',v)
                self.assertFalse(any(call[0]=='exec'and len(call)>4 and call[4]==run_trial.KEEPER_CAPTURE for call in calls))
                self.assertEqual(native.call_args.args[1],test_profiles.FIXTURE['go_argv'])

    def test_unknown_keeper_startup_rejected_before_native(self):
        _,v,calls,native,error=self.helper.run_fake_trial('go',PROFILE,alias_manifest=self.alias,task_init=True,keeper_binding=None)
        self.assertIsInstance(error,RuntimeError)
        native.assert_not_called()
        self.assertFalse(any(call[0]=='exec'and len(call)>4 and call[4]==run_trial.COLLECT for call in calls))
        self.assertEqual(v['infrastructure_error'],'RuntimeError')

    def test_public_cli_accepts_only_finite_explicit_profile(self):
        args=['run_trial.py','--task','/task','--arm','go','--out','/new','--profile',PROFILE,'--served-alias-manifest','/fixed']
        with patch('sys.argv',args),patch.object(run_trial,'run_trial')as native:run_trial.main()
        self.assertEqual(native.call_args.kwargs['profile_id'],PROFILE)
        self.assertEqual(native.call_args.kwargs['served_alias_manifest'],Path('/fixed'))
        with patch('sys.argv',args[:-4]+['--profile','init=true']),patch.object(run_trial,'run_trial')as native,contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):run_trial.main()
        native.assert_not_called()
