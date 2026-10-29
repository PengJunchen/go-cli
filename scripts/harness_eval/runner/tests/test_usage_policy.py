"""Loopback-only policy contracts; no model, Docker or native CLI."""
import concurrent.futures
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
import unittest
import urllib.error
import urllib.request
from unittest.mock import patch

from audit_proxy import AuditServer,STRICT_POLICY,OBSERVE_POLICY,ledger_observations
from eval_core import MODEL,OUTPUT_LIMIT
from run_trial import final_record
import test_summarize
from summarize_trials import summarize

ALIASES={MODEL:MODEL,MODEL.split('/',1)[-1]:MODEL,'known-other':'other-canonical-model'}
EVIDENCE='a'*64
GOOD={'id':'synthetic-response','model':MODEL,'choices':[{'finish_reason':'stop','message':{'role':'assistant','content':'ok'}}], 'usage':{'prompt_tokens':7,'completion_tokens':3,'prompt_tokens_details':{'cached_tokens':2},'completion_tokens_details':{'reasoning_tokens':1}}}

class Upstream(BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def do_POST(self):
        size=int(self.headers['Content-Length']);body=self.rfile.read(size)
        with self.server.lock:
            self.server.requests.append(json.loads(body));plan=self.server.plans.pop(0)if self.server.plans else (200,'application/json',json.dumps(GOOD).encode())
        if self.server.before_response:self.server.before_response()
        status,kind,data=plan;self.send_response(status);self.send_header('Content-Type',kind)
        self.send_header('Connection','close');self.end_headers();self.wfile.write(data)

class PolicyContracts(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.up=ThreadingHTTPServer(('127.0.0.1',0),Upstream);self.up.requests=[];self.up.plans=[];self.up.lock=threading.Lock();self.up.before_response=None
        threading.Thread(target=self.up.serve_forever,daemon=True).start();self.addCleanup(self.up.server_close);self.addCleanup(self.up.shutdown)
        self.proxies=[]
    def proxy(self,policy=OBSERVE_POLICY):
        kw={}if policy==STRICT_POLICY else {'policy_id':policy,'served_model_aliases':ALIASES,'served_model_evidence_sha256':EVIDENCE}
        p=AuditServer(('127.0.0.1',0),f'http://127.0.0.1:{self.up.server_port}',Path(self.temp.name)/('wire'+str(len(self.proxies))+'.jsonl'),'synthetic',**kw)
        threading.Thread(target=p.serve_forever,daemon=True).start();self.addCleanup(p.server_close);self.addCleanup(p.shutdown);self.proxies.append(p);return p
    def ask(self,p,stream=False,*,path='/v1/chat/completions',**changes):
        body={'model':MODEL,'reasoning_effort':'high','max_tokens':OUTPUT_LIMIT,'messages':[],'stream':stream}|changes
        req=urllib.request.Request(f'http://127.0.0.1:{p.server_port}'+path,json.dumps(body).encode(),{'Content-Type':'application/json'})
        try:
            with urllib.request.urlopen(req,timeout=5)as response:return response.status,response.read()
        except urllib.error.HTTPError as error:return error.code,error.read()
    def rows(self,p,n):
        end=time.monotonic()+2
        while time.monotonic()<end:
            if p.journal.exists():
                rows=[json.loads(x)for x in p.journal.read_text().splitlines()]
                if len([r for r in rows if r['phase']=='finish'])>=n:return rows
            time.sleep(.005)
        self.fail('missing expected finish rows')
    def test_default_strict_still_stops_after_unknown_but_observe_permits_native_requests(self):
        for policy,expected in ((STRICT_POLICY,429),(OBSERVE_POLICY,200)):
            p=self.proxy(policy);self.up.plans=[(502,'application/json',b'{"error":{"message":"SENSITIVE_ERROR_SENTINEL"}}')]
            before=len(self.up.requests);self.assertEqual(self.ask(p)[0],502);self.rows(p,1)
            self.assertEqual(self.ask(p)[0],expected)
            self.assertEqual(len(self.up.requests)-before,1 if policy==STRICT_POLICY else 2)
            self.assertNotIn('SENSITIVE_ERROR_SENTINEL',p.journal.read_text())
    def test_three_client_retries_take_slots_49_to_51_no_relay_retry_or_model_change(self):
        p=self.proxy();p.seq=47;self.up.plans=[(502,'application/json',b'{}')]
        self.assertEqual(self.ask(p)[0],502)
        for _ in range(3):self.assertEqual(self.ask(p)[0],200)
        rows=self.rows(p,4);self.assertEqual([r['sequence']for r in rows if r['phase']=='start'],[48,49,50,51])
        self.assertEqual(len(self.up.requests),4)
        self.assertTrue(all(r['model']==MODEL and r['reasoning_effort']=='high'for r in self.up.requests))
    def test_valid_200_without_usage_preserves_measurement_unknown_not_a_stop(self):
        p=self.proxy();value=copy.deepcopy(GOOD);value.pop('usage');self.up.plans=[(200,'application/json',json.dumps(value).encode())]
        self.assertEqual(self.ask(p)[0],200);self.assertEqual(self.ask(p)[0],200)
        observed=ledger_observations(self.rows(p,2))
        self.assertEqual(observed['ledger_status'],'complete');self.assertEqual(observed['n_usage_complete'],1)
        self.assertIsNone(observed['exact_total_tokens']);self.assertEqual(observed['known_total_tokens_lower_bound'],10)
        self.assertIsNone(observed['cost_usd'])
    def test_stream_terminal_evidence_and_forwarded_bytes_are_separate(self):
        p=self.proxy();json_data=json.dumps(GOOD).encode();sse=(b'data: '+json_data+b'\n\ndata: [DONE]\n\n');partial=b'data: {"choices":[{"delta":{"content":"SENSITIVE_PARTIAL"}}]}\n\n'
        for stream,ctype,data in [(False,'application/json',json_data),(True,'text/event-stream',sse),(True,'text/event-stream',partial)]:
            self.up.plans=[(200,ctype,data)];self.assertEqual(self.ask(p,stream)[1],data)
        rows=self.rows(p,3);ends=[r for r in rows if r['phase']=='finish']
        self.assertTrue(ends[0]['json_parse_complete']);self.assertFalse(ends[0]['sse_done_seen'])
        self.assertTrue(ends[1]['sse_done_seen']);self.assertFalse(ends[1]['json_parse_complete'])
        self.assertFalse(ends[2]['sse_done_seen']);self.assertEqual(ends[2]['finish_reasons'],[])
        self.assertTrue(all(r['transport_eof']for r in ends));self.assertNotIn('SENSITIVE_PARTIAL',p.journal.read_text())
        self.assertEqual(self.ask(p)[0],200)
    def test_start_slot_persisted_before_upstream_and_unknown_send_not_refunded(self):
        p=self.proxy();events=[]
        original=p._write_locked
        def write(row):original(row);events.append('durable-'+row['phase'])
        p._write_locked=write;self.up.before_response=lambda:events.append('upstream')
        self.ask(p);self.rows(p,1);self.assertLess(events.index('durable-start'),events.index('upstream'))
        p2=AuditServer(('127.0.0.1',0),'http://127.0.0.1:1',Path(self.temp.name)/'failed.jsonl','synthetic',policy_id=OBSERVE_POLICY,served_model_aliases=ALIASES,served_model_evidence_sha256=EVIDENCE)
        threading.Thread(target=p2.serve_forever,daemon=True).start();self.addCleanup(p2.server_close);self.addCleanup(p2.shutdown)
        self.assertEqual(self.ask(p2)[0],502);rows=self.rows(p2,1)
        self.assertEqual(p2.seq,1);self.assertEqual(ledger_observations(rows)['n_unknown_dispatch'],1)
    def test_persistence_failure_blocks_before_upstream_and_is_not_unknown_usage(self):
        p=self.proxy();p._write_locked=lambda row:(_ for _ in ()).throw(OSError('synthetic disk'))
        self.assertEqual(self.ask(p)[0],503);self.assertEqual(self.up.requests,[])
        self.assertEqual(p.control_stop_reason,'journal_persistence_error')
    def test_native_concurrency_80_slots_and_wall_backstop_do_not_reset(self):
        p=self.proxy();p.seq=78
        with concurrent.futures.ThreadPoolExecutor(max_workers=4)as pool:statuses=list(pool.map(lambda _:self.ask(p)[0],range(4)))
        self.assertEqual(statuses.count(200),2);self.assertEqual(statuses.count(429),2);self.assertEqual(len(self.up.requests),2)
        self.assertEqual(p.seq,80)
        p=self.proxy()
        with patch('audit_proxy.time.monotonic',return_value=10):self.assertEqual(self.ask(p)[0],200)
        self.rows(p,1)
        with patch('audit_proxy.time.monotonic',return_value=3011):self.assertEqual(self.ask(p)[0],429)
        self.assertEqual(p.seq,1)
    def test_observe_wrong_post_path_stops_later_valid_calls_without_logging_raw_path(self):
        p=self.proxy();sentinel='/v1/private-SENSITIVE_PATH_SENTINEL'
        self.assertEqual(self.ask(p,path=sentinel)[0],403)
        self.assertEqual(self.ask(p)[0],503)
        self.assertEqual(self.up.requests,[])
        rows=[json.loads(line)for line in p.journal.read_text().splitlines()]
        self.assertEqual(rows[0]['origin'],'relay_contract')
        self.assertEqual(rows[0]['reason'],'unsupported_chat_path')
        self.assertNotIn(sentinel,p.journal.read_text())
        strict=self.proxy(STRICT_POLICY)
        self.assertEqual(self.ask(strict,path=sentinel)[0],403)
        self.assertEqual(self.ask(strict)[0],200)

    def test_known_output_over_actual_native_cap_stops_but_missing_usage_stays_unknown(self):
        for cap in (16384,1024):
            with self.subTest(cap=cap):
                p=self.proxy();value=copy.deepcopy(GOOD);value['usage']['completion_tokens']=cap+1
                self.up.plans=[(200,'application/json',json.dumps(value).encode())]
                self.assertEqual(self.ask(p,max_tokens=cap)[0],200)
                self.rows(p,1)
                self.assertEqual(p.control_stop_reason,'provider_output_limit_violation')
                self.assertEqual(self.ask(p,max_tokens=cap)[0],503)
        p=self.proxy();value=copy.deepcopy(GOOD);value['usage']={'prompt_tokens':7,'total_tokens':999999}
        self.up.plans=[(200,'application/json',json.dumps(value).encode())]
        self.ask(p,max_tokens=1024);rows=self.rows(p,1)
        self.assertIsNone(p.control_stop_reason);self.assertEqual(self.ask(p,max_tokens=1024)[0],200)
        self.assertIsNone(ledger_observations(rows)['exact_total_tokens'])
        p=self.proxy();before=len(self.up.requests)
        self.assertEqual(self.ask(p,max_tokens=1024,max_completion_tokens=2048)[0],422)
        self.assertEqual(len(self.up.requests),before)

    def test_auth_and_request_contract_stops_have_distinct_origins(self):
        p=self.proxy();self.up.plans=[(401,'application/json',b'{"error":{"message":"secret"}}')]
        self.assertEqual(self.ask(p)[0],401);self.rows(p,1);self.assertEqual(self.ask(p)[0],503)
        self.assertEqual(p.control_stop_reason,'api_access_error')
        p=self.proxy();before=len(self.up.requests);self.assertEqual(self.ask(p,model='other')[0],422)
        self.assertEqual(len(self.up.requests),before);self.assertEqual(p.control_stop_reason,'request_contract_error')
    def test_response_contract_error_and_200_error_envelope_are_not_usage_only(self):
        for value in ({'choices':'invalid'}, {'error':{'message':'SENSITIVE_ERROR_BODY'}}):
            p=self.proxy();self.up.plans=[(200,'application/json',json.dumps(value).encode())]
            self.ask(p);self.rows(p,1)
            self.assertEqual(self.ask(p)[0],503)
            self.assertNotIn('SENSITIVE_ERROR_BODY',p.journal.read_text())

    def test_restart_cannot_reuse_journal_and_frozen_alias_map_is_not_mutable(self):
        p=self.proxy();self.ask(p);self.rows(p,1)
        with self.assertRaises(ValueError):AuditServer(('127.0.0.1',0),f'http://127.0.0.1:{self.up.server_port}',p.journal,'synthetic',policy_id=OBSERVE_POLICY,served_model_aliases=ALIASES,served_model_evidence_sha256=EVIDENCE)
        with self.assertRaises(TypeError):p.served_model_aliases['new']=MODEL

    def test_aliases_must_be_frozen_known_mismatch_stops_unknown_alias_does_not(self):
        with self.assertRaises(ValueError):AuditServer(('127.0.0.1',0),'http://127.0.0.1:1',Path(self.temp.name)/'x','x',policy_id=OBSERVE_POLICY)
        for alias,blocked in ((MODEL.split('/',1)[-1],False),('unknown-served-alias',False),('known-other',True)):
            p=self.proxy();value=copy.deepcopy(GOOD);value['model']=alias;self.up.plans=[(200,'application/json',json.dumps(value).encode())]
            self.ask(p);ends=[r for r in self.rows(p,1)if r['phase']=='finish']
            self.assertEqual(self.ask(p)[0],503 if blocked else 200)
            self.assertEqual(ends[0]['served_model_provenance'],'unknown'if alias=='unknown-served-alias'else'known')

class LedgerPolicyContracts(unittest.TestCase):
    def test_orphan_missing_finish_partial_fields_and_detail_subsets_are_not_zero_or_double_counted(self):
        rows=[{'phase':'start','sequence':1},{'phase':'finish','sequence':1,'usage':{'prompt_tokens':8,'completion_tokens':2,'prompt_tokens_details':{'cached_tokens':7},'completion_tokens_details':{'reasoning_tokens':1}}}]
        result=ledger_observations(rows);self.assertEqual(result['exact_total_tokens'],10)
        self.assertEqual(result['known_total_tokens_lower_bound'],10)
        rows += [{'phase':'start','sequence':2},{'phase':'finish','sequence':2,'usage':{'prompt_tokens':4,'completion_tokens':None}}]
        result=ledger_observations(rows);self.assertIsNone(result['exact_total_tokens']);self.assertEqual(result['known_total_tokens_lower_bound'],14)
        self.assertEqual(result['n_output_unknown'],1)
        for malformed in ([{'phase':'finish','sequence':1,'usage':GOOD['usage']}],[{'phase':'start','sequence':1}],rows+[{'phase':'finish','sequence':9,'usage':GOOD['usage']}]):
            self.assertNotEqual(ledger_observations(malformed)['ledger_status'],'complete')
        value={'prompt_tokens':True,'completion_tokens':-1,'prompt_tokens_details':{'cached_tokens':8}}
        result=ledger_observations([{'phase':'start','sequence':1},{'phase':'finish','sequence':1,'usage':value}])
        self.assertIsNone(result['exact_total_tokens']);self.assertIsNone(result['cost_usd'])
        result=ledger_observations([{'phase':'start','sequence':1},{'phase':'finish','sequence':1,'http_status':200,'usage':{'prompt_tokens':8,'completion_tokens':2,'prompt_tokens_details':{'cached_tokens':9},'completion_tokens_details':{'reasoning_tokens':True}}}])
        self.assertEqual(result['exact_total_tokens'],10);self.assertEqual(result['n_cached_known'],0);self.assertEqual(result['n_reasoning_known'],0)
    def test_last_wire_error_is_not_silently_equated_with_native_terminal_error(self):
        row={'phase':'finish','sequence':1,'policy_id':OBSERVE_POLICY,'served_alias_fingerprint':'a'*64,
             'http_status':502,'usage':None,'transport_eof':True,'json_parse_complete':True}
        rows=[{'phase':'start','sequence':1,'policy_id':OBSERVE_POLICY,'served_alias_fingerprint':'a'*64},row]
        unknown=final_record({'exit_code':0},rows)
        self.assertFalse(unknown['api_terminal_error']);self.assertTrue(unknown['api_completion_unknown'])
        self.assertFalse(final_record({'exit_code':1},rows)['api_completion_unknown'])
        self.assertTrue(final_record({'exit_code':0,'native_terminal_status':'api_error'},rows)['api_terminal_error'])
        self.assertFalse(final_record({'exit_code':0,'native_terminal_status':'success'},rows)['api_completion_unknown'])

    def test_distinct_policy_fingerprints_never_merge_and_partial_known_input_is_kept(self):
        helper=test_summarize.SummaryContracts(methodName='test_success_failure_and_walls_are_separate_without_ranking');helper.setUp()
        try:
            first=helper.trial(audit_policy_id=OBSERVE_POLICY,audit_policy_fingerprint='a')
            second=helper.trial(audit_policy_id=OBSERVE_POLICY,audit_policy_fingerprint='b')
            self.assertEqual(len(summarize([first,second])['groups']),2)
            row=helper.trial(usage={'ledger_complete':True,'attempts':1,'unknown_usage_attempts':1,'unknown_input_attempts':0,'unknown_output_attempts':1,'known_input_tokens':5,'known_output_tokens':0})
            usage=summarize([row])['trials'][0]['usage']
            self.assertEqual(usage['input_tokens'],5);self.assertIsNone(usage['output_tokens']);self.assertIsNone(usage['total_tokens'])
        finally:helper.doCleanups()

    def test_cli_zero_api_terminal_is_not_delivery_but_usage_only_unknown_can_deliver(self):
        helper=test_summarize.SummaryContracts(methodName='test_success_failure_and_walls_are_separate_without_ranking');helper.setUp()
        try:
            a=helper.trial(exit_code=0,api_terminal_error=True)
            row=summarize([a])['trials'][0]
            self.assertEqual(row['official_score'],1);self.assertEqual(row['delivery_score'],0)
            runtime=helper.trial(exit_code=1,api_completion_unknown=True)
            self.assertEqual(summarize([runtime])['trials'][0]['delivery_score'],0)
            b=helper.trial(usage={'attempts':1,'unknown_usage_attempts':1,'known_input_tokens':0,'known_output_tokens':0})
            self.assertEqual(summarize([b])['trials'][0]['delivery_score'],1)
        finally:helper.doCleanups()
