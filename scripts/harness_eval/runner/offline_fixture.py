"""No model calls: deterministic read->final protocol fixture, not task solving."""
import json
import os
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
from eval_core import MODEL

class FixtureHandler(BaseHTTPRequestHandler):
    def log_message(self,*_):pass
    def do_POST(self):
        body=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        observed=any(m.get('role')=='tool' for m in body.get('messages',[]))
        tool=next((t['function'] for t in body.get('tools',[]) if t['function'].get('name')=='read'),None)
        message={'role':'assistant','content':'OFFLINE_FIXTURE_DONE'}
        finish='stop'
        if not observed and tool:
            props=tool.get('parameters',{}).get('properties',{})
            name=next((n for n in ('path','file_path','filePath') if n in props),'path')
            message={'role':'assistant','content':None,'tool_calls':[{'id':'fixture-read-1','type':'function',
               'function':{'name':'read','arguments':json.dumps({name:'/app/go.mod'})}}]}
            names={t['function'].get('name') for t in body.get('tools',[])}
            if {'read','bash','git_status'} <= names:
                message['tool_calls'].extend([
                  {'id':'fixture-bash-1','type':'function','function':{'name':'bash','arguments':json.dumps({'command':'printf fixture-managed'})}},
                  {'id':'fixture-git-status-1','type':'function','function':{'name':'git_status','arguments':'{}'}},
                  {'id':'fixture-read-2','type':'function','function':{'name':'read','arguments':json.dumps({name:'/app/go.mod'})}}
                ])
            if os.environ.get('EVAL_FIXTURE_SCENARIO')=='single-status' and 'git_status' in names:
                message['tool_calls']=[{'id':'fixture-git-status-1','type':'function','function':{'name':'git_status','arguments':'{}'}}]
            finish='tool_calls'
        if os.environ.get('EVAL_FIXTURE_SCENARIO')=='write-parallel':
            completed={m.get('tool_call_id') for m in body.get('messages',[]) if m.get('role')=='tool'}
            if 'fixture-write-1' not in completed:
                message={'role':'assistant','content':None,'tool_calls':[{'id':'fixture-write-1','type':'function','function':{'name':'write','arguments':json.dumps({'path':'/app/.eval-fixture.txt','content':'fixture-canary-5736e88\n'})}}]}
                finish='tool_calls'
            elif 'fixture-git-status-1' not in completed:
                props=tool.get('parameters',{}).get('properties',{})
                name=next((n for n in ('path','file_path','filePath') if n in props),'path')
                message={'role':'assistant','content':None,'tool_calls':[
                  {'id':'fixture-read-1','type':'function','function':{'name':'read','arguments':json.dumps({name:'/app/.eval-fixture.txt'})}},
                  {'id':'fixture-bash-1','type':'function','function':{'name':'bash','arguments':json.dumps({'command':'printf fixture-managed'})}},
                  {'id':'fixture-git-status-1','type':'function','function':{'name':'git_status','arguments':'{}'}},
                  {'id':'fixture-read-2','type':'function','function':{'name':'read','arguments':json.dumps({name:'/app/.eval-fixture.txt'})}}]}
                finish='tool_calls'
        usage={'prompt_tokens':100,'completion_tokens':3,'prompt_tokens_details':{'cached_tokens':0},
               'completion_tokens_details':{'reasoning_tokens':0}}
        self.send_response(200)
        if body.get('stream'):
            self.send_header('Content-Type','text/event-stream');self.end_headers()
            delta=dict(message)
            if 'tool_calls' in delta:
                for index, call in enumerate(delta['tool_calls']):call['index']=index
            obj={'id':'fixture-response','model':MODEL,'object':'chat.completion.chunk',
                 'choices':[{'index':0,'delta':delta,'finish_reason':None}]}
            self.wfile.write(('data: '+json.dumps(obj)+'\n\n').encode())
            obj['choices']=[{'index':0,'delta':{},'finish_reason':finish}];obj['usage']=usage
            self.wfile.write(('data: '+json.dumps(obj)+'\n\ndata: [DONE]\n\n').encode())
        else:
            self.send_header('Content-Type','application/json');self.end_headers()
            self.wfile.write(json.dumps({'id':'fixture-response','model':MODEL,'object':'chat.completion',
              'choices':[{'index':0,'message':message,'finish_reason':finish}],'usage':usage}).encode())

def server():return ThreadingHTTPServer(('127.0.0.1',0),FixtureHandler)
