"""Pure installation contracts: no Docker/native command is executed."""
import copy
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import run_trial
from eval_core import PROFILE_ID


IDENTITY={'uid':0,'gid':0,'euid':0,'egid':0}


class NativeConfigInstallContracts(unittest.TestCase):
    def fake_install(self,root,*,identity=None,mutate=None,copy_error=False):
        identity=dict(IDENTITY if identity is None else identity)
        calls=[];archives=[]
        def docker(*args,**kwargs):
            calls.append(args)
            self.assertEqual(args[:4],('exec','owned','python3','-c'))
            self.assertNotIn('--user',args)
            if args[4]==run_trial.CONFIG_IDENTITY_PROBE:
                value={'identity':identity,'paths_safe':True}
            else:
                self.assertEqual(args[4],run_trial.CONFIG_READBACK_PROBE)
                value=json.loads(args[5])
                if mutate:mutate(value)
            return SimpleNamespace(stdout=json.dumps(value),returncode=0)
        def copy_archive(cid,data):
            self.assertEqual(cid,'owned');archives.append(data)
            if copy_error:raise subprocess.CalledProcessError(1,['docker','cp'])
        with patch.object(run_trial,'docker',side_effect=docker),patch.object(run_trial,'copy_native_config_archive',side_effect=copy_archive):
            result=run_trial.install_native_configs('owned',root,profile_id=PROFILE_ID)
        return result,calls,archives

    def test_host_private_bytes_and_tar_owner_modes_are_umask_independent(self):
        for mask in (0o022,0o077):
            with self.subTest(umask=mask),tempfile.TemporaryDirectory() as temp:
                old=os.umask(mask)
                try:result,calls,archives=self.fake_install(Path(temp)/'configs')
                finally:os.umask(old)
                root=Path(temp)/'configs'
                self.assertEqual(stat.S_IMODE(root.stat().st_mode),0o700)
                self.assertEqual(set(p.name for p in root.iterdir()),set(run_trial.NATIVE_CONFIG_TARGETS))
                self.assertTrue(result['verified_before_native'])
                self.assertEqual(len(calls),2)
                self.assertEqual(len(archives),1)
                with tarfile.open(fileobj=io.BytesIO(archives[0]),mode='r:') as tar:
                    rows=tar.getmembers()
                    self.assertEqual({r.name for r in rows},set(run_trial.NATIVE_CONFIG_DIRS)|{p.lstrip('/') for p in run_trial.NATIVE_CONFIG_TARGETS.values()})
                    for row in rows:
                        self.assertEqual((row.uid,row.gid),(0,0))
                        self.assertEqual(row.uname,'');self.assertEqual(row.gname,'')
                        self.assertFalse(row.issym() or row.islnk())
                        self.assertEqual(row.mode,0o700 if row.isdir() else 0o600)
                        if row.isfile():
                            host=next(root/name for name,path in run_trial.NATIVE_CONFIG_TARGETS.items() if path.lstrip('/')==row.name)
                            self.assertEqual(tar.extractfile(row).read(),host.read_bytes())
                            self.assertEqual(stat.S_IMODE(host.stat().st_mode),0o600)
                            self.assertEqual(hashlib.sha256(host.read_bytes()).hexdigest(),result['host_config_sha256'][host.name])

    def test_numeric_owner_is_actual_default_identity_not_host_501_20(self):
        with tempfile.TemporaryDirectory() as temp:
            result,_,archives=self.fake_install(Path(temp)/'configs',identity={'uid':1001,'gid':1002,'euid':1001,'egid':1002})
            with tarfile.open(fileobj=io.BytesIO(archives[0]),mode='r:') as tar:
                self.assertTrue(all((r.uid,r.gid)==(1001,1002)for r in tar.getmembers()))
            self.assertEqual(result['identity']['uid'],1001)

    def test_actual_readback_permission_owner_mode_bytes_and_copy_fail_closed(self):
        for change in ('uid','gid','mode','sha256','size','identity','missing','extra'):
            with self.subTest(change=change),tempfile.TemporaryDirectory() as temp:
                def mutate(value):
                    first=next(iter(value['files']))
                    if change=='identity':value['identity']['euid']=501
                    elif change=='missing':del value['files'][first]
                    elif change=='extra':value['files']['/unknown']={}
                    else:value['files'][first][change]=(501 if change in ('uid','gid') else 0) if change!='sha256' else '0'*64
                with self.assertRaises(ValueError):self.fake_install(Path(temp)/'configs',mutate=mutate)
        with tempfile.TemporaryDirectory() as temp:
            with self.assertRaises(subprocess.CalledProcessError):self.fake_install(Path(temp)/'configs',copy_error=True)
        with tempfile.TemporaryDirectory() as temp:
            def unreadable(value):raise PermissionError('synthetic EACCES')
            with self.assertRaises(PermissionError):self.fake_install(Path(temp)/'configs',mutate=unreadable)

    def test_unknown_identity_rejected_before_host_write_or_archive_copy(self):
        for value in ({'uid':0,'gid':0,'euid':501,'egid':0},{'uid':True,'gid':0,'euid':True,'egid':0},{'uid':-1,'gid':0,'euid':-1,'egid':0},{}):
            with self.subTest(value=value),tempfile.TemporaryDirectory() as temp:
                root=Path(temp)/'configs'
                with self.assertRaises(ValueError),patch.object(run_trial,'copy_native_config_archive') as archive:
                    self.fake_install(root,identity=value)
                self.assertFalse(root.exists())
                archive.assert_not_called()

    def test_archive_rejects_unknown_files_symlink_and_hardlink(self):
        for kind in ('unknown','symlink','hardlink','mode','directory-mode'):
            with self.subTest(kind=kind),tempfile.TemporaryDirectory() as temp:
                root=Path(temp)/'configs';run_trial.write_native_configs(root,PROFILE_ID)
                p=root/'go.json'
                if kind=='unknown':(root/'extra').write_bytes(b'x')
                if kind=='symlink':p.unlink();p.symlink_to(root/'models.json')
                if kind=='hardlink':p.unlink();os.link(root/'models.json',p)
                if kind=='mode':p.chmod(0o644)
                if kind=='directory-mode':root.chmod(0o755)
                with self.assertRaises((ValueError,OSError)):run_trial.make_native_config_archive(root,IDENTITY)

    def test_real_readback_probe_opens_exact_files_and_rejects_eacces_or_wrong_bytes(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp)/'configs';run_trial.write_native_configs(root,PROFILE_ID)
            _,expected=run_trial.make_native_config_archive(root,IDENTITY)
            bytes_by_path={target:(root/name).read_bytes()for name,target in run_trial.NATIVE_CONFIG_TARGETS.items()}
            for scenario in ('good','eacces','wrong_bytes','wrong_owner','symlink_parent','wrong_mode'):
                with self.subTest(scenario=scenario):
                    opened=[];chunks={};handles={}
                    def lstat(path):
                        symlink=scenario=='symlink_parent' and path=='/root/.pi'
                        return SimpleNamespace(st_mode=(stat.S_IFLNK if symlink else stat.S_IFDIR)|0o700,st_uid=0,st_gid=0)
                    def open_file(path,flags):
                        self.assertEqual(flags,os.O_RDONLY|os.O_NOFOLLOW)
                        if scenario=='eacces':raise PermissionError('synthetic EACCES')
                        fd=len(opened)+100;opened.append(path);handles[fd]=path
                        data=bytes_by_path[path]
                        chunks[fd]=[(b'wrong' if scenario=='wrong_bytes' else data),b'']
                        return fd
                    def fstat(fd):
                        return SimpleNamespace(st_mode=stat.S_IFREG|(0o644 if scenario=='wrong_mode' else 0o600),st_nlink=1,st_uid=501 if scenario=='wrong_owner' else 0,st_gid=0)
                    with patch('sys.argv',['probe',json.dumps(expected)]),patch('os.getuid',return_value=0),patch('os.getgid',return_value=0),patch('os.geteuid',return_value=0),patch('os.getegid',return_value=0),patch('os.lstat',side_effect=lstat),patch('os.open',side_effect=open_file),patch('os.fstat',side_effect=fstat),patch('os.read',side_effect=lambda fd,n:chunks[fd].pop(0)),patch('os.close') as close,contextlib.redirect_stdout(io.StringIO()) as output:
                        if scenario=='good':
                            exec(run_trial.CONFIG_READBACK_PROBE,{})
                            self.assertEqual(json.loads(output.getvalue()),expected)
                            self.assertEqual(opened,list(run_trial.NATIVE_CONFIG_TARGETS.values()))
                            self.assertEqual(close.call_count,3)
                        else:
                            with self.assertRaises((RuntimeError,PermissionError)):
                                exec(run_trial.CONFIG_READBACK_PROBE,{})

    def test_real_preflight_probe_rejects_existing_configs_links_and_uncontrolled_dirs(self):
        for scenario in ('good','symlink','wrong_owner','world_writable','existing_file'):
            with self.subTest(scenario=scenario):
                def lstat(path):
                    return SimpleNamespace(st_mode=(stat.S_IFLNK if scenario=='symlink' and path=='/root/.pi' else stat.S_IFDIR)|(0o777 if scenario=='world_writable' else 0o755),st_uid=501 if scenario=='wrong_owner' else 0,st_gid=0)
                with patch('os.getuid',return_value=0),patch('os.getgid',return_value=0),patch('os.geteuid',return_value=0),patch('os.getegid',return_value=0),patch('os.lstat',side_effect=lstat),patch('os.path.lexists',return_value=scenario=='existing_file'),patch('os.access',return_value=True),contextlib.redirect_stdout(io.StringIO()) as output:
                    if scenario=='good':
                        exec(run_trial.CONFIG_IDENTITY_PROBE,{})
                        self.assertEqual(json.loads(output.getvalue()),{'identity':IDENTITY,'paths_safe':True})
                    else:
                        with self.assertRaises(RuntimeError):exec(run_trial.CONFIG_IDENTITY_PROBE,{})

    def test_daemon_cp_uses_stdin_archive_without_exec_chown_or_cap_change(self):
        with patch.object(run_trial.subprocess,'run',return_value=SimpleNamespace(returncode=0)) as execute:
            run_trial.copy_native_config_archive('owned',b'archive')
        args=execute.call_args
        self.assertEqual(args.args[0],['docker','cp','-a','-','owned:/'])
        self.assertEqual(args.kwargs['input'],b'archive')
        self.assertTrue(args.kwargs['check'])

    def test_failed_installation_is_setup_unknown_and_driver_never_calls_native(self):
        from test_profiles import ProfileDriverContract,NEW_PROFILE
        helper=ProfileDriverContract(methodName='test_old_profile_has_no_new_environment_override_or_inspect')
        try:
            with patch.object(run_trial,'install_native_configs',side_effect=ValueError('synthetic config readback gate')):
                _,result,_,execute,error=helper.run_fake_trial('go',NEW_PROFILE)
            self.assertIsNotNone(error)
            execute.assert_not_called()
            self.assertEqual(result['infrastructure_error'],'ValueError')
            self.assertIsNone(result['task_passed'])
        finally:helper.doCleanups()

    def test_driver_guards_installation_before_native_invocation_and_caps_stay_dropped(self):
        import inspect
        source=inspect.getsource(run_trial.run_trial)
        self.assertLess(source.index('install_native_configs('),source.index('run_cli_once('))
        self.assertNotIn('chown',source)
        self.assertIn("'--cap-drop','ALL'",source)
        self.assertNotIn('--cap-add',source)
