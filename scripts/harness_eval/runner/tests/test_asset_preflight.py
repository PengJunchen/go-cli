import importlib.util
import contextlib
import io
from types import SimpleNamespace
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[1] / 'scripts/asset_preflight.py'
spec = importlib.util.spec_from_file_location('asset_preflight_contract', SCRIPT)
asset_preflight = importlib.util.module_from_spec(spec)
spec.loader.exec_module(asset_preflight)

class StopAfterCreate(Exception):
    pass

class ControlProfileContract(unittest.TestCase):
    def test_control_uses_the_clean_grader_container_profile(self):
        task = 'instance_navidrome__navidrome-1e96b858a91c640fe64e84c5e5ad8cc0954ea38d'
        with tempfile.TemporaryDirectory() as temporary, patch.object(asset_preflight, 'OUT', Path(temporary)), patch.object(asset_preflight, 'call', side_effect=StopAfterCreate) as create:
            with self.assertRaises(StopAfterCreate):
                asset_preflight.control(task, 'empty')
            args = create.call_args.args[0]
        self.assertEqual(args[:2], ['docker', 'create'])
        for flag, value in [('--platform', 'linux/amd64'), ('--network', 'bridge'), ('--cpus', '1'),
                            ('--memory', '4g'), ('--pids-limit', '512'), ('--cap-drop', 'ALL'),
                            ('--security-opt', 'no-new-privileges'), ('--workdir', '/app'),
                            ('--entrypoint', 'sleep')]:
            with self.subTest(flag=flag):
                self.assertIn(flag, args)
                self.assertEqual(args[args.index(flag)+1], value)
        self.assertEqual(args[-2:], [asset_preflight.image(task), 'infinity'])
        self.assertNotIn('--privileged', args)
        self.assertNotIn('--cap-add', args)

    def test_verifier_timeout_matches_clean_grader(self):
        task = 'instance_navidrome__navidrome-1e96b858a91c640fe64e84c5e5ad8cc0954ea38d'
        def run(args, **kwargs):
            stdout = '0' if args[:3] == ['docker', 'exec', 'fixture-container'] and 'cat' in args else '{}'
            return SimpleNamespace(returncode=0, stdout=stdout, stderr='')
        with tempfile.TemporaryDirectory() as temporary, patch.object(asset_preflight, 'OUT', Path(temporary)), patch.object(asset_preflight, 'call', return_value=SimpleNamespace(stdout='fixture-container')), patch.object(asset_preflight.subprocess, 'run', side_effect=run) as invoke, contextlib.redirect_stdout(io.StringIO()):
            asset_preflight.control(task, 'empty')
            verifier = next(c for c in invoke.call_args_list if '/tests/test.sh' in c.args[0])
        self.assertEqual(verifier.args[0], ['docker', 'exec', 'fixture-container', 'timeout', '--signal=TERM', '--kill-after=15s', '3000s', '/bin/bash', '/tests/test.sh'])
        self.assertEqual(verifier.kwargs['timeout'], 3045)

if __name__ == '__main__':
    unittest.main()
