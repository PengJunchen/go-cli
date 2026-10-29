"""Clean replay using the pinned dataset's official PatchReplayAgent and tests."""
import argparse
import hashlib
import os
import re
import stat
import tempfile
import asyncio
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import time
import tomllib
import uuid
from types import SimpleNamespace
from run_trial import ROOT,docker,dump,sha,resolve_recorded_image


def grade_inputs(task,source):
    return {'patch':source/'agent/model.patch','tests':task/'tests',
            'reuse_agent_container':False,'timeout_seconds':3000}


class DockerEnvironmentShim:
    """Only the upload/exec protocol used by official PatchReplayAgent."""
    def __init__(self,cid,*,expected_patch_sha256):
        if not isinstance(expected_patch_sha256,str) or not re.fullmatch('[0-9a-f]{64}',expected_patch_sha256):raise ValueError('missing exact source patch hash')
        self.cid=cid;self.expected_patch_sha256=expected_patch_sha256;self.patch_transport=None
    @staticmethod
    def patch_bytes(path):
        fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)
        with os.fdopen(fd,'rb') as stream:
            info=os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_nlink!=1:raise ValueError('replay patch is not an independent regular file')
            data=stream.read()
        return data,{'device':info.st_dev,'inode':info.st_ino,'uid':info.st_uid,'gid':info.st_gid,'mode':stat.S_IMODE(info.st_mode),'bytes':len(data),'sha256':hashlib.sha256(data).hexdigest()}
    async def upload_file(self,source_path,target_path):
        if str(target_path)!='/tmp/replay.patch':raise ValueError('only fixed official replay patch upload is supported')
        source=Path(source_path);data,before=self.patch_bytes(source)
        if before['sha256']!=self.expected_patch_sha256:raise ValueError('official upload source patch hash changed')
        self.patch_transport={'source_before':before,'source_unchanged':None,'copy_returned_success':False,'error_type':None}
        primary=None
        try:
            with tempfile.TemporaryDirectory(prefix='swe-replay-patch-') as directory:
                if stat.S_IMODE(Path(directory).stat().st_mode)!=0o700:raise ValueError('replay transport staging is not private')
                copy=Path(directory)/'replay.patch';fd=os.open(copy,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
                with os.fdopen(fd,'wb') as stream:stream.write(data);stream.flush();os.fsync(stream.fileno());os.fchmod(stream.fileno(),0o644)
                if copy.read_bytes()!=data or stat.S_IMODE(copy.stat().st_mode)!=0o644:raise ValueError('replay transport byte or mode mismatch')
                self.patch_transport.update(private_parent_mode=0o700,transport_mode=0o644,transport_sha256=before['sha256'],target='/tmp/replay.patch')
                await asyncio.to_thread(docker,'cp',copy,self.cid+':/tmp/replay.patch')
                self.patch_transport['copy_returned_success']=True
        except Exception as error:
            primary=error;self.patch_transport['error_type']=type(error).__name__;raise
        finally:
            try:
                _,after=self.patch_bytes(source);self.patch_transport['source_after']=after;self.patch_transport['source_unchanged']=after==before
            except Exception as error:self.patch_transport['source_after_unknown_type']=type(error).__name__
            if self.patch_transport['source_unchanged']is not True and primary is None:raise ValueError('official source patch changed during transport')
    async def exec(self,command,timeout_sec=300,**kwargs):
        result=await asyncio.to_thread(docker,'exec',self.cid,'/bin/bash','-c',command,
                                      check=False,timeout=timeout_sec+10)
        return SimpleNamespace(return_code=result.returncode,stdout=result.stdout,stderr=result.stderr)


def grade(task,source,out):
    out.mkdir(parents=True,exist_ok=False)
    cfg=tomllib.loads((task/'task.toml').read_text())
    source_result=json.loads((source/'result.json').read_text())
    if source_result.get('task_id')!=task.name:raise ValueError('source task identity mismatch')
    patch=source/'agent/model.patch'
    if not patch.is_file():raise ValueError('missing patch: this trial cannot be graded as an empty patch')
    if source_result.get('patch_sha256')!=sha(patch):raise ValueError('source patch identity mismatch')
    image_meta=resolve_recorded_image(source_result['image_id'])
    if image_meta['Id']!=source_result['image_id']:raise ValueError('source and grading images differ')
    source_job=out/'source-job';trial=source_job/('instance_'+uuid.uuid4().hex[:8]);(trial/'agent').mkdir(parents=True)
    shutil.copyfile(patch,trial/'agent/model.patch')
    dump(trial/'result.json',{'task_name':cfg['task']['name']})
    dump(out/'config.json',{'task':{'path':str(task)}})
    replay_path=ROOT/'third_party/swe-bench-pro/v2/tooling/patch_replay.py'
    spec=importlib.util.spec_from_file_location('official_patch_replay',replay_path)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    from harbor.models.agent.context import AgentContext
    agent=module.PatchReplayAgent(logs_dir=out/'agent',source_job=str(source_job),model_name='replay',skip_model_parsing=True)
    cid=None
    shim=None
    record={'task_id':task.name,'source_trial':str(source),'source_patch_sha256':sha(patch),
            'image_id':image_meta['Id'],'official_replay_sha256':sha(replay_path),
            'verifier_sha256':{p.name:sha(p) for p in (task/'tests').iterdir() if p.is_file()},
            'clean_image':True,'only_model_patch_replayed':True,'verifier_network':'public as official task.toml',
            'score':None,'verifier_timeout_seconds':3000,'cpus':1,'memory_bytes':4*1024**3}
    try:
        cid=docker('create','--platform','linux/amd64','--network','bridge','--cpus','1','--memory','4g',
                   '--pids-limit','512','--cap-drop','ALL','--security-opt','no-new-privileges',
                   '--workdir','/app','--entrypoint','sleep',image_meta['Id'],'infinity').stdout.strip()
        docker('start',cid)
        shim=DockerEnvironmentShim(cid,expected_patch_sha256=record['source_patch_sha256'])
        asyncio.run(agent.run('',shim,AgentContext()))
        replay=json.loads((out/'agent/replay.json').read_text())
        record['replay']=replay
        if replay.get('apply_rc',0)!=0:
            record['ungradable_reason']='official patch replay apply failed';return
        docker('cp',str(task/'tests')+'/.',cid+':/tests',timeout=120)
        docker('exec',cid,'mkdir','-p','/logs/verifier')
        started=time.monotonic()
        with (out/'verifier.stdout').open('wb') as stdout,(out/'verifier.stderr').open('wb') as stderr:
            result=subprocess.run(['docker','exec',cid,'timeout','--signal=TERM','--kill-after=15s',
                                   '3000s','/bin/bash','/tests/test.sh'],stdout=stdout,stderr=stderr,timeout=3045)
        record['verifier_exit_code']=result.returncode;record['elapsed_seconds']=time.monotonic()-started
        docker('cp',cid+':/logs/verifier',out/'verifier',check=False,timeout=120)
        reward=out/'verifier/reward.txt'
        if reward.is_file():
            value=reward.read_text().strip()
            if value in ('0','1'):record['score']=int(value)
            else:record['ungradable_reason']='invalid reward file'
        else:record['ungradable_reason']='reward file missing'
    except Exception as exc:
        record['infrastructure_error']=type(exc).__name__;record['infrastructure_error_detail']=str(exc)
        raise
    finally:
        if shim is not None:record['patch_transport']=shim.patch_transport
        if cid:docker('rm','-f',cid,check=False,timeout=30)
        dump(out/'grade.json',record)
        print(out)


def main():
    p=argparse.ArgumentParser();p.add_argument('--task',type=Path,required=True)
    p.add_argument('--source',type=Path,required=True);p.add_argument('--out',type=Path,required=True)
    args=p.parse_args();grade(args.task.resolve(),args.source.resolve(),args.out.resolve())

if __name__=='__main__':main()
