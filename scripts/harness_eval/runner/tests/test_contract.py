import json
import tempfile
import time
import threading
import unittest
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from eval_core import MODEL, GO_REVISION, go_config, pi_config, cli_argv, usage_totals
from audit_proxy import AuditServer


class Contracts(unittest.TestCase):
    def test_new_main_profile_isolates_automatic_personal_memory(self):
        profile = go_config('http://relay:8000/v1')
        self.assertEqual(profile.get('memory'), {'enabled': False})
        self.assertEqual(GO_REVISION, '5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1')

    def test_configs_same_model_effort_output_native_harness(self):
        g = go_config('http://relay:8000/v1')
        p = pi_config('http://relay:8000/v1')
        self.assertEqual(g['model']['name'], MODEL)
        self.assertEqual(g['agent']['thinking'], 'high')
        self.assertNotIn('max_iterations', g['agent'])
        self.assertNotIn('compaction', g)
        self.assertNotIn('builtin', g.get('tools', {}))
        self.assertEqual(g.get('small_model', {}).get('model', ''), '')
        self.assertEqual(g['model']['max_tokens'], 16384)
        m = p['providers']['eval']['models'][0]
        self.assertEqual(m['id'], MODEL)
        self.assertEqual(m['maxTokens'], 16384)
        self.assertEqual(p['providers']['eval']['api'], 'openai-completions')

    def test_prompt_passed_unchanged_once_no_secret_argv(self):
        prompt = "Fix the bug.\nDon't add hints."
        for arm in ('go', 'pi'):
            argv = cli_argv(arm, prompt)
            self.assertEqual(argv[-1], prompt)
            self.assertEqual(argv.count(prompt), 1)
            self.assertNotIn('--api-key', argv)
        self.assertIn('--no-sandbox', cli_argv('go', prompt))
        self.assertIn('--thinking', cli_argv('pi', prompt))

    def test_usage_missing_is_unknown_and_reasoning_not_double_counted(self):
        rows = [{'phase':'finish','usage':{'prompt_tokens':100,'completion_tokens':20,
                'prompt_tokens_details':{'cached_tokens':70},
                'completion_tokens_details':{'reasoning_tokens':12}}},
                {'phase':'finish','usage':None}]
        result = usage_totals(rows)
        self.assertEqual(result['known_input_tokens'], 100)
        self.assertEqual(result['known_output_tokens'], 20)
        self.assertEqual(result['known_reasoning_tokens'], 12)
        self.assertEqual(result['known_cached_input_tokens'], 70)
        self.assertEqual(result['unknown_usage_attempts'], 1)
        self.assertIsNone(result['cost_usd'])
        self.assertEqual(result['unknown_cached_input_attempts'],1)
        self.assertEqual(result['unknown_reasoning_attempts'],1)


class Upstream(BaseHTTPRequestHandler):
    calls = []
    def log_message(self, *_): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        self.calls.append({'path':self.path,'body':body,'headers':dict(self.headers)})
        self.send_response(200)
        if body.get('stream'):
            self.send_header('Content-Type','text/event-stream');self.end_headers()
            data = {'id':'fixture-1','model':MODEL,'choices':[{'finish_reason':'stop'}],
                    'usage':{'prompt_tokens':7,'completion_tokens':3}}
            self.wfile.write(('data: '+json.dumps(data)+'\n\ndata: [DONE]\n\n').encode())
        else:
            self.send_header('Content-Type','application/json');self.end_headers()
            self.wfile.write(json.dumps({'id':'fixture-2','model':MODEL,'choices':[],
                                         'usage':{'prompt_tokens':5,'completion_tokens':2}}).encode())


class ProxyContracts(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        Upstream.calls = []
        self.up = ThreadingHTTPServer(('127.0.0.1',0),Upstream)
        threading.Thread(target=self.up.serve_forever,daemon=True).start()
        self.proxy = AuditServer(('127.0.0.1',0),f'http://127.0.0.1:{self.up.server_port}',
                                 Path(self.tmp.name)/'wire.jsonl',session='offline-test')
        threading.Thread(target=self.proxy.serve_forever,daemon=True).start()
        self.base = f'http://127.0.0.1:{self.proxy.server_port}'
    def tearDown(self):
        self.proxy.shutdown();self.proxy.server_close()
        self.up.shutdown();self.up.server_close();self.tmp.cleanup()
    def ask(self, body, path='/v1/chat/completions'):
        req=urllib.request.Request(self.base+path,json.dumps(body).encode(),
              {'Content-Type':'application/json','Authorization':'Bearer caller-secret'})
        return urllib.request.urlopen(req,timeout=5).read()
    def test_wire_keeps_native_tools_counts_all_calls_stream_and_summary(self):
        for stream in (True,False):
            body={'model':MODEL,'reasoning_effort':'high','max_tokens':16384,
                  'messages':[{'role':'user','content':'same task'}],
                  'tools':[{'type':'function','function':{'name':'subagent','parameters':{}}}],
                  'stream':stream}
            self.ask(body)
            self.assertEqual(Upstream.calls[-1]['body'],body)
        rows=[json.loads(x) for x in self.proxy.journal.read_text().splitlines()]
        finishes=[r for r in rows if r['phase']=='finish']
        self.assertEqual(len(finishes),2)
        self.assertEqual(finishes[0]['usage']['prompt_tokens'],7)
        self.assertEqual(finishes[1]['usage']['prompt_tokens'],5)
        self.assertEqual(finishes[0]['tool_names'],['subagent'])
        self.assertNotIn('caller-secret',self.proxy.journal.read_text())
        self.assertNotIn('caller-secret',str(Upstream.calls[0]['headers']))
    def test_reject_other_path_model_effort_and_output_limit(self):
        good={'model':MODEL,'reasoning_effort':'high','max_tokens':16384,'messages':[]}
        for change,path in [({},'/v1/responses'),({'model':'other'},'/v1/chat/completions'),
            ({'reasoning_effort':'max'},'/v1/chat/completions'),
            ({'max_tokens':32768},'/v1/chat/completions'),
            ({'n':2},'/v1/chat/completions'), ({'tools':[None]},'/v1/chat/completions')]:
            with self.assertRaises(urllib.error.HTTPError):self.ask(good|change,path)
        self.assertEqual(Upstream.calls,[])
        with self.assertRaises(urllib.error.HTTPError):self.ask([])
        self.assertEqual(Upstream.calls,[])
    def test_sampling_scalars_are_observed_without_injection_or_transformation(self):
        base={'model':MODEL,'reasoning_effort':'high','max_tokens':16384,'messages':[]}
        explicit={'temperature':0.25,'top_p':0.8,'seed':17,'parallel_tool_calls':False}
        self.ask(base|explicit)
        self.ask(base)
        deadline=time.monotonic()+2
        while time.monotonic()<deadline:
            rows=[json.loads(x) for x in self.proxy.journal.read_text().splitlines()]
            finishes=[r for r in rows if r['phase']=='finish']
            if len(finishes)==2:break
            time.sleep(.005)
        self.assertEqual(Upstream.calls[0]['body'],base|explicit)
        self.assertEqual(Upstream.calls[1]['body'],base)
        self.assertEqual(finishes[0].get('sampling_parameters'),explicit)
        self.assertEqual(finishes[1].get('sampling_parameters'),{})
    def test_transport_failure_preserves_attempt_with_unknown_usage(self):
        self.up.shutdown();self.up.server_close()
        with self.assertRaises(urllib.error.HTTPError):
            self.ask({'model':MODEL,'reasoning_effort':'high','max_tokens':16384,'messages':[]})
        deadline=time.monotonic()+2
        rows=[]
        while time.monotonic()<deadline:
            rows=[json.loads(x) for x in self.proxy.journal.read_text().splitlines()]
            if rows and rows[-1].get('phase')=='finish':break
            time.sleep(.005)
        self.assertEqual(rows[-1]['phase'],'finish')
        self.assertIsNone(rows[-1]['usage'])
        self.assertIn('error',rows[-1])

if __name__=='__main__':unittest.main()
