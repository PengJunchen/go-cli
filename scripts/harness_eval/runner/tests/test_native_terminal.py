"""Synthetic stdout only: safe terminal schema, no CLI process or model."""
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

from native_terminal import observe_native_terminal


class NativeTerminalContracts(unittest.TestCase):
    def observe(self,arm,rows,exit_code=0,tail=b''):
        with tempfile.TemporaryDirectory()as temp:
            path=Path(temp)/'stdout.jsonl'
            if arm=='go':rows=[dict({'content':'','timestamp':'2026-10-29T00:00:00Z'},**row)for row in rows]
            raw=b''.join((json.dumps(row)+'\n').encode()for row in rows)+tail;path.write_bytes(raw)
            result=observe_native_terminal(arm,path,exit_code)
            self.assertEqual(result['stdout_sha256'],hashlib.sha256(raw).hexdigest())
            self.assertEqual(result['stdout_size'],len(raw))
            self.assertNotIn('SENSITIVE',json.dumps(result))
            return result
    def test_go_final_typed_done_overrides_recovered_earlier_error_not_content_words(self):
        rows=[{'kind':'error','content':'SENSITIVE old error'},{'kind':'tool_result','is_error':True,'content':'error SENSITIVE'},{'kind':'done','content':'SENSITIVE error api_error'}]
        result=self.observe('go',rows);self.assertEqual(result['status'],'success');self.assertTrue(result['recovered_prior_errors'])
    def test_go_terminal_error_nonzero_and_unknown_preserved(self):
        self.assertEqual(self.observe('go',[{'kind':'error','content':'SENSITIVE'}])['status'],'runtime_error')
        self.assertEqual(self.observe('go',[{'kind':'done','content':'SENSITIVE'}],1)['status'],'runtime_error')
        self.assertEqual(self.observe('go',[{'kind':'message','content':'done SENSITIVE'}])['status'],'unknown')
        self.assertEqual(self.observe('go',[{'kind':'done'}],tail=b'{broken')['status'],'unknown')
    def test_pi_final_settled_recovery_is_success_even_with_prior_error(self):
        result=self.observe('pi',[{'type':'session','cwd':'SENSITIVE','id':'SENSITIVE'},{'type':'agent_end','willRetry':True,'messages':[{'role':'assistant','stopReason':'error','errorMessage':'SENSITIVE'}]},{'type':'agent_end','willRetry':False,'messages':[{'role':'assistant','stopReason':'stop','content':[{'text':'SENSITIVE'}]}]},{'type':'agent_settled','aborted':False}])
        self.assertEqual(result['status'],'success');self.assertTrue(result['recovered_prior_errors'])
    def test_pi_cli_zero_final_error_length_abort_are_not_success(self):
        for reason,status in [('error','api_error'),('length','output_truncated'),('aborted','aborted')]:
            with self.subTest(reason=reason):
                result=self.observe('pi',[{'type':'agent_end','willRetry':False,'messages':[{'role':'assistant','stopReason':reason,'errorMessage':'SENSITIVE'}]},{'type':'agent_settled','aborted':reason=='aborted'}])
                self.assertEqual(result['status'],status)
    def test_pi_retry_pending_missing_settled_unknown_schema_and_order_stay_unknown(self):
        good={'type':'agent_end','willRetry':False,'messages':[{'role':'assistant','stopReason':'stop'}]}
        for rows in ([good],[dict(good,willRetry=True),{'type':'agent_settled','aborted':False}],[{'type':'agent_settled','aborted':False},good],[{'type':'message_end','message':{'role':'assistant','stopReason':'stop'}},{'type':'agent_settled','aborted':False}],[dict(good,messages=[{'role':'assistant','stopReason':'unknown'}]),{'type':'agent_settled','aborted':False}]):
            with self.subTest(rows=rows):self.assertEqual(self.observe('pi',rows)['status'],'unknown')
    def test_pi_uses_last_assistant_and_final_end_not_any_earlier_success(self):
        success={'type':'agent_end','willRetry':False,'messages':[{'role':'assistant','stopReason':'stop'}]}
        error={'type':'agent_end','willRetry':False,'messages':[{'role':'assistant','stopReason':'stop'},{'role':'assistant','stopReason':'error','errorMessage':'SENSITIVE'},{'role':'toolResult','content':'stop'}]}
        self.assertEqual(self.observe('pi',[success,error,{'type':'agent_settled','aborted':False}])['status'],'api_error')
