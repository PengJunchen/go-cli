"""One frozen CLI attempt in an internal-only task container. No agent retries."""
import argparse
import hashlib
import inspect
import io
import stat
import tarfile
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import time
import tomllib
import uuid
from audit_proxy import OBSERVE_POLICY, STRICT_POLICY, ledger_observations, resolve_audit_policy
from native_terminal import observe_native_terminal
from eval_core import MODEL,GO_REVISION,PROFILE_ID,PROFILE_IDS,PI_VERSION,resolve_profile,go_config,pi_config,cli_argv,usage_totals

ROOT=Path(__file__).resolve().parent

def dump(path,value):
    path.parent.mkdir(parents=True,exist_ok=True)
    path.write_text(json.dumps(value,indent=2,ensure_ascii=False)+'\n')

def sha(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()

def validate_go_supervisor_environment(environment):
    """Validate only the fixed opt-out key; never return raw container env."""
    if not isinstance(environment,list) or any(not isinstance(entry,str) for entry in environment):
        raise ValueError('container supervisor environment is unknown')
    values=[]
    for entry in environment:
        key,separator,value=entry.partition('=')
        if key=='GO_CLI_LINUX_SUBREAPER':values.append(value if separator else None)
    if values!=['0']:
        raise ValueError('container supervisor environment must contain exactly one explicit opt-out')
    return {'source':'docker_inspect_Config.Env','effective_value':'0','matching_entry_count':1,
            'validated_before_container_start':True}

def load_payload_manifest(payload):
    metadata=json.loads((payload/'manifest.json').read_text())
    revision=metadata.get('go_cli_source_revision','')
    if len(revision)!=40 or any(c not in '0123456789abcdef' for c in revision):
        raise ValueError('missing exact Go source revision')
    if metadata.get('go_cli_sha256')!=sha(payload/'go-cli'):
        raise ValueError('Go binary hash does not match source manifest')
    return metadata

def docker(*args,check=True,timeout=120):
    return subprocess.run(['docker',*map(str,args)],check=check,capture_output=True,text=True,timeout=timeout)

def resolve_recorded_image(image_id):
    if not isinstance(image_id,str) or not re.fullmatch(r'sha256:[0-9a-f]{64}',image_id):
        raise ValueError('missing immutable source image ID')
    metadata=json.loads(docker('image','inspect',image_id).stdout)[0]
    if metadata.get('Id')!=image_id:
        raise ValueError('source and actual grading image IDs differ')
    return metadata

def resolve_locked_image(entry,task_image):
    pinned=entry.get('pinned_image')
    if not isinstance(pinned,str) or not re.fullmatch(r'[^@\s]+@sha256:[0-9a-f]{64}',pinned):
        raise ValueError('missing immutable task image digest pin')
    repository=task_image.split('@',1)[0]
    if repository.rfind(':')>repository.rfind('/'):
        repository=repository.rsplit(':',1)[0]
    if pinned.split('@',1)[0]!=repository:
        raise ValueError('task and pinned image repositories differ')
    metadata=json.loads(docker('image','inspect',pinned).stdout)[0]
    if pinned not in metadata.get('RepoDigests',[]) or not re.fullmatch(r'sha256:[0-9a-f]{64}',metadata.get('Id','')):
        raise ValueError('local image does not prove its locked digest and actual ID')
    return pinned,metadata

def run_cli_once(cid,argv,out,timeout):
    started=time.monotonic()
    with (out/'stdout.jsonl').open('wb') as stdout,(out/'stderr.txt').open('wb') as stderr:
        proc=subprocess.Popen(['docker','exec',cid,'timeout','--signal=TERM','--kill-after=15s',f'{timeout}s',*argv],
            stdin=subprocess.DEVNULL,stdout=stdout,stderr=stderr)
        try:
            proc.wait(timeout=timeout+45)
            return {'exit_code':proc.returncode,'elapsed_seconds':time.monotonic()-started,
                    'timed_out':proc.returncode in (124,137),'cli_invocations':1}
        except subprocess.TimeoutExpired:
            docker('kill',cid,check=False,timeout=20)
            proc.wait(timeout=20)
            return {'exit_code':proc.returncode,'elapsed_seconds':time.monotonic()-started,
                    'timed_out':True,'outer_kill':True,'cli_invocations':1}

def final_record(result,rows):
    starts={r['sequence'] for r in rows if r.get('phase')=='start'}
    finishes={r.get('sequence') for r in rows if r.get('phase')=='finish'}
    opened=starts-finishes
    totals=usage_totals(rows+[{'phase':'finish','usage':None} for _ in opened])
    reasons=[r.get('reason','') for r in rows if r.get('phase')=='rejected']
    stop_class=('budget_stop' if 'request_cap' in reasons else
                'measurement_stop' if 'unknown_usage_stop' in reasons or opened else
                'api_contract_stop' if reasons else None)
    record=result|{'task_passed':None,'usage':totals,'open_attempts':len(opened),
                   'wire_contract_rejections':len(reasons),'stop_class':stop_class}
    if result.get('audit_policy_id')==OBSERVE_POLICY or any(r.get('policy_id')==OBSERVE_POLICY for r in rows):
        observed=ledger_observations(rows)
        policies={r.get('policy_id')for r in rows if r.get('phase')in ('start','finish')}
        fingerprints={r.get('served_alias_fingerprint')for r in rows if r.get('served_alias_fingerprint')}
        declared=result.get('audit_policy_fingerprint')
        if not fingerprints and isinstance(declared,str):fingerprints={declared}
        policy_invalid=(bool(policies)and policies!={OBSERVE_POLICY})or len(fingerprints)!=1 or(bool(declared)and fingerprints!={declared})
        ends=[r for r in rows if r.get('phase')=='finish']
        last=ends[-1]if ends else{}
        usable=(type(last.get('http_status'))is int and 200<=last['http_status']<300
                and not last.get('error')and not last.get('api_error_envelope')and not last.get('response_contract_error')
                and last.get('transport_eof')is True and(last.get('json_parse_complete')is True or last.get('sse_done_seen')is True or bool(last.get('finish_reasons'))))
        controls={r.get('control_stop_reason')for r in ends if r.get('control_stop_reason')}
        native_terminal=result.get('native_terminal_status')
        api_terminal=(result.get('api_terminal_error')is True or native_terminal=='api_error'
                      or bool(controls & {'api_access_error','api_terminal_error'}))
        completion_unknown=(type(result.get('exit_code'))is int and result['exit_code']==0)and not api_terminal and not result.get('native_terminal_failure')and (native_terminal=='unknown'or(bool(ends)and not usable and native_terminal!='success'))
        stop=('budget_stop'if any(r in ('request_cap','wall_clock_cap')for r in reasons)else
              'api_contract_stop'if reasons or policy_invalid or bool(controls-{'api_access_error','api_terminal_error'})else'api_terminal_error'if api_terminal else
              'measurement_stop'if observed['ledger_status']!='complete'else None)
        record.update(audit_policy_id=OBSERVE_POLICY,audit_policy_fingerprint=next(iter(fingerprints))if len(fingerprints)==1 else None,
                      audit_measurement=observed,usage=observed['usage'],
                      api_terminal_error=api_terminal,api_completion_unknown=completion_unknown,
                      api_control_stop_reasons=sorted(controls),stop_class=stop,
                      unknown_usage_policy='observe null/coverage/lower bound; native retries retain common budgets')
    return record


PROBE=r'''
import json,urllib.request,time,socket,sys
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
results={}
for attempt in range(80):
 try:
  opener.open('http://relay:8000/v1/models',timeout=2).close();break
 except Exception:
  time.sleep(.25)
for label,url in [('relay','http://relay:8000/v1/models'),('github','https://github.com'),
 ('pypi','https://pypi.org/simple/'),('npm','https://registry.npmjs.org'),
 ('host_gateway','http://host.docker.internal:3425/v1/models')]:
 try:
  with opener.open(url,timeout=4) as response: response.read(10)
  results[label]=True
 except Exception as exc:
  results[label]=False
  if label=='relay':results['relay_error']=type(exc).__name__+':'+str(exc)[:160]
for label,host,port in [('public_ip','1.1.1.1',443),('internal_host_gateway',sys.argv[1],3425)]:
 try:
  socket.create_connection((host,port),timeout=3).close();results[label]=True
 except OSError:results[label]=False
print(json.dumps(results))
'''

def snapshot_processes(proc_root,excluded_pids):
    """Read metadata only; an unreadable process is unknown, not absent."""
    snapshot={'snapshot_unix_time':time.time(),'scope':'owned task PID namespace before cleanup signals',
              'excluded_pids':sorted(excluded_pids),'enumeration_status':'unknown',
              'processes':None,'observed_process_count':None,'unknown_process_count':None}
    try:
        names=os.listdir(proc_root)
    except OSError as exc:
        snapshot['enumeration_error']=type(exc).__name__
        return snapshot
    rows=[]
    for pid in sorted({int(name) for name in names if name.isdigit()}-set(excluded_pids)):
        row={'pid':pid,'status':'unknown'}
        try:
            with open(os.path.join(proc_root,str(pid),'stat'),encoding='utf-8') as source:
                value=source.read()
            left,right=value.find('('),value.rfind(')')
            fields=value[right+2:].split()
            if left<0 or right<=left or int(value[:left].strip())!=pid or len(fields)<20:
                raise ValueError('invalid proc stat identity')
            row={'pid':pid,'status':'observed','comm':value[left+1:right],'state':fields[0],
                 'ppid':int(fields[1]),'pgid':int(fields[2]),'starttime':int(fields[19])}
        except FileNotFoundError:row['reason']='process_disappeared'
        except PermissionError:row['reason']='permission_denied'
        except (ValueError,UnicodeError):row['reason']='malformed_proc_stat'
        except OSError:row['reason']='os_error'
        rows.append(row)
    snapshot.update(enumeration_status='observed',processes=rows,
                    observed_process_count=sum(row['status']=='observed' for row in rows),
                    unknown_process_count=sum(row['status']=='unknown' for row in rows))
    return snapshot


def _keeper_container_id(container_id):
    if not isinstance(container_id,str) or len(container_id)!=64 or any(c not in '0123456789abcdef' for c in container_id):
        raise RuntimeError('exact owned keeper container identity unavailable')
    return container_id


def _keeper_identity(proc_root,row,container_id):
    if (not isinstance(row,dict) or row.get('status')!='observed'
            or any(type(row.get(key)) is not int for key in ('pid','ppid','pgid','starttime'))
            or row['pid']<=1 or row['pid']==os.getpid() or row['ppid']!=1
            or row['starttime']<=0 or row.get('state') not in ('R','S','D','I')):
        raise RuntimeError('trusted init keeper process identity unavailable')
    try:
        executable=os.path.join(proc_root,str(row['pid']),'exe')
        target=os.readlink(executable)
        info=os.stat(executable)
    except OSError as exc:
        raise RuntimeError('trusted init keeper executable identity unavailable') from exc
    if (not isinstance(target,str) or not target.startswith('/') or target.endswith(' (deleted)')
            or type(info.st_dev) is not int or type(info.st_ino) is not int or info.st_dev<0 or info.st_ino<=0):
        raise RuntimeError('trusted init keeper executable identity malformed')
    return {'schema':'trusted-init-keeper/v1','container_id':_keeper_container_id(container_id),'init':True,
            'pid':row['pid'],'ppid':row['ppid'],'pgid':row['pgid'],'starttime':row['starttime'],
            'exe':target,'exe_device':info.st_dev,'exe_inode':info.st_ino}


def _validate_keeper_binding(binding,container_id):
    keys={'schema','container_id','init','pid','ppid','pgid','starttime','exe','exe_device','exe_inode'}
    if (not isinstance(binding,dict) or set(binding)!=keys
            or binding['schema']!='trusted-init-keeper/v1' or binding['init']is not True
            or binding['container_id']!=_keeper_container_id(container_id)
            or any(type(binding[key])is not int for key in ('pid','ppid','pgid','starttime','exe_device','exe_inode'))
            or binding['pid']<=1 or binding['ppid']!=1 or binding['starttime']<=0
            or binding['exe_device']<0 or binding['exe_inode']<=0
            or not isinstance(binding['exe'],str) or not binding['exe'].startswith('/')):
        raise RuntimeError('trusted init keeper binding unavailable or mismatched')
    return binding


def capture_init_keeper(proc_root,container_id):
    # Only the trusted startup stage calls this, before payload/native dispatch.
    # The fixed sleep-infinity entrypoint is then the unique non-collector process.
    _keeper_container_id(container_id)
    before=snapshot_processes(proc_root,[1,os.getpid()])
    if before['enumeration_status']!='observed' or before['unknown_process_count']!=0 or len(before['processes'])!=1:
        raise RuntimeError('trusted init keeper startup is missing, ambiguous or unknown')
    return _keeper_identity(proc_root,before['processes'][0],container_id)


def verified_init_keeper_pid(proc_root,before,binding,container_id):
    binding=_validate_keeper_binding(binding,container_id)
    matches=[row for row in before['processes'] if row.get('pid')==binding['pid']]
    if len(matches)!=1 or _keeper_identity(proc_root,matches[0],container_id)!=binding:
        raise RuntimeError('trusted init keeper changed before collection')
    return binding['pid']


_KEEPER_HELPERS=''.join(inspect.getsource(helper) for helper in
    (snapshot_processes,_keeper_container_id,_keeper_identity,_validate_keeper_binding,
     capture_init_keeper,verified_init_keeper_pid))
KEEPER_CAPTURE='import json,os,sys,time\n'+_KEEPER_HELPERS+"print(json.dumps(capture_init_keeper('/proc',sys.argv[1]),sort_keys=True))\n"


# Embed the same pure helper exercised by fake-procfs tests. Do not execute
# this signal-sending program on the host; it is for our owned container only.
COLLECT='import json,os,signal,subprocess,sys,time\n'+_KEEPER_HELPERS+r'''
baseline=sys.argv[1]
# Only this container's PID namespace. Stop then kill every leftover agent/tool
# process before reading artifacts; PID 1, this collector and a verified init
# keeper survive. Keeper identity is never inferred from comm or a fixed PID.
before=snapshot_processes('/proc',[1,os.getpid()])
os.makedirs('/logs/agent',exist_ok=True)
with open('/logs/agent/processes-before-cleanup.json','w') as output:
 json.dump(before,output)
 output.flush()
 os.fsync(output.fileno())
if before['enumeration_status']!='observed':
 raise RuntimeError('process enumeration unknown; pre-cleanup evidence retained')
keeper=None
if len(sys.argv)>2:
 keeper=verified_init_keeper_pid('/proc',before,json.loads(sys.argv[2]),sys.argv[3])
others=[row['pid'] for row in before['processes'] if row['pid']!=keeper]
for sig in (signal.SIGSTOP,signal.SIGKILL):
 for pid in others:
  try:os.kill(pid,sig)
  except ProcessLookupError:pass
base=['git','-c','core.hooksPath=/dev/null','-c','core.fsmonitor=false','-c','diff.external=']
subprocess.run(base+['add','-A'],check=True)
r=subprocess.run(base+['diff','--cached','--binary','--no-ext-diff',baseline],capture_output=True,check=True)
open('/logs/agent/model.patch','wb').write(r.stdout)
subprocess.run(base+['reset','-q'],check=True)
open('/logs/agent/process-cleanup.json','w').write(json.dumps({'other_pids_stopped_then_killed':others,'namespace':'owned task container'}))
'''


NATIVE_CONFIG_TARGETS = {
    'go.json':'/root/.go-cli/config.json',
    'models.json':'/root/.pi/agent/models.json',
    'pi-settings.json':'/root/.pi/agent/settings.json',
}
NATIVE_CONFIG_DIRS = ('root/.go-cli','root/.pi','root/.pi/agent')

CONFIG_IDENTITY_PROBE = r'''import json,os,stat
identity={'uid':os.getuid(),'gid':os.getgid(),'euid':os.geteuid(),'egid':os.getegid()}
for path in ['/root','/root/.go-cli','/root/.pi','/root/.pi/agent']:
 try: info=os.lstat(path)
 except FileNotFoundError:
  if path=='/root':raise
  continue
 if not stat.S_ISDIR(info.st_mode):raise RuntimeError('native config parent is not an ordinary directory')
 if path!='/root' and ((info.st_uid,info.st_gid)!=(identity['euid'],identity['egid']) or stat.S_IMODE(info.st_mode) not in (0o700,0o755)):
  raise RuntimeError('existing native config parent has another owner or uncontrolled mode')
for path in ['/root/.go-cli/config.json','/root/.pi/agent/models.json','/root/.pi/agent/settings.json']:
 if os.path.lexists(path):raise RuntimeError('refusing to overwrite an existing native config')
if not os.access('/root',os.R_OK|os.X_OK):raise RuntimeError('default identity cannot traverse native config home')
print(json.dumps({'identity':identity,'paths_safe':True},sort_keys=True))
'''

CONFIG_READBACK_PROBE = r'''import hashlib,json,os,stat,sys
expected=json.loads(sys.argv[1])
identity={'uid':os.getuid(),'gid':os.getgid(),'euid':os.geteuid(),'egid':os.getegid()}
if identity!=expected['identity']:raise RuntimeError('default exec identity changed')
result={'identity':identity,'directories':{},'files':{}}
for path in ['/root','/root/.go-cli','/root/.pi','/root/.pi/agent']:
 info=os.lstat(path)
 if not stat.S_ISDIR(info.st_mode):raise RuntimeError('native config parent is not an ordinary directory')
 if path=='/root':continue
 row={'uid':info.st_uid,'gid':info.st_gid,'mode':stat.S_IMODE(info.st_mode)}
 if row!=expected['directories'][path]:raise RuntimeError('private config directory owner/mode mismatch')
 result['directories'][path]=row
for path in ['/root/.go-cli/config.json','/root/.pi/agent/models.json','/root/.pi/agent/settings.json']:
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 try:
  info=os.fstat(fd)
  if not stat.S_ISREG(info.st_mode) or info.st_nlink!=1:raise RuntimeError('native config is not an independent regular file')
  contents=b''
  while True:
   block=os.read(fd,65536)
   if not block:break
   contents+=block
   if len(contents)>1024*1024:raise RuntimeError('native config exceeds bound')
 finally:os.close(fd)
 row={'uid':info.st_uid,'gid':info.st_gid,'mode':stat.S_IMODE(info.st_mode),'size':len(contents),'sha256':hashlib.sha256(contents).hexdigest()}
 if row!=expected['files'][path]:raise RuntimeError('native config readback bytes/owner/mode mismatch')
 result['files'][path]=row
print(json.dumps(result,sort_keys=True))
'''


def validate_native_identity(value):
    if (not isinstance(value,dict) or set(value)!={'uid','gid','euid','egid'}
            or any(type(v) is not int or not 0<=v<2**32-1 for v in value.values())
            or value['uid']!=value['euid'] or value['gid']!=value['egid']):
        raise ValueError('actual default exec UID/GID is unknown or inconsistent')
    return dict(value)


def write_native_configs(configs,profile_id):
    configs=Path(configs)
    configs.mkdir(mode=0o700,parents=False,exist_ok=False)
    values={'go.json':go_config('http://relay:8000/v1',profile_id=profile_id),
            'models.json':pi_config('http://relay:8000/v1'),
            'pi-settings.json':{'cacheWarming':'off','telemetry':False}}
    for name,value in values.items():
        data=(json.dumps(value,indent=2,ensure_ascii=False)+'\n').encode()
        fd=os.open(configs/name,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
        with os.fdopen(fd,'wb') as stream:stream.write(data)


def make_native_config_archive(configs,identity):
    identity=validate_native_identity(identity)
    configs=Path(configs)
    if configs.is_symlink() or not configs.is_dir() or stat.S_IMODE(configs.stat().st_mode)!=0o700 or set(p.name for p in configs.iterdir())!=set(NATIVE_CONFIG_TARGETS):
        raise ValueError('native config sources must be exactly the fixed ordinary files')
    expected={'identity':identity,'directories':{},'files':{}}
    output=io.BytesIO()
    with tarfile.open(fileobj=output,mode='w',format=tarfile.USTAR_FORMAT) as archive:
        for name in NATIVE_CONFIG_DIRS:
            row=tarfile.TarInfo(name);row.type=tarfile.DIRTYPE;row.mode=0o700
            row.uid=identity['uid'];row.gid=identity['gid'];row.uname=row.gname='';row.mtime=0
            archive.addfile(row)
            expected['directories']['/'+name]={'uid':row.uid,'gid':row.gid,'mode':row.mode}
        for name,target in NATIVE_CONFIG_TARGETS.items():
            fd=os.open(configs/name,os.O_RDONLY|os.O_NOFOLLOW)
            with os.fdopen(fd,'rb') as stream:
                info=os.fstat(stream.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_nlink!=1 or stat.S_IMODE(info.st_mode)!=0o600:
                    raise ValueError('native config source must be private unlinked regular file')
                data=stream.read(1024*1024+1)
            if len(data)>1024*1024:raise ValueError('native config source exceeds bound')
            row=tarfile.TarInfo(target.lstrip('/'));row.type=tarfile.REGTYPE;row.mode=0o600
            row.uid=identity['uid'];row.gid=identity['gid'];row.uname=row.gname='';row.mtime=0;row.size=len(data)
            archive.addfile(row,io.BytesIO(data))
            expected['files'][target]={'uid':row.uid,'gid':row.gid,'mode':row.mode,'size':len(data),'sha256':hashlib.sha256(data).hexdigest()}
    return output.getvalue(),expected


def copy_native_config_archive(cid,archive):
    # Daemon-side extraction preserves the archive's explicit numeric owner.
    # No in-container chown, capability addition, or global upload API.
    return subprocess.run(['docker','cp','-a','-',cid+':/'],input=archive,check=True,
                          capture_output=True,timeout=120)


def install_native_configs(cid,configs,*,profile_id=PROFILE_ID):
    observation=json.loads(docker('exec',cid,'python3','-c',CONFIG_IDENTITY_PROBE).stdout)
    if not isinstance(observation,dict) or observation.get('paths_safe') is not True:
        raise ValueError('native config destination paths are not verified')
    identity=validate_native_identity(observation.get('identity'))
    write_native_configs(configs,profile_id)
    archive,expected=make_native_config_archive(configs,identity)
    copy_native_config_archive(cid,archive)
    actual=json.loads(docker('exec',cid,'python3','-c',CONFIG_READBACK_PROBE,
                            json.dumps(expected,sort_keys=True,separators=(',',':'))).stdout)
    if actual!=expected:raise ValueError('native config readback does not match private host bytes/identity')
    return {'identity':identity,'identity_source':'actual docker exec default process',
            'host_config_sha256':{name:expected['files'][target]['sha256']for name,target in NATIVE_CONFIG_TARGETS.items()},
            'archive_sha256':hashlib.sha256(archive).hexdigest(),'readback':actual,
            'verified_before_native':True,'installation':'fixed-path daemon tar archive, no exec chown'}



FUTURE_PROFILE_ID='main-bash300-personal-memory-off-usage-observe-v3'
FUTURE_INIT_PROFILE_ID='main-bash300-personal-memory-off-usage-observe-init-v4'
RUNNER_PROFILE_IDS=(*PROFILE_IDS,FUTURE_PROFILE_ID,FUTURE_INIT_PROFILE_ID)


def resolve_runner_profile(profile_id,served_alias_manifest):
    if profile_id not in (FUTURE_PROFILE_ID,FUTURE_INIT_PROFILE_ID):
        profile=resolve_profile(profile_id)
        if served_alias_manifest is not None:raise ValueError('legacy strict profile cannot opt in through aliases')
        return profile|{'native_config_profile_id':profile_id},resolve_audit_policy(STRICT_POLICY),None
    if served_alias_manifest is None:raise ValueError('future profile requires its frozen served alias manifest')
    path=Path(served_alias_manifest)
    fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
    with os.fdopen(fd,'rb')as source:
        info=os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode)or info.st_nlink!=1 or info.st_size>65536:raise ValueError('alias manifest is not a bounded ordinary file')
        raw=source.read(65537)
    if len(raw)>65536:raise ValueError('alias manifest exceeds bound')
    document=json.loads(raw)
    audit=resolve_audit_policy(OBSERVE_POLICY,document)
    base='main-bash300-personal-memory-off-v2'
    return resolve_profile(base)|{'profile_id':profile_id,'native_config_profile_id':base,'task_docker_init_requested':profile_id==FUTURE_INIT_PROFILE_ID},audit,hashlib.sha256(raw).hexdigest()


def observe_task_docker_init(host_config,requested):
    """Observe actual task init only; no caller env or profile default is evidence."""
    actual=host_config.get('Init') if isinstance(host_config,dict) else None
    return {'requested':requested,'observed_init':actual if type(actual)is bool else None,
            'source':'docker_inspect_HostConfig.Init','before_container_start':True}


def run_trial(task,arm,out,probe_only=False,fixture_smoke=False,fixture_scenario="parallel",*,profile_id=PROFILE_ID,served_alias_manifest=None):
    profile,audit,alias_manifest_sha256=resolve_runner_profile(profile_id,served_alias_manifest)
    task_init_requested=profile.get('task_docker_init_requested',False)
    out.mkdir(parents=True,exist_ok=False)
    cfg=tomllib.loads((task/'task.toml').read_text())
    if audit['policy_id']==OBSERVE_POLICY and cfg['agent']['timeout_sec']!=3000:raise ValueError('future profile requires the fixed3000s task window')
    prompt=(task/'instruction.md').read_text()
    lock=json.loads((ROOT/'dataset-lock.json').read_text())
    entry=next(t for t in lock['tasks'] if t['task_id']==task.name)
    if sha(task/'instruction.md')!=entry['instruction_sha256'] or sha(task/'task.toml')!=entry['task_toml_sha256']:
        raise ValueError('frozen task asset hash mismatch')
    image,image_meta=resolve_locked_image(entry,cfg['environment']['docker_image'])
    image_id=image_meta['Id']
    relay_image=json.loads(docker('image','inspect','python:3.12-alpine').stdout)[0]['Id']
    token=uuid.uuid4().hex[:12];network='go-cli-eval-'+token
    task_cid=None;relay_cid=None
    record={'driver':'custom Docker driver using Harbor task assets','task_name':cfg['task']['name'],
            'profile_id':profile['profile_id'],'native_config_profile_id':profile['native_config_profile_id'],
            'audit_policy_id':audit['policy_id'],'audit_policy_fingerprint':audit['fingerprint'],
            'served_alias_manifest_sha256':alias_manifest_sha256,'go_automatic_personal_memory':False,
            'configured_go_bash_execution_window_seconds':profile['configured_go_bash_execution_window_seconds'],
            'go_bash_execution_window_source':'host_profile_declaration','go_supervisor_mode':profile['go_supervisor_mode'],
            'go_supervisor_mode_source':'host_profile_declaration',
            'go_supervisor_environment_validation':None,
            'task_docker_init_requested':task_init_requested,'task_docker_init_observation':None,
            'go_memory_setting_differs_from_product_default':True,
            'task_id':task.name,'arm':arm,'go_cli_revision':GO_REVISION,'pi_version':PI_VERSION,
            'model':MODEL,'reasoning_effort':'high','max_output_tokens':16384,
            'instruction_sha256':sha(task/'instruction.md'),'task_toml_sha256':sha(task/'task.toml'),
            'image_id':image_id,'image_ref':image,'relay_image_id':relay_image,'task_timeout_seconds':int(cfg['agent']['timeout_sec']),
            'cpus':1,'memory_bytes':4*1024**3,'storage_requested_mb':10240,'storage_hard_limit_enforced':False,
            'network':'internal-only task plus fixed-model relay on dual networks',
            'native_tools_preserved':True,'auxiliary_model_configured':False,'offline_fixture':fixture_smoke,'fixture_scenario':fixture_scenario if fixture_smoke else None,
            'compaction':'native default, not replaced or aligned','task_passed':None,
            'max_upstream_requests':80,'whole_task_token_cost_hard_limit_enforced':False,
            'unknown_usage_policy':('observe null/coverage/lower bound; native retries retain common budgets'if audit['policy_id']==OBSERVE_POLICY else'block all subsequent upstream calls; preserve missing usage as unknown')}
    (out/'agent').mkdir();(out/'wire').mkdir()
    try:
        docker('network','create','--internal',network)
        network_meta=json.loads(docker('network','inspect',network).stdout)[0]
        gateway_ip=network_meta['IPAM']['Config'][0]['Gateway']
        relay_cid=docker('create','--network','bridge','--name','eval-relay-'+token,
            '--cpus','0.25','--memory','256m','--pids-limit','64','--cap-drop','ALL',
            '--security-opt','no-new-privileges',
            '--env',f'EVAL_FIXTURE_SCENARIO={fixture_scenario if fixture_smoke else "disabled"}',
            '--mount',f'type=bind,src={ROOT / "audit_proxy.py"},dst=/proxy/audit_proxy.py,readonly',
            '--mount',f'type=bind,src={ROOT / "eval_core.py"},dst=/proxy/eval_core.py,readonly',
            '--mount',f'type=bind,src={ROOT / "offline_fixture.py"},dst=/proxy/offline_fixture.py,readonly',
            '--mount',f'type=bind,src={out / "wire"},dst=/wire',
            relay_image,'python','/proxy/audit_proxy.py','--upstream',('http://127.0.0.1:1'if fixture_smoke else'http://host.docker.internal:3425'),
            '--journal','/wire/wire.jsonl','--session','go-cli-eval-'+token,
            *(['--fixture'] if fixture_smoke else []),
            *(['--policy',OBSERVE_POLICY,'--served-alias-json',json.dumps({'aliases':audit['aliases'],'evidence_sha256':audit['evidence_sha256']},sort_keys=True,separators=(',',':'))]if audit['policy_id']==OBSERVE_POLICY else [])).stdout.strip()
        docker('network','connect','--alias','relay',network,relay_cid)
        docker('start',relay_cid)
        task_cid=docker('create',*(['--init']if task_init_requested else[]),'--platform','linux/amd64','--network',network,
            '--cpus','1','--memory','4g','--pids-limit','512','--cap-drop','ALL',
            '--security-opt','no-new-privileges','--workdir','/app',
            '--env','GO_CLI_PROVIDER_API_KEY=eval-local','--env','EVAL_GATEWAY_KEY=eval-local',
            *(['--env','GO_CLI_LINUX_SUBREAPER=0'] if profile['force_go_supervisor_off'] else []),
            '--env','PI_CODING_AGENT_DIR=/root/.pi/agent','--env','NO_COLOR=1',
            '--env','PI_TELEMETRY=0','--entrypoint','sleep',image_id,'infinity').stdout.strip()
        record['container_id']=task_cid
        if profile['force_go_supervisor_off']:
            inspected=json.loads(docker('inspect',task_cid).stdout)
            if not isinstance(inspected,list) or len(inspected)!=1 or not isinstance(inspected[0],dict) or not isinstance(inspected[0].get('Config'),dict):
                raise ValueError('container supervisor environment inspection is unknown')
            record['go_supervisor_environment_validation']=validate_go_supervisor_environment(inspected[0]['Config'].get('Env'))
            record['task_docker_init_observation']=observe_task_docker_init(inspected[0].get('HostConfig'),task_init_requested)
            if task_init_requested and record['task_docker_init_observation']['observed_init']is not True:
                raise ValueError('task Docker init must be observed true before start')
        docker('start',task_cid)
        if task_init_requested:
            if record['task_docker_init_observation']['observed_init']is not True:
                raise ValueError('trusted keeper capture requires observed task init')
            record['collector_keeper_binding']=json.loads(docker('exec',task_cid,'python3','-c',KEEPER_CAPTURE,task_cid).stdout)
            _validate_keeper_binding(record['collector_keeper_binding'],task_cid)
        record['network_probe']=json.loads(docker('exec',task_cid,'python3','-c',PROBE,gateway_ip,timeout=60).stdout)
        if record['network_probe']!={'relay':True,'github':False,'pypi':False,'npm':False,'host_gateway':False,'public_ip':False,'internal_host_gateway':False}:
            raise RuntimeError('network isolation gate failed')
        baseline=docker('exec',task_cid,'git','rev-parse','HEAD').stdout.strip()
        record['repository_base_commit']=baseline
        if probe_only:
            record['probe_only']=True;return
        payload=ROOT/'artifacts/payload'
        if not (payload/'go-cli').is_file() or not (payload/'node/bin/node').is_file() or not (payload/'pi/node_modules/@earendil-works/pi-coding-agent/package.json').is_file():
            raise RuntimeError('common Linux amd64 payload not prepared')
        payload_manifest=load_payload_manifest(payload)
        record['baseline_revision']=GO_REVISION
        record['go_cli_revision']=payload_manifest.get('go_cli_source_state',payload_manifest['go_cli_source_revision'])
        record['go_cli_source_patch_sha256']=payload_manifest.get('go_cli_source_patch_sha256')
        record['go_cli_binary_sha256']=payload_manifest['go_cli_sha256']
        record['pi_lock_sha256']=payload_manifest.get('pi_lock_sha256')
        record['node_version']=payload_manifest.get('node')
        record['payload_manifest_sha256']=sha(payload/'manifest.json')
        docker('exec',task_cid,'mkdir','-p','/opt/eval','/logs/agent')
        docker('cp',str(payload)+'/.',task_cid+':/opt/eval',timeout=180)
        configs=out/'configs'
        record['native_config_installation']=install_native_configs(task_cid,configs,profile_id=profile['native_config_profile_id'])
        record['config_sha256']=dict(record['native_config_installation']['host_config_sha256'])
        actual_argv=cli_argv(arm,prompt)
        record['argv_sha256']=hashlib.sha256(json.dumps(actual_argv).encode()).hexdigest()
        result=run_cli_once(task_cid,actual_argv,out,int(cfg['agent']['timeout_sec']))
        record.update(result)
        if audit['policy_id']==OBSERVE_POLICY:
            terminal=observe_native_terminal(arm,out/'stdout.jsonl',result.get('exit_code'))
            record.update(native_terminal=terminal,native_terminal_status=terminal['status'],
                          native_terminal_failure=terminal['status']in ('runtime_error','aborted','output_truncated'))
        if not result.get('outer_kill'):
            cleanup=docker('exec',task_cid,'python3','-c',COLLECT,baseline,
                *([json.dumps(record['collector_keeper_binding'],sort_keys=True),task_cid]if task_init_requested else[]),check=False,timeout=120)
            record['artifact_collection_exit_code']=cleanup.returncode
            (out/'artifact-collection.log').write_text(cleanup.stdout+cleanup.stderr)
        if result.get('outer_kill') or record.get('artifact_collection_exit_code')!=0:
            with (out/'timeout-app.tar').open('wb') as snapshot:
                capture=subprocess.run(['docker','cp',task_cid+':/app','-'],stdout=snapshot,stderr=subprocess.PIPE,timeout=180)
            record['fallback_snapshot_exit_code']=capture.returncode
            record['fallback_snapshot_sha256']=sha(out/'timeout-app.tar')
        docker('cp',task_cid+':/logs/agent/.',out/'agent',check=False,timeout=180)
        if (out/'agent/model.patch').is_file():record['patch_sha256']=sha(out/'agent/model.patch')
        diff=docker('diff',task_cid,check=False).stdout
        (out/'container-diff.txt').write_text(diff)
        record['container_diff_sha256']=sha(out/'container-diff.txt')
        record['container_diff_line_count']=len(diff.splitlines())
    except Exception as exc:
        record['infrastructure_error']=type(exc).__name__
        record['infrastructure_error_detail']=str(exc)
        raise
    finally:
        # Every resource here was created by this trial; unrelated containers stay untouched.
        if task_cid:docker('rm','-f',task_cid,check=False,timeout=30)
        if relay_cid:
            relaylog=docker('logs',relay_cid,check=False,timeout=15)
            (out/'relay.log').write_text(relaylog.stdout+relaylog.stderr)
            docker('rm','-f',relay_cid,check=False,timeout=30)
        docker('network','rm',network,check=False,timeout=30)
        wire=out/'wire/wire.jsonl'
        rows=[json.loads(line) for line in wire.read_text().splitlines()] if wire.exists() else []
        record=final_record(record,rows)
        dump(out/'result.json',record)
        print(out)


def main():
    p=argparse.ArgumentParser();p.add_argument('--task',required=True,type=Path)
    p.add_argument('--arm',choices=['go','pi'],required=True);p.add_argument('--out',required=True,type=Path)
    p.add_argument('--profile',choices=RUNNER_PROFILE_IDS,default=PROFILE_ID)
    p.add_argument('--served-alias-manifest',type=Path)
    p.add_argument('--probe-only',action='store_true');p.add_argument('--fixture-smoke',action='store_true');p.add_argument('--fixture-scenario',choices=['parallel','single-status','write-parallel','recover-once','fail-always'],default='parallel');args=p.parse_args()
    run_trial(args.task.resolve(),args.arm,args.out.resolve(),args.probe_only,args.fixture_smoke,args.fixture_scenario,profile_id=args.profile,served_alias_manifest=args.served_alias_manifest)

if __name__=='__main__':main()
