"""Allow-listed terminal metadata from pinned Go/Pi NDJSON, never raw content."""
import hashlib
import json
import os
import stat
from pathlib import Path

SCHEMAS={'go':'go-agent-event-e9-v1','pi':'pi-json-event-1.4.0-v1'}
MAX_LINE_BYTES=16*1024*1024


def observe_native_terminal(arm,path,exit_code):
    if arm not in SCHEMAS:raise ValueError('unknown native arm')
    result={'schema':SCHEMAS[arm],'status':'unknown','event_status':'unknown','parse_status':'unknown',
            'records_count':0,'stdout_sha256':None,'stdout_size':None,
            'prior_error_events':0,'recovered_prior_errors':False}
    path=Path(path)
    try:
        before=path.lstat()
        if not stat.S_ISREG(before.st_mode)or before.st_nlink!=1:raise ValueError('unknown stdout file identity')
        digest=hashlib.sha256();bad=False;oversize=False
        last_go=None;last_end=None;end_index=-1;settled=None;settled_index=-1
        fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
        with os.fdopen(fd,'rb')as source:
            opened=os.fstat(source.fileno())
            if (opened.st_dev,opened.st_ino)!=(before.st_dev,before.st_ino):raise ValueError('stdout identity changed')
            while True:
                raw=source.readline(MAX_LINE_BYTES+1)
                if not raw:break
                digest.update(raw)
                if len(raw)>MAX_LINE_BYTES:
                    oversize=True
                    while raw and not raw.endswith(b'\n'):
                        raw=source.readline(MAX_LINE_BYTES+1);digest.update(raw)
                    continue
                if not raw.strip():continue
                try:row=json.loads(raw)
                except (ValueError,UnicodeError):bad=True;continue
                result['records_count']+=1
                if not isinstance(row,dict):bad=True;continue
                if arm=='go':
                    if row.get('kind')in ('done','error'):
                        if not isinstance(row.get('content'),str)or not isinstance(row.get('timestamp'),str)or not row['timestamp']:
                            bad=True;continue
                        last_go=row['kind']
                        if last_go=='error':result['prior_error_events']+=1
                else:
                    if row.get('type')=='agent_end':
                        last_end=row;end_index=result['records_count']
                        messages=row.get('messages')
                        assistants=[m for m in messages if isinstance(m,dict)and m.get('role')=='assistant']if isinstance(messages,list)else[]
                        if assistants and assistants[-1].get('stopReason')=='error':result['prior_error_events']+=1
                    elif row.get('type')=='agent_settled':settled=row;settled_index=result['records_count']
        after=path.lstat();result.update(stdout_sha256=digest.hexdigest(),stdout_size=after.st_size)
        stable=(before.st_dev,before.st_ino,before.st_size,before.st_mtime_ns)==(after.st_dev,after.st_ino,after.st_size,after.st_mtime_ns)
        if not stable:result['parse_status']='output_changed'
        elif bad or oversize:result['parse_status']='malformed_or_oversized_ndjson'
        elif arm=='go':
            result['parse_status']='complete'if last_go is not None else'missing_typed_terminal'
            result['event_status']={'done':'success','error':'runtime_error'}.get(last_go,'unknown')
        elif last_end is not None and settled is not None and settled_index>end_index:
            messages=last_end.get('messages')
            assistants=[m for m in messages if isinstance(m,dict)and m.get('role')=='assistant']if isinstance(messages,list)else[]
            reason=assistants[-1].get('stopReason')if assistants else None
            if last_end.get('willRetry')is False and type(settled.get('aborted'))is bool:
                result['parse_status']='complete'
                result['event_status']=('aborted'if settled['aborted']else
                    {'stop':'success','error':'api_error','aborted':'aborted','length':'output_truncated'}.get(reason,'unknown'))
            else:result['parse_status']='retry_pending_or_unknown_settlement'
        else:result['parse_status']='missing_final_end_or_settlement'
        if stable and not bad and not oversize:result['status']=result['event_status']
    except (OSError,ValueError):result['parse_status']='unavailable_or_unknown_output'
    if type(exit_code)is int and exit_code!=0:result['status']='runtime_error'
    elif type(exit_code)is not int:result['status']='unknown'
    result['recovered_prior_errors']=result['status']=='success'and result['prior_error_events']>0
    return result
