"""Fixed-destination chat relay and append-only metadata journal. No CONNECT."""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import threading
import time
import urllib.parse
from types import MappingProxyType
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from eval_core import MODEL,OUTPUT_LIMIT


def digest(value):
    return hashlib.sha256(json.dumps(value,sort_keys=True,ensure_ascii=False).encode()).hexdigest()


def command_only_schema(parameters):
    if not isinstance(parameters,dict) or parameters.get('type')!='object':return None
    properties=parameters.get('properties')
    if not isinstance(properties,dict):return None
    if set(properties)!={'command'}:return False
    command=properties['command']
    if not isinstance(command,dict) or 'type' not in command:return None
    if command['type']!='string':return False
    required=parameters.get('required')
    if not isinstance(required,list) or any(not isinstance(name,str) for name in required):return None
    if required!=['command']:return False
    if 'additionalProperties' not in parameters:return None
    additional=parameters['additionalProperties']
    if additional is False:return True
    if additional is True or isinstance(additional,dict):return False
    return None


def observe_tool_metadata(tools):
    """Observe actual tool bytes as hashes/finite claims, never raw content."""
    if not isinstance(tools,list):return []
    observed=[]
    for index,tool in enumerate(tools):
        function=tool.get('function') if isinstance(tool,dict) else None
        function=function if isinstance(function,dict) else {}
        name=function.get('name') if isinstance(function.get('name'),str) else None
        description=function.get('description')
        parameters=function.get('parameters')
        description_hash=parameters_hash=None
        try:
            if isinstance(description,str):description_hash=hashlib.sha256(description.encode('utf-8')).hexdigest()
        except UnicodeError:pass
        try:
            if isinstance(parameters,dict):
                canonical=json.dumps(parameters,sort_keys=True,separators=(',',':'),ensure_ascii=False,allow_nan=False).encode('utf-8')
                parameters_hash=hashlib.sha256(canonical).hexdigest()
        except (TypeError,ValueError,UnicodeError):pass
        claim=None
        if name=='bash' and description_hash is not None:
            clauses=set(re.findall(r'The configured host execution window is ([^;\r\n]+);',description))
            if len(clauses)==1 and next(iter(clauses)) in ('30s','5m0s'):claim=next(iter(clauses))
        observed.append({'index':index,'name':name,'description_sha256':description_hash,
                         'parameters_sha256':parameters_hash,'go_host_window_claim':claim,
                         'command_only_schema':command_only_schema(parameters) if name=='bash' and parameters_hash is not None else None})
    return observed


STRICT_POLICY='strict-unknown-usage-stop-v1'
OBSERVE_POLICY='usage-observe-native-retry-v1'


def normalized_usage(value):
    if not isinstance(value,dict):return None
    result={}
    for name in ('prompt_tokens','completion_tokens'):
        number=value.get(name)
        if type(number)is int and number>=0:result[name]=number
    for field,key,total in [('prompt_tokens_details','cached_tokens','prompt_tokens'),('completion_tokens_details','reasoning_tokens','completion_tokens')]:
        detail=value.get(field)
        number=detail.get(key)if isinstance(detail,dict)else None
        if type(number)is int and number>=0 and total in result and number<=result[total]:result[field]={key:number}
    return result


def ledger_observations(rows):
    starts={};finishes={};invalid=False
    for row in rows:
        if row.get('phase')not in ('start','finish'):continue
        seq=row.get('sequence')
        if type(seq)is not int or not 1<=seq<=80:invalid=True;continue
        bucket=starts if row['phase']=='start'else finishes
        if seq in bucket:invalid=True;bucket[seq]=None
        else:bucket[seq]=row
    maximum=max([*starts,*finishes],default=0);slots=list(range(1,maximum+1))
    complete=bool(slots)and not invalid and set(starts)==set(finishes)==set(slots)and all(starts[n]is not None and finishes[n]is not None for n in slots)
    sums={'input':0,'output':0,'cached':0,'reasoning':0};known={k:0 for k in sums};usage_complete=0;dispatch_unknown=0
    for seq in slots:
        finish=finishes.get(seq)if starts.get(seq)is not None else None
        if finish is None or type(finish.get('http_status'))is not int:dispatch_unknown+=1
        usage=normalized_usage(finish.get('usage'))if finish is not None else None
        usage=usage or {}
        if 'prompt_tokens'in usage and 'completion_tokens'in usage:usage_complete+=1
        for kind,field in [('input','prompt_tokens'),('output','completion_tokens')]:
            if field in usage:sums[kind]+=usage[field];known[kind]+=1
        for kind,field,key in [('cached','prompt_tokens_details','cached_tokens'),('reasoning','completion_tokens_details','reasoning_tokens')]:
            if field in usage:sums[kind]+=usage[field][key];known[kind]+=1
    usage={'ledger_complete':complete,'attempts':len(slots),'known_input_tokens':sums['input'],'known_output_tokens':sums['output'],
           'known_cached_input_tokens':sums['cached'],'known_reasoning_tokens':sums['reasoning'],
           'unknown_usage_attempts':len(slots)-usage_complete,'unknown_input_attempts':len(slots)-known['input'],
           'unknown_output_attempts':len(slots)-known['output'],'unknown_cached_input_attempts':len(slots)-known['cached'],
           'unknown_reasoning_attempts':len(slots)-known['reasoning'],'cost_usd':None}
    return {'ledger_status':'complete'if complete else'invalid'if invalid else'incomplete',
            'ledger_reason':'complete'if complete else'invalid_event_or_duplicate'if invalid else'missing_start_finish_or_prefix',
            'n_orphan_finishes':len(set(finishes)-set(starts)),'n_open_starts':len(set(starts)-set(finishes)),
            'n_http_status_known':sum(type(row.get('http_status'))is int for seq,row in finishes.items()if row is not None and starts.get(seq)is not None),
            'n_forward_slots':len(slots),'n_start':len(starts),'n_finish':len(finishes),
            'n_unknown_dispatch':dispatch_unknown,'n_usage_complete':usage_complete,
            'n_input_known':known['input'],'n_output_known':known['output'],
            'n_input_unknown':len(slots)-known['input'],'n_output_unknown':len(slots)-known['output'],
            'n_cached_known':known['cached'],'n_reasoning_known':known['reasoning'],
            'exact_total_tokens':sums['input']+sums['output']if complete and known['input']==known['output']==len(slots)else None,
            'known_total_tokens_lower_bound':sums['input']+sums['output'],'usage':usage,'cost_usd':None}


def resolve_audit_policy(policy_id,document=None):
    if policy_id==STRICT_POLICY:
        if document is not None:raise ValueError('strict policy does not accept an alias override')
        aliases={};evidence=None
    elif policy_id==OBSERVE_POLICY:
        if not isinstance(document,dict)or set(document)!={'aliases','evidence_sha256'}:raise ValueError('observe policy needs a closed frozen alias document')
        aliases=document['aliases'];evidence=document['evidence_sha256']
        if (not isinstance(aliases,dict)or not aliases or aliases.get(MODEL)!=MODEL
                or any(not isinstance(k,str)or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}',k)
                       or not isinstance(v,str)or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}',v)for k,v in aliases.items())
                or not isinstance(evidence,str)or not re.fullmatch('[0-9a-f]{64}',evidence)):
            raise ValueError('invalid frozen served-alias map or evidence SHA')
        aliases=dict(aliases)
    else:raise ValueError('unknown fixed audit policy')
    return {'policy_id':policy_id,'aliases':aliases,'evidence_sha256':evidence,
            'fingerprint':digest({'aliases':aliases,'evidence_sha256':evidence})}


class AuditServer(ThreadingHTTPServer):
    daemon_threads=True
    def __init__(self,address,upstream,journal,session,*,policy_id=STRICT_POLICY,served_model_aliases=None,served_model_evidence_sha256=None):
        document=None if served_model_aliases is None and served_model_evidence_sha256 is None else {'aliases':served_model_aliases,'evidence_sha256':served_model_evidence_sha256}
        selected=resolve_audit_policy(policy_id,document)
        aliases=selected['aliases']
        if policy_id==OBSERVE_POLICY and Path(journal).exists():raise ValueError('observe policy requires a new journal; restart cannot reset reserved slots')
        self.upstream=urllib.parse.urlsplit(upstream)
        if self.upstream.scheme not in ('http','https') or self.upstream.username or self.upstream.password or self.upstream.path not in ('','/') or self.upstream.query or self.upstream.fragment:
            raise ValueError('invalid fixed upstream')
        self.journal=Path(journal);self.journal.parent.mkdir(parents=True,exist_ok=True)
        self.session=session;self.lock=threading.Lock();self.seq=0
        self.max_requests=80;self.unknown_usage_seen=False;self.control_stop_reason=None
        self.policy_id=policy_id;self.served_model_aliases=MappingProxyType(aliases)
        self.alias_fingerprint=selected['fingerprint']
        self.first_forward_monotonic=None
        super().__init__(address,Handler)
    def _write_locked(self,row):
        fd=os.open(self.journal,os.O_WRONLY|os.O_APPEND|os.O_CREAT|os.O_NOFOLLOW,0o600)
        with os.fdopen(fd,'a')as output:
            output.write(json.dumps(row,ensure_ascii=False)+'\n');output.flush();os.fsync(output.fileno())
    def append(self,row):
        with self.lock:
            try:self._write_locked(row)
            except OSError:self.control_stop_reason='journal_persistence_error';raise
    def stop(self,reason):
        if self.policy_id==OBSERVE_POLICY:
            with self.lock:self.control_stop_reason=self.control_stop_reason or reason
    def reserve(self,row):
        with self.lock:
            now=time.monotonic()
            blocked=(self.control_stop_reason or ('unknown_usage_stop'if self.policy_id==STRICT_POLICY and self.unknown_usage_seen else
                     'request_cap'if self.seq>=self.max_requests else
                     'wall_clock_cap'if self.policy_id==OBSERVE_POLICY and self.first_forward_monotonic is not None and now-self.first_forward_monotonic>=3000 else None))
            if blocked:return blocked
            self.seq+=1;row['sequence']=self.seq
            if self.first_forward_monotonic is None:self.first_forward_monotonic=now
            try:self._write_locked(row|{'phase':'start','time':time.time(),'dispatch_state':'durably_reserved_before_send'})
            except OSError:self.control_stop_reason='journal_persistence_error';return self.control_stop_reason
            return None

class Handler(BaseHTTPRequestHandler):
    protocol_version='HTTP/1.1'
    def log_message(self,*_):pass
    def reject(self,status,reason):
        data=json.dumps({'error':{'message':reason,'type':'evaluation_contract'}}).encode()
        self.send_response(status);self.send_header('Content-Type','application/json')
        self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
    def do_CONNECT(self):self.reject(405,'CONNECT is forbidden')
    def do_GET(self):
        if self.path!='/v1/models':return self.reject(403,'only the fixed local model catalog is available')
        data=json.dumps({'object':'list','data':[{'id':MODEL,'object':'model','context_window':500000,
            'max_output_tokens':OUTPUT_LIMIT,'reasoning':True,'reasoning_levels':['high']}]}).encode()
        self.send_response(200);self.send_header('Content-Type','application/json')
        self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
    def do_POST(self):
        if self.path!='/v1/chat/completions':
            if self.server.policy_id==OBSERVE_POLICY:
                self.server.stop('request_contract_error')
                self.server.append({'phase':'rejected','policy_id':self.server.policy_id,
                    'served_alias_fingerprint':self.server.alias_fingerprint,'origin':'relay_contract',
                    'reason':'unsupported_chat_path','time':time.time(),'session':self.server.session})
                self.close_connection=True
            return self.reject(403,'only fixed chat completions are allowed')
        try:
            size=int(self.headers.get('Content-Length','0'))
            if not 0<size<=32*1024*1024:raise ValueError('invalid body size')
            raw=self.rfile.read(size);body=json.loads(raw)
            if not isinstance(body,dict):raise ValueError('body must be an object')
            if not isinstance(body.get('messages'),list):raise ValueError('messages must be a list')
            tools=body.get('tools',[])
            if not isinstance(tools,list) or any(not isinstance(t,dict) or not isinstance(t.get('function'),dict) for t in tools):
                raise ValueError('tools must contain function objects')
            if body.get('n',1)!=1:raise ValueError('only one response candidate is allowed')
            if body.get('model')!=MODEL:raise ValueError('model mismatch')
            if body.get('reasoning_effort')!='high':raise ValueError('reasoning effort mismatch')
            limits=[body[k] for k in ('max_tokens','max_completion_tokens') if k in body]
            if not limits or any(type(v) is not int or not 0<v<=OUTPUT_LIMIT for v in limits):raise ValueError('output limit mismatch')
            if self.server.policy_id==OBSERVE_POLICY and len(set(limits))!=1:raise ValueError('ambiguous conflicting output limits')
            if any(k in body for k in ('base_url','url','upstream','api_key')):raise ValueError('upstream overrides forbidden')
        except (ValueError,TypeError,KeyError) as exc:
            self.server.stop('request_contract_error')
            self.server.append({'phase':'rejected','policy_id':self.server.policy_id,'served_alias_fingerprint':self.server.alias_fingerprint,'origin':'relay_contract','reason':str(exc),'time':time.time(),'session':self.server.session,
                'model_requested':body.get('model') if isinstance(locals().get('body'),dict) else None,
                'reasoning_effort':body.get('reasoning_effort') if isinstance(locals().get('body'),dict) else None,
                'output_limits':{k:body.get(k) for k in ('max_tokens','max_completion_tokens') if type(body.get(k)) in (int,float)} if isinstance(locals().get('body'),dict) else {}})
            return self.reject(422,str(exc))
        row={'session':self.server.session,'model_requested':body.get('model'),
             'reasoning_effort':body.get('reasoning_effort'),'output_limits':limits,
             'effective_output_limit':limits[0]if len(set(limits))==1 else None,
             'sampling_parameters':{k:body[k] for k in ('temperature','top_p','seed','parallel_tool_calls')
                                    if k in body and type(body[k]) in (int,float,bool,type(None))},
             'policy_id':self.server.policy_id,'served_alias_fingerprint':self.server.alias_fingerprint,
             'stream':bool(body.get('stream')),'requested_stream':body.get('stream')if type(body.get('stream'))is bool else None,'observed_content_type':None,
             'sse_done_seen':False,'json_parse_complete':False,'transport_eof':False,'response_bytes':0,
             'request_bytes':len(raw),'request_sha256':hashlib.sha256(raw).hexdigest(),
             'messages_sha256':digest(body.get('messages',[])),'tools_sha256':digest(body.get('tools',[])),
             'tool_names':[t.get('function',{}).get('name') for t in body.get('tools',[])],
             'tool_metadata':observe_tool_metadata(body.get('tools',[])),
             'usage':None,'response_id':None,'finish_reasons':[],'terminal_marker':False}
        blocked=self.server.reserve(row)
        if blocked:
            try:self.server.append({'phase':'rejected','reason':blocked,'origin':'relay_control','policy_id':self.server.policy_id,'time':time.time(),'session':self.server.session})
            except OSError:pass
            return self.reject(429 if blocked in ('request_cap','unknown_usage_stop','wall_clock_cap')else 503,blocked)
        endpoint=self.server.upstream
        conn_cls=http.client.HTTPSConnection if endpoint.scheme=='https' else http.client.HTTPConnection
        conn=conn_cls(endpoint.hostname,endpoint.port,timeout=310)
        started=time.monotonic();sent_headers=False;buffer=b'';nonstream=bytearray();response_hash=hashlib.sha256()
        row['dispatch_state']='uncertain';phase='upstream_send'
        def accept(value):
            if not isinstance(value,dict):
                row['response_contract_error']=True;self.server.stop('provider_response_contract');return
            if value.get('id'):row['response_id_sha256']=hashlib.sha256(str(value['id']).encode()).hexdigest()
            if isinstance(value.get('model'),str):
                served=value['model'];canonical=self.server.served_model_aliases.get(served)
                row['served_model_provenance']='known'if canonical is not None else'unknown'
                row['served_model_claim_sha256']=hashlib.sha256(served.encode()).hexdigest()
                if canonical is not None and canonical!=MODEL:row['response_contract_error']=True;self.server.stop('served_model_mismatch')
            row['usage']=normalized_usage(value.get('usage'))if isinstance(value.get('usage'),dict)else row['usage']
            completion=row['usage'].get('completion_tokens')if isinstance(row['usage'],dict)else None
            cap=row.get('effective_output_limit')
            if self.server.policy_id==OBSERVE_POLICY and type(completion)is int and type(cap)is int and completion>cap:
                row['response_contract_error']=True;self.server.stop('provider_output_limit_violation')
            error=value.get('error')
            if isinstance(error,dict):
                if row.get('http_status')==200:row['api_error_envelope']=True;self.server.stop('api_terminal_error')
                if error.get('code')in ('insufficient_quota','billing_hard_limit_reached')or error.get('type')in ('insufficient_quota','billing_hard_limit_reached'):self.server.stop('api_access_error')
            choices=value.get('choices',[])
            if not isinstance(choices,list):row['response_contract_error']=True;self.server.stop('provider_response_contract');return
            for choice in choices:
                if not isinstance(choice,dict):row['response_contract_error']=True;self.server.stop('provider_response_contract');continue
                if choice.get('finish_reason')is not None:
                    reason=choice['finish_reason']
                    row['finish_reasons'].append(reason if reason in ('stop','length','tool_calls','function_call','content_filter')else'unknown')
        try:
            headers={'Content-Type':'application/json','Authorization':'Bearer '+os.environ.get('UPSTREAM_API_KEY','eval-local'),
                     'X-Magpie-Session':self.server.session,'X-Magpie-Agent':'go-cli-eval'}
            # Forward exact JSON bytes; never pass caller Authorization, Host or other headers.
            conn.request('POST',endpoint.path.rstrip('/')+'/v1/chat/completions',body=raw,headers=headers)
            phase='upstream_headers'
            response=conn.getresponse();row['http_status']=response.status;row['dispatch_state']='response_observed'
            if response.status in (401,403):self.server.stop('api_access_error')
            row['observed_content_type']=response.getheader('Content-Type','application/json').split(';',1)[0].strip().lower()
            for name in ('X-Magpie-Provider','X-Magpie-Model','X-Magpie-Request-Id'):
                if response.getheader(name):row[name.lower()]=response.getheader(name)
            self.send_response(response.status);self.send_header('Content-Type',response.getheader('Content-Type','application/json'))
            if response.getheader('Retry-After'):self.send_header('Retry-After',response.getheader('Retry-After'))
            self.send_header('Connection','close');self.end_headers();sent_headers=True;self.close_connection=True
            sse='event-stream' in response.getheader('Content-Type','')
            while True:
                phase='upstream_read'
                chunk=response.read1(65536)
                if not chunk:break
                response_hash.update(chunk);row['response_bytes']+=len(chunk)
                if sse:
                    buffer+=chunk
                    while b'\n' in buffer:
                        line,buffer=buffer.split(b'\n',1)
                        if line.startswith(b'data:'):
                            value=line[5:].strip()
                            if value==b'[DONE]':row['terminal_marker']=True;row['sse_done_seen']=True
                            elif value:
                                try:accept(json.loads(value))
                                except (ValueError,UnicodeDecodeError):pass
                else:nonstream.extend(chunk)
                phase='downstream_write'
                self.wfile.write(chunk);self.wfile.flush()
            row['transport_eof']=True
            if not sse:
                try:accept(json.loads(nonstream));row['terminal_marker']=True;row['json_parse_complete']=True
                except (ValueError,UnicodeDecodeError):pass
        except Exception as exc:
            row['error']=type(exc).__name__;row['error_phase']=phase
            if not sent_headers:
                try:self.reject(502,'upstream transport failure')
                except OSError:pass
        finally:
            conn.close();row['elapsed_seconds']=time.monotonic()-started;row['response_sha256']=response_hash.hexdigest()
            row['served_model_provenance']=row.get('served_model_provenance','unknown')
            with self.server.lock:
                if not isinstance(row['usage'],dict) or type(row['usage'].get('prompt_tokens'))is not int or type(row['usage'].get('completion_tokens'))is not int:
                    self.server.unknown_usage_seen=True
            row['control_stop_reason']=self.server.control_stop_reason
            self.server.append(row|{'phase':'finish','time':time.time()})


def main():
    parser=argparse.ArgumentParser();parser.add_argument('--upstream',required=True)
    parser.add_argument('--journal',required=True);parser.add_argument('--session',required=True)
    parser.add_argument('--port',type=int,default=8000);parser.add_argument('--fixture',action='store_true')
    parser.add_argument('--policy',choices=[STRICT_POLICY,OBSERVE_POLICY],default=STRICT_POLICY)
    group=parser.add_mutually_exclusive_group()
    group.add_argument('--served-alias-manifest');group.add_argument('--served-alias-json')
    args=parser.parse_args()
    aliases=({}if args.served_alias_manifest is None and args.served_alias_json is None else
             json.loads(args.served_alias_json)if args.served_alias_json is not None else json.loads(Path(args.served_alias_manifest).read_text()))
    resolve_audit_policy(args.policy,None if not aliases else aliases)
    if args.fixture:
        from offline_fixture import server
        fixture=server();threading.Thread(target=fixture.serve_forever,daemon=True).start()
        args.upstream=f'http://127.0.0.1:{fixture.server_port}'
    AuditServer(('0.0.0.0',args.port),args.upstream,Path(args.journal),args.session,policy_id=args.policy,served_model_aliases=aliases.get('aliases'),served_model_evidence_sha256=aliases.get('evidence_sha256')).serve_forever()

if __name__=='__main__':main()
