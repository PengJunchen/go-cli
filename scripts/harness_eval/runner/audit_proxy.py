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


class AuditServer(ThreadingHTTPServer):
    daemon_threads=True
    def __init__(self,address,upstream,journal,session):
        super().__init__(address,Handler)
        self.upstream=urllib.parse.urlsplit(upstream)
        if self.upstream.scheme not in ('http','https') or self.upstream.username or self.upstream.password or self.upstream.path not in ('','/') or self.upstream.query or self.upstream.fragment:
            raise ValueError('invalid fixed upstream')
        self.journal=Path(journal);self.journal.parent.mkdir(parents=True,exist_ok=True)
        self.session=session;self.lock=threading.Lock();self.seq=0
        self.max_requests=80;self.unknown_usage_seen=False
    def append(self,row):
        with self.lock:
            with self.journal.open('a') as f:f.write(json.dumps(row,ensure_ascii=False)+'\n')


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
        if self.path!='/v1/chat/completions':return self.reject(403,'only fixed chat completions are allowed')
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
            if any(k in body for k in ('base_url','url','upstream','api_key')):raise ValueError('upstream overrides forbidden')
        except (ValueError,TypeError,KeyError) as exc:
            self.server.append({'phase':'rejected','reason':str(exc),'time':time.time(),'session':self.server.session,
                'model_requested':body.get('model') if isinstance(locals().get('body'),dict) else None,
                'reasoning_effort':body.get('reasoning_effort') if isinstance(locals().get('body'),dict) else None,
                'output_limits':{k:body.get(k) for k in ('max_tokens','max_completion_tokens') if type(body.get(k)) in (int,float)} if isinstance(locals().get('body'),dict) else {}})
            return self.reject(422,str(exc))
        with self.server.lock:
            budget_blocked=('unknown_usage_stop' if self.server.unknown_usage_seen else
                            'request_cap' if self.server.seq>=self.server.max_requests else None)
            if not budget_blocked:
                self.server.seq+=1
            seq=self.server.seq
        if budget_blocked:
            self.server.append({'phase':'rejected','reason':budget_blocked,'time':time.time(),'session':self.server.session})
            return self.reject(429,'evaluation request cap or unknown usage stop')
        row={'sequence':seq,'session':self.server.session,'model_requested':body.get('model'),
             'reasoning_effort':body.get('reasoning_effort'),'output_limits':limits,
             'sampling_parameters':{k:body[k] for k in ('temperature','top_p','seed','parallel_tool_calls')
                                    if k in body and type(body[k]) in (int,float,bool,type(None))},
             'stream':bool(body.get('stream')),'request_bytes':len(raw),'request_sha256':hashlib.sha256(raw).hexdigest(),
             'messages_sha256':digest(body.get('messages',[])),'tools_sha256':digest(body.get('tools',[])),
             'tool_names':[t.get('function',{}).get('name') for t in body.get('tools',[])],
             'tool_metadata':observe_tool_metadata(body.get('tools',[])),
             'usage':None,'response_id':None,'finish_reasons':[],'terminal_marker':False}
        self.server.append(row|{'phase':'start','time':time.time()})
        endpoint=self.server.upstream
        conn_cls=http.client.HTTPSConnection if endpoint.scheme=='https' else http.client.HTTPConnection
        conn=conn_cls(endpoint.hostname,endpoint.port,timeout=310)
        started=time.monotonic();sent_headers=False;buffer=b'';nonstream=bytearray()
        def accept(value):
            if not isinstance(value,dict):return
            if value.get('id'):row['response_id']=value['id']
            if value.get('model'):row['model_served_from_response']=value['model']
            if isinstance(value.get('usage'),dict):row['usage']=value['usage']
            for choice in value.get('choices',[]):
                if choice.get('finish_reason') is not None:row['finish_reasons'].append(choice['finish_reason'])
        try:
            headers={'Content-Type':'application/json','Authorization':'Bearer '+os.environ.get('UPSTREAM_API_KEY','eval-local'),
                     'X-Magpie-Session':self.server.session,'X-Magpie-Agent':'go-cli-eval'}
            # Forward exact JSON bytes; never pass caller Authorization, Host or other headers.
            conn.request('POST',endpoint.path.rstrip('/')+'/v1/chat/completions',body=raw,headers=headers)
            response=conn.getresponse();row['http_status']=response.status
            for name in ('X-Magpie-Provider','X-Magpie-Model','X-Magpie-Request-Id'):
                if response.getheader(name):row[name.lower()]=response.getheader(name)
            self.send_response(response.status);self.send_header('Content-Type',response.getheader('Content-Type','application/json'))
            self.send_header('Connection','close');self.end_headers();sent_headers=True;self.close_connection=True
            sse='event-stream' in response.getheader('Content-Type','')
            while chunk:=response.read1(65536):
                if sse:
                    buffer+=chunk
                    while b'\n' in buffer:
                        line,buffer=buffer.split(b'\n',1)
                        if line.startswith(b'data:'):
                            value=line[5:].strip()
                            if value==b'[DONE]':row['terminal_marker']=True
                            elif value:
                                try:accept(json.loads(value))
                                except (ValueError,UnicodeDecodeError):pass
                else:nonstream.extend(chunk)
                self.wfile.write(chunk);self.wfile.flush()
            if not sse:
                try:accept(json.loads(nonstream));row['terminal_marker']=True
                except (ValueError,UnicodeDecodeError):pass
        except Exception as exc:
            row['error']=type(exc).__name__
            if not sent_headers:
                try:self.reject(502,'upstream transport failure')
                except OSError:pass
        finally:
            conn.close();row['elapsed_seconds']=time.monotonic()-started
            with self.server.lock:
                if not isinstance(row['usage'],dict) or not isinstance(row['usage'].get('prompt_tokens'),int) or not isinstance(row['usage'].get('completion_tokens'),int):
                    self.server.unknown_usage_seen=True
            self.server.append(row|{'phase':'finish','time':time.time()})


def main():
    parser=argparse.ArgumentParser();parser.add_argument('--upstream',required=True)
    parser.add_argument('--journal',required=True);parser.add_argument('--session',required=True)
    parser.add_argument('--port',type=int,default=8000);parser.add_argument('--fixture',action='store_true');args=parser.parse_args()
    if args.fixture:
        from offline_fixture import server
        fixture=server();threading.Thread(target=fixture.serve_forever,daemon=True).start()
        args.upstream=f'http://127.0.0.1:{fixture.server_port}'
    AuditServer(('0.0.0.0',args.port),args.upstream,Path(args.journal),args.session).serve_forever()

if __name__=='__main__':main()
