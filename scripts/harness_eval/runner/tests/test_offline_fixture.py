import json
import threading
import unittest
from unittest.mock import patch
from urllib.request import Request, urlopen
from urllib.error import HTTPError
from offline_fixture import server

class FixtureContract(unittest.TestCase):
    def test_write_then_parallel_read_git_then_followup(self):
        with patch.dict('os.environ', {'EVAL_FIXTURE_SCENARIO':'write-parallel'}):
            fixture=server();worker=threading.Thread(target=fixture.serve_forever);worker.start()
            try:
                tools=[{'type':'function','function':{'name':n,'parameters':{'properties':{'path':{},'content':{}}}}} for n in ['read','write','bash','git_status']]
                def ask(messages):
                    req=Request(f'http://127.0.0.1:{fixture.server_port}/chat/completions',data=json.dumps({'messages':messages,'tools':tools}).encode(),headers={'Content-Type':'application/json'})
                    with urlopen(req) as response:return json.load(response)['choices'][0]['message']
                first=ask([])
                self.assertEqual([x['function']['name'] for x in first['tool_calls']],['write'])
                args=json.loads(first['tool_calls'][0]['function']['arguments'])
                self.assertEqual(args,{'path':'/app/.eval-fixture.txt','content':'fixture-canary-5736e88\n'})
                second=ask([{'role':'tool','tool_call_id':'fixture-write-1','content':'ok'}])
                self.assertEqual([x['function']['name'] for x in second['tool_calls']],['read','bash','git_status','read'])
                self.assertEqual(json.loads(second['tool_calls'][0]['function']['arguments'])['path'],'/app/.eval-fixture.txt')
                final=ask([{'role':'tool','tool_call_id':'fixture-write-1','content':'ok'},{'role':'tool','tool_call_id':'fixture-git-status-1','content':'ok'}])
                self.assertEqual(final['content'],'OFFLINE_FIXTURE_DONE')
            finally:
                fixture.shutdown();fixture.server_close();worker.join()


class FaultFixtureContract(unittest.TestCase):
    def test_finite_recovery_and_exhaustion_scenarios_are_loopback_and_no_model(self):
        for scenario,expected in [('recover-once',[502,200,200]),('fail-always',[502,502,502])]:
            with self.subTest(scenario=scenario):
                fixture=server(scenario);worker=threading.Thread(target=fixture.serve_forever);worker.start()
                try:
                    statuses=[]
                    for _ in range(3):
                        req=Request(f'http://127.0.0.1:{fixture.server_port}/v1/chat/completions',data=b'{"messages":[],"tools":[]}',headers={'Content-Type':'application/json'})
                        try:
                            with urlopen(req,timeout=5)as response:statuses.append(response.status)
                        except HTTPError as error:
                            statuses.append(error.code);self.assertNotIn('usage',json.loads(error.read()))
                    self.assertEqual(statuses,expected);self.assertEqual(fixture.request_count,3)
                finally:fixture.shutdown();fixture.server_close();worker.join()
