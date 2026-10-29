"""Standard driver opt-in contracts: all Docker/native operations are mocked."""
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import run_trial
from audit_proxy import OBSERVE_POLICY,STRICT_POLICY
from eval_core import MODEL
import test_profiles
NEW_PROFILE=test_profiles.NEW_PROFILE


class PolicyDriverContracts(unittest.TestCase):
    def alias(self,root,value=None):
        path=root/'alias.json';path.write_text(json.dumps(value if value is not None else{'aliases':{MODEL:MODEL,MODEL.split('/',1)[-1]:MODEL},'evidence_sha256':'a'*64}));path.chmod(0o600);return path
    def test_fixed_profile_requires_frozen_alias_before_output_task_or_docker(self):
        with tempfile.TemporaryDirectory()as temp:
            root=Path(temp)
            for alias in (None,self.alias(root,{'aliases':{},'evidence_sha256':'a'*64})):
                out=root/'not-created'
                with patch.object(run_trial,'docker')as docker:
                    with self.assertRaises(ValueError):run_trial.run_trial(root/'no-task','go',out,profile_id=run_trial.FUTURE_PROFILE_ID,served_alias_manifest=alias)
                    docker.assert_not_called();self.assertFalse(out.exists())
    def test_new_profile_selects_relay_policy_and_safe_alias_metadata_both_arms(self):
        helper=test_profiles.ProfileDriverContract(methodName='test_old_profile_has_no_new_environment_override_or_inspect')
        try:
            with tempfile.TemporaryDirectory()as temp:
                alias=self.alias(Path(temp))
                for arm in ('go','pi'):
                    _,result,calls,execute,error=helper.run_fake_trial(arm,run_trial.FUTURE_PROFILE_ID,alias_manifest=alias)
                    self.assertIsNone(error);self.assertEqual(result['profile_id'],run_trial.FUTURE_PROFILE_ID)
                    self.assertEqual(result['native_config_profile_id'],NEW_PROFILE)
                    self.assertEqual(result['audit_policy_id'],OBSERVE_POLICY)
                    self.assertEqual(result['native_terminal_status'],'success')
                    self.assertEqual(result['max_upstream_requests'],80);self.assertEqual(result['task_timeout_seconds'],3000)
                    relay=next(call for call in calls if call[0]=='create'and'--name'in call)
                    self.assertEqual(relay[relay.index('--policy')+1],OBSERVE_POLICY)
                    document=json.loads(relay[relay.index('--served-alias-json')+1]);self.assertEqual(document['aliases'][MODEL],MODEL)
                    self.assertNotIn(str(alias),relay)
                    self.assertEqual(execute.call_count,1)
        finally:helper.doCleanups()
    def test_default_profile_cannot_silently_opt_in_via_alias_or_host_environment(self):
        with tempfile.TemporaryDirectory()as temp:
            alias=self.alias(Path(temp))
            with self.assertRaises(ValueError),patch.object(run_trial,'docker')as docker:
                run_trial.run_trial(Path('/no-task'),'go',Path(temp)/'no-output',profile_id=NEW_PROFILE,served_alias_manifest=alias)
            docker.assert_not_called()
    def test_public_cli_has_explicit_future_profile_and_alias_path(self):
        args=['run_trial.py','--task','/task','--arm','go','--out','/new','--profile',run_trial.FUTURE_PROFILE_ID,'--served-alias-manifest','/frozen-alias']
        with patch('sys.argv',args),patch.object(run_trial,'run_trial')as execute:run_trial.main()
        self.assertEqual(execute.call_args.kwargs['profile_id'],run_trial.FUTURE_PROFILE_ID)
        self.assertEqual(execute.call_args.kwargs['served_alias_manifest'],Path('/frozen-alias'))
