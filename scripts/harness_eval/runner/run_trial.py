"""One frozen CLI attempt in an internal-only task container. No agent retries."""
import argparse
import hashlib
import inspect
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import time
import tomllib
import uuid
from eval_core import MODEL,GO_REVISION,PROFILE_ID,PI_VERSION,go_config,pi_config,cli_argv,usage_totals

ROOT=Path(__file__).resolve().parent

def dump(path,value):
    path.parent.mkdir(parents=True,exist_ok=True)
    path.write_text(json.dumps(value,indent=2,ensure_ascii=False)+'\n')

def sha(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()

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
    return result|{'task_passed':None,'usage':totals,'open_attempts':len(opened),
                   'wire_contract_rejections':len(reasons),'stop_class':stop_class}

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


# Embed the same pure helper exercised by fake-procfs tests. Do not execute
# this signal-sending program on the host; it is for our owned container only.
COLLECT='import json,os,signal,subprocess,sys,time\n'+inspect.getsource(snapshot_processes)+r'''
baseline=sys.argv[1]
# Only this container's PID namespace. Stop then kill every leftover agent/tool
# process before reading artifacts; PID 1 and this trusted collector survive.
before=snapshot_processes('/proc',[1,os.getpid()])
os.makedirs('/logs/agent',exist_ok=True)
with open('/logs/agent/processes-before-cleanup.json','w') as output:
 json.dump(before,output)
 output.flush()
 os.fsync(output.fileno())
if before['enumeration_status']!='observed':
 raise RuntimeError('process enumeration unknown; pre-cleanup evidence retained')
others=[row['pid'] for row in before['processes']]
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


def run_trial(task,arm,out,probe_only=False,fixture_smoke=False,fixture_scenario="parallel"):
    out.mkdir(parents=True,exist_ok=False)
    cfg=tomllib.loads((task/'task.toml').read_text())
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
            'profile_id':PROFILE_ID,'go_automatic_personal_memory':False,
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
            'unknown_usage_policy':'block all subsequent upstream calls; preserve missing usage as unknown'}
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
            relay_image,'python','/proxy/audit_proxy.py','--upstream','http://host.docker.internal:3425',
            '--journal','/wire/wire.jsonl','--session','go-cli-eval-'+token,
            *(['--fixture'] if fixture_smoke else [])).stdout.strip()
        docker('network','connect','--alias','relay',network,relay_cid)
        docker('start',relay_cid)
        task_cid=docker('create','--platform','linux/amd64','--network',network,
            '--cpus','1','--memory','4g','--pids-limit','512','--cap-drop','ALL',
            '--security-opt','no-new-privileges','--workdir','/app',
            '--env','GO_CLI_PROVIDER_API_KEY=eval-local','--env','EVAL_GATEWAY_KEY=eval-local',
            '--env','PI_CODING_AGENT_DIR=/root/.pi/agent','--env','NO_COLOR=1',
            '--env','PI_TELEMETRY=0','--entrypoint','sleep',image_id,'infinity').stdout.strip()
        record['container_id']=task_cid
        docker('start',task_cid)
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
        docker('exec',task_cid,'mkdir','-p','/opt/eval','/logs/agent','/root/.go-cli','/root/.pi/agent')
        docker('cp',str(payload)+'/.',task_cid+':/opt/eval',timeout=180)
        configs=out/'configs'
        dump(configs/'go.json',go_config('http://relay:8000/v1'))
        dump(configs/'models.json',pi_config('http://relay:8000/v1'))
        dump(configs/'pi-settings.json',{'cacheWarming':'off','telemetry':False})
        for source,destination in [('go.json','/root/.go-cli/config.json'),('models.json','/root/.pi/agent/models.json'),('pi-settings.json','/root/.pi/agent/settings.json')]:
            docker('cp',configs/source,task_cid+':'+destination)
        record['config_sha256']={p.name:sha(p) for p in configs.iterdir()}
        actual_argv=cli_argv(arm,prompt)
        record['argv_sha256']=hashlib.sha256(json.dumps(actual_argv).encode()).hexdigest()
        result=run_cli_once(task_cid,actual_argv,out,int(cfg['agent']['timeout_sec']))
        record.update(result)
        if not result.get('outer_kill'):
            cleanup=docker('exec',task_cid,'python3','-c',COLLECT,baseline,check=False,timeout=120)
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
    p.add_argument('--probe-only',action='store_true');p.add_argument('--fixture-smoke',action='store_true');p.add_argument('--fixture-scenario',choices=['parallel','single-status','write-parallel'],default='parallel');args=p.parse_args()
    run_trial(args.task.resolve(),args.arm,args.out.resolve(),args.probe_only,args.fixture_smoke,args.fixture_scenario)

if __name__=='__main__':main()
