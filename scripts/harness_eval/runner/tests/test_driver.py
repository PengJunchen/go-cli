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


class ProcessSnapshotContract(unittest.TestCase):
    @staticmethod
    def stat_line(pid, comm='worker name'):
        fields=['0']*52
        for number,value in {1:str(pid),2:'('+comm+')',3:'S',4:'2',5:'7',6:'3',22:'123456'}.items():
            fields[number-1]=value
        return ' '.join(fields)

    def test_proc_snapshot_preserves_identity_without_sensitive_fields(self):
        from run_trial import snapshot_processes
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary)
            for pid in [1,7,55]:
                (root/str(pid)).mkdir();(root/str(pid)/'stat').write_text(self.stat_line(pid,'name with ) spaces'))
            (root/'self').mkdir()
            result=snapshot_processes(str(root),[1,55])
        self.assertEqual(result['enumeration_status'],'observed')
        self.assertEqual(result['observed_process_count'],1)
        self.assertEqual(result['unknown_process_count'],0)
        self.assertEqual(result['processes'],[{'pid':7,'status':'observed','comm':'name with ) spaces','state':'S','ppid':2,'pgid':7,'starttime':123456}])
        self.assertNotIn('cmdline',json.dumps(result));self.assertNotIn('environ',json.dumps(result))

    def test_proc_snapshot_does_not_turn_unknowns_into_empty_processes(self):
        import builtins
        from run_trial import snapshot_processes
        real_open=builtins.open
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary)
            for pid in [7,8,9]:(root/str(pid)).mkdir()
            (root/'8/stat').write_text('malformed')
            (root/'9/stat').write_text(self.stat_line(9))
            def read(path,*args,**kwargs):
                if str(path).endswith('/9/stat'):raise PermissionError('fixture')
                return real_open(path,*args,**kwargs)
            with patch('builtins.open',side_effect=read):result=snapshot_processes(str(root),[1,55])
        self.assertEqual(result['observed_process_count'],0)
        self.assertEqual(result['unknown_process_count'],3)
        self.assertEqual([(r['pid'],r['status'],r['reason']) for r in result['processes']],[(7,'unknown','process_disappeared'),(8,'unknown','malformed_proc_stat'),(9,'unknown','permission_denied')])

    def test_proc_enumeration_failure_is_unknown_not_zero(self):
        from run_trial import snapshot_processes
        with patch('run_trial.os.listdir',side_effect=PermissionError('fixture')):
            result=snapshot_processes('/fake-proc',[1,55])
        self.assertEqual(result['enumeration_status'],'unknown')
        self.assertEqual(result['enumeration_error'],'PermissionError')
        self.assertIsNone(result['processes'])
        self.assertIsNone(result['observed_process_count'])
        self.assertIsNone(result['unknown_process_count'])

if __name__=='__main__':unittest.main()
