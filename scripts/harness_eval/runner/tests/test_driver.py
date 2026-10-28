import tempfile
import json
import hashlib
import unittest
from pathlib import Path
from unittest.mock import patch
from run_trial import run_cli_once, final_record, load_payload_manifest


class FakeProcess:
    returncode=0
    def wait(self,timeout=None):return 0


class DriverContract(unittest.TestCase):
    def test_single_invocation_timeout_is_container_side_and_logs_preserved(self):
        with tempfile.TemporaryDirectory() as temp,patch('run_trial.subprocess.Popen',return_value=FakeProcess()) as call:
            result=run_cli_once('owned-container',['cli','unchanged prompt'],Path(temp),3000)
            args=call.call_args.args[0]
            self.assertEqual(args[:4],['docker','exec','owned-container','timeout'])
            self.assertIn('3000s',args)
            self.assertEqual(args[-1],'unchanged prompt')
            self.assertEqual(call.call_count,1)
            self.assertEqual(result['exit_code'],0)
            self.assertTrue((Path(temp)/'stdout.jsonl').exists())
    def test_binary_identity_must_match_declared_source_manifest(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);(root/'go-cli').write_bytes(b'fixed binary')
            metadata={'go_cli_source_revision':'a'*40,
                      'go_cli_sha256':hashlib.sha256(b'fixed binary').hexdigest()}
            (root/'manifest.json').write_text(json.dumps(metadata))
            self.assertEqual(load_payload_manifest(root),metadata)
            (root/'go-cli').write_bytes(b'different binary')
            with self.assertRaises(ValueError):load_payload_manifest(root)

    def test_open_request_is_counted_as_unknown(self):
        result=final_record({'exit_code':124},[{'phase':'start','sequence':1}])
        self.assertEqual(result['usage']['attempts'],1)
        self.assertEqual(result['usage']['unknown_usage_attempts'],1)
        self.assertEqual(result['open_attempts'],1)

    def test_exit_zero_does_not_assert_success(self):
        result=final_record({'exit_code':0},[])
        self.assertIsNone(result['task_passed'])
        self.assertEqual(result['usage']['attempts'],0)
        self.assertIsNone(result['usage']['cost_usd'])

if __name__=='__main__':unittest.main()
