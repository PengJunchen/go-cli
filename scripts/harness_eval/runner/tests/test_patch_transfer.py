import asyncio,hashlib,importlib.util,json,os,pathlib,stat,tempfile,unittest
from types import SimpleNamespace
from unittest.mock import patch,Mock
from harbor.models.agent.context import AgentContext
import grade_trial as g
DATA=b'diff --git a/a b/a\r\n+synthetic-byte-\xff\n'
class PatchTransfer(unittest.TestCase):
 def test_both_umasks_private_staging_and_exact_bytes_canonical_unchanged(self):
  for mask in (0o022,0o077):
   with self.subTest(umask=oct(mask)),tempfile.TemporaryDirectory() as directory:
    source=pathlib.Path(directory)/'model.patch';source.write_bytes(DATA);source.chmod(0o600);seen=[]
    def docker(*args,**kwargs):
     self.assertEqual(args[0],'cp');p=pathlib.Path(args[1]);seen.append(p);self.assertEqual(p.read_bytes(),DATA);self.assertEqual(stat.S_IMODE(p.stat().st_mode),0o644);self.assertEqual(stat.S_IMODE(p.parent.stat().st_mode),0o700);self.assertEqual(args[2],'cid:/tmp/replay.patch');return SimpleNamespace(returncode=0,stdout='',stderr='')
    shim=g.DockerEnvironmentShim('cid',expected_patch_sha256=hashlib.sha256(DATA).hexdigest());old=os.umask(mask)
    try:
     with patch.object(g,'docker',side_effect=docker):asyncio.run(shim.upload_file(source,'/tmp/replay.patch'))
    finally:os.umask(old)
    self.assertEqual(source.read_bytes(),DATA);self.assertEqual(stat.S_IMODE(source.stat().st_mode),0o600);self.assertFalse(seen[0].exists());self.assertTrue(shim.patch_transport['source_unchanged'])
 def test_unknown_target_or_wrong_patch_hash_never_calls_docker(self):
  with tempfile.TemporaryDirectory() as directory:
   p=pathlib.Path(directory)/'model.patch';p.write_bytes(DATA);p.chmod(0o600)
   for target,sha in [('/tmp/other',hashlib.sha256(DATA).hexdigest()),('/tmp/replay.patch','0'*64)]:
    with self.subTest(target=target),patch.object(g,'docker') as docker:
     shim=g.DockerEnvironmentShim('cid',expected_patch_sha256=sha)
     with self.assertRaises(ValueError):asyncio.run(shim.upload_file(p,target))
     docker.assert_not_called()
 def test_copy_error_keeps_original_and_only_records_safe_type(self):
  with tempfile.TemporaryDirectory() as directory:
   p=pathlib.Path(directory)/'model.patch';p.write_bytes(DATA);p.chmod(0o600);shim=g.DockerEnvironmentShim('cid',expected_patch_sha256=hashlib.sha256(DATA).hexdigest())
   with patch.object(g,'docker',side_effect=RuntimeError('PRIVATE_ERROR_NOT_METADATA')):
    with self.assertRaises(RuntimeError):asyncio.run(shim.upload_file(p,'/tmp/replay.patch'))
   self.assertEqual(p.read_bytes(),DATA);self.assertEqual(stat.S_IMODE(p.stat().st_mode),0o600);self.assertNotIn('PRIVATE_ERROR_NOT_METADATA',json.dumps(shim.patch_transport));self.assertFalse(shim.patch_transport['copy_returned_success'])
 def test_pinned_official_agent_uses_unchanged_upload_protocol_with_fake_exec(self):
  with tempfile.TemporaryDirectory() as directory:
   root=pathlib.Path(directory);trial=root/'source-job/instance_synthetic';(trial/'agent').mkdir(parents=True);source=trial/'agent/model.patch';source.write_bytes(DATA);source.chmod(0o600);(trial/'result.json').write_text('{"task_name":"synthetic-task"}');(root/'config.json').write_text('{"task":{"path":"/synthetic-task"}}')
   replay=pathlib.Path(__file__).parent/'fixtures/swe_pro_patch_replay.py';self.assertEqual(hashlib.sha256(replay.read_bytes()).hexdigest(),'5c8b344de37164e9723f1724c2573013957fb644c3913a64c12c1718c737d876');spec=importlib.util.spec_from_file_location('pinned_official_replay',replay);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);agent=module.PatchReplayAgent(logs_dir=root/'agent',source_job=str(root/'source-job'),model_name='replay',skip_model_parsing=True);shim=g.DockerEnvironmentShim('cid',expected_patch_sha256=hashlib.sha256(DATA).hexdigest());calls=[]
   def fake_docker(*args,**kwargs):
    calls.append(args[0])
    if args[0]=='cp':self.assertEqual(pathlib.Path(args[1]).read_bytes(),DATA);self.assertEqual(stat.S_IMODE(pathlib.Path(args[1]).stat().st_mode),0o644)
    return SimpleNamespace(returncode=0,stdout='',stderr='')
   with patch.object(g,'docker',side_effect=fake_docker):asyncio.run(agent.run('',shim,AgentContext()))
   self.assertEqual(calls,['cp','exec','exec']);self.assertEqual(json.loads((root/'agent/replay.json').read_text())['apply_rc'],0);self.assertEqual(source.read_bytes(),DATA);self.assertEqual(stat.S_IMODE(source.stat().st_mode),0o600)
if __name__=='__main__':unittest.main()
