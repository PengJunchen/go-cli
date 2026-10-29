"""Independent, local-only reviewer checks. Never contact a model endpoint."""
import http.client
import hashlib
import asyncio
import importlib.util
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
import urllib.error

import test_contract
from eval_core import MODEL, OUTPUT_LIMIT, usage_totals
from run_trial import ROOT


class ReviewerContracts(test_contract.ProxyContracts):
    def good(self):
        return {'model': MODEL, 'reasoning_effort': 'high',
                'max_tokens': OUTPUT_LIMIT,
                'messages': [{'role': 'user', 'content': 'offline fixture'}]}

    def test_n_cannot_amplify_single_request_budget(self):
        with self.assertRaises(urllib.error.HTTPError):
            self.ask(self.good() | {'n': 2})

    def test_request_cap_rejects_before_next_upstream_call(self):
        self.proxy.max_requests = 1
        self.ask(self.good())
        with self.assertRaises(urllib.error.HTTPError):
            self.ask(self.good())
        self.assertEqual(len(test_contract.Upstream.calls), 1)

    def test_native_smaller_output_limit_is_preserved(self):
        body = self.good() | {'max_tokens': 1024}
        self.ask(body)
        self.assertEqual(test_contract.Upstream.calls[-1]['body'], body)

    def test_non_object_json_rejected_as_contract_error(self):
        for body in ([], None, 'text'):
            with self.subTest(body_type=type(body).__name__):
                with self.assertRaises(urllib.error.HTTPError):
                    self.ask(body)

    def test_malformed_tools_rejected_as_contract_error(self):
        for tools in ('read', [1], [{'function': None}]):
            with self.subTest(tools=tools):
                with self.assertRaises(urllib.error.HTTPError):
                    self.ask(self.good() | {'tools': tools})

    def test_connect_is_forbidden(self):
        conn = http.client.HTTPConnection('127.0.0.1', self.proxy.server_port,
                                          timeout=5)
        try:
            conn.request('CONNECT', 'example.com:443')
            response = conn.getresponse()
            self.assertEqual(response.status, 405)
            response.read()
        finally:
            conn.close()

    def test_no_arbitrary_proxy_path_or_upstream_override(self):
        for path in ('/v1/chat/completions?url=http://example.com',
                     '/v1/chat/completions/../models',
                     '/https://example.com'):
            with self.subTest(path=path):
                with self.assertRaises(urllib.error.HTTPError):
                    self.ask(self.good(), path)
        for key in ('base_url', 'url', 'upstream', 'api_key'):
            with self.subTest(key=key):
                with self.assertRaises(urllib.error.HTTPError):
                    self.ask(self.good() | {key: 'untrusted'})


class ReviewerUsage(unittest.TestCase):
    def test_absent_usage_details_are_distinguishable_from_reported_zero(self):
        unknown = usage_totals([{'phase': 'finish', 'usage': {
            'prompt_tokens': 4, 'completion_tokens': 2}}])
        zero = usage_totals([{'phase': 'finish', 'usage': {
            'prompt_tokens': 4, 'completion_tokens': 2,
            'prompt_tokens_details': {'cached_tokens': 0},
            'completion_tokens_details': {'reasoning_tokens': 0}}}])
        self.assertNotEqual(unknown, zero,
                            'Missing cache/reasoning details must remain unknown')


class ReviewerOfficialReplay(unittest.TestCase):
    def test_pinned_official_agent_loads_and_finds_captured_patch(self):
        from harbor.models.agent.context import AgentContext

        class FakeEnvironment:
            def __init__(self):
                self.uploads = []
                self.commands = []

            async def upload_file(self, source_path, target_path):
                self.uploads.append((Path(source_path), target_path))

            async def exec(self, command, **kwargs):
                self.commands.append(command)
                return SimpleNamespace(return_code=0, stdout='', stderr='')

        official = Path(__file__).parent / 'fixtures/swe_pro_patch_replay.py'
        self.assertEqual(hashlib.sha256(official.read_bytes()).hexdigest(), '5c8b344de37164e9723f1724c2573013957fb644c3913a64c12c1718c737d876')
        spec = importlib.util.spec_from_file_location('reviewer_official_replay', official)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            trial = root / 'source-job/instance_fixture'
            (trial / 'agent').mkdir(parents=True)
            patch = trial / 'agent/model.patch'
            patch.write_text('offline contract artifact; never applied or executed\n')
            (trial / 'result.json').write_text(json.dumps({'task_name': 'benchmark/frozen-task'}))
            out = root / 'grade'
            out.mkdir()
            (out / 'config.json').write_text(json.dumps({'task': {'path': '/frozen/frozen-task'}}))
            agent = module.PatchReplayAgent(logs_dir=out / 'agent', source_job=str(root / 'source-job'),
                                           model_name='replay', skip_model_parsing=True)
            env = FakeEnvironment()
            asyncio.run(agent.run('', env, AgentContext()))
            replay = json.loads((out / 'agent/replay.json').read_text())
            self.assertEqual(replay['apply_rc'], 0)
            self.assertEqual(env.uploads, [(patch, '/tmp/replay.patch')])
            self.assertEqual(len(env.commands), 2)
            self.assertIn('git apply', env.commands[0])


if __name__ == '__main__':
    unittest.main()
