import copy
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest
import urllib.request
from unittest.mock import patch

from audit_proxy import AuditServer, observe_tool_metadata
from eval_core import MODEL


def bash(description='The configured host execution window is 30s;'):
    return {'type': 'function', 'function': {'name': 'bash', 'description': description, 'parameters': {
        'type': 'object', 'properties': {'command': {'type': 'string'}},
        'required': ['command'], 'additionalProperties': False}}}


class ToolMetadataContract(unittest.TestCase):
    def test_actual_hashes_change_with_real_description_and_schema_without_mutation(self):
        first, second = bash('The configured host execution window is 30s; 工具说明'), bash('The configured host execution window is 5m0s;')
        first['function']['parameters']['properties']['command']['description'] = '命令输入'
        second['function']['parameters']['properties']['timeout'] = {'type': 'number'}
        tools = [first, second]
        original = copy.deepcopy(tools)
        rows = observe_tool_metadata(tools)
        self.assertEqual(tools, original)
        self.assertEqual([row['index'] for row in rows], [0, 1])
        self.assertEqual([row['name'] for row in rows], ['bash', 'bash'])
        self.assertEqual([row['go_host_window_claim'] for row in rows], ['30s', '5m0s'])
        self.assertEqual([row['command_only_schema'] for row in rows], [True, False])
        for tool, row in zip(tools, rows):
            function = tool['function']
            self.assertEqual(row['description_sha256'], hashlib.sha256(function['description'].encode('utf-8')).hexdigest())
            canonical = json.dumps(function['parameters'], sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode('utf-8')
            self.assertEqual(row['parameters_sha256'], hashlib.sha256(canonical).hexdigest())
        self.assertNotEqual(rows[0]['description_sha256'], rows[1]['description_sha256'])
        self.assertNotEqual(rows[0]['parameters_sha256'], rows[1]['parameters_sha256'])

    def test_unknowns_and_pi_are_not_filled_with_go_defaults(self):
        pi = bash('Execute a command. Optional timeout in seconds, default 30 seconds.')
        pi['function']['parameters'] = {'type': 'object', 'properties': {'command': {'type': 'string'}, 'timeout': {'type': 'number'}}}
        missing = {'function': {'name': 'bash', 'description': None, 'parameters': 'not-an-object'}}
        conflict = bash('The configured host execution window is 30s; The configured host execution window is 5m0s;')
        other = bash(); other['function']['name'] = 'other'
        unknown_window = bash('The configured host execution window is 300s;')
        rows = observe_tool_metadata([pi, missing, conflict, other, unknown_window])
        self.assertTrue(all(row['go_host_window_claim'] is None for row in rows))
        self.assertFalse(rows[0]['command_only_schema'])
        self.assertIsNone(rows[1]['description_sha256'])
        self.assertIsNone(rows[1]['parameters_sha256'])
        self.assertIsNone(rows[1]['command_only_schema'])
        self.assertEqual(observe_tool_metadata(None), [])
        for nonfinite in (float('nan'), float('inf'), float('-inf')):
            invalid = bash()
            invalid['function']['parameters']['unsupported_nonfinite'] = nonfinite
            metadata = observe_tool_metadata([invalid])[0]
            self.assertIsNone(metadata['parameters_sha256'])
            self.assertIsNone(metadata['command_only_schema'])
        invalid_utf8 = bash('The configured host execution window is 30s;\ud800')
        metadata = observe_tool_metadata([invalid_utf8])[0]
        self.assertIsNone(metadata['description_sha256'])
        self.assertIsNone(metadata['go_host_window_claim'])

    def test_journal_is_metadata_only_and_upstream_receives_identical_raw_bytes(self):
        captured = []
        class Upstream(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_POST(self):
                captured.append(self.rfile.read(int(self.headers['Content-Length'])))
                result = json.dumps({'choices': [], 'usage': {'prompt_tokens': 7, 'completion_tokens': 3}}).encode()
                self.send_response(200)
                self.send_header('Content-Length', str(len(result)))
                self.end_headers(); self.wfile.write(result)
        with tempfile.TemporaryDirectory() as temporary, patch.dict('os.environ', {'UPSTREAM_API_KEY': 'synthetic-relay-key'}):
            upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
            proxy = AuditServer(('127.0.0.1', 0), f'http://127.0.0.1:{upstream.server_port}', Path(temporary) / 'wire.jsonl', 'metadata-contract')
            threads = [threading.Thread(target=server.serve_forever) for server in (upstream, proxy)]
            for thread in threads: thread.start()
            try:
                tool = bash('The configured host execution window is 5m0s; SYNTHETIC_PRIVATE_DESCRIPTION')
                tool['function']['parameters']['properties']['command']['description'] = 'SYNTHETIC_PRIVATE_COMMAND'
                body = {'model': MODEL, 'reasoning_effort': 'high', 'max_tokens': 16384,
                        'messages': [{'role': 'user', 'content': 'SYNTHETIC_PRIVATE_MESSAGE'},
                                     {'role': 'assistant', 'content': None, 'tool_calls': [{'id': 'private-call', 'type': 'function', 'function': {
                                         'name': 'bash', 'arguments': json.dumps({'command': 'SYNTHETIC_PRIVATE_ACTUAL_COMMAND'})}}]}], 'tools': [tool]}
                raw = json.dumps(body, indent=3, ensure_ascii=False).encode('utf-8')
                request = urllib.request.Request(f'http://127.0.0.1:{proxy.server_port}/v1/chat/completions', raw, {'Content-Type': 'application/json'})
                with urllib.request.urlopen(request, timeout=5) as response: response.read()
                deadline = time.monotonic() + 2
                rows = []
                while time.monotonic() < deadline:
                    rows = [json.loads(line) for line in proxy.journal.read_text().splitlines()]
                    if any(row['phase'] == 'finish' for row in rows): break
                    time.sleep(.005)
                self.assertEqual(captured, [raw])
                self.assertEqual([row['phase'] for row in rows], ['start', 'finish'])
                self.assertEqual(rows[0]['sequence'], 1)
                self.assertEqual(rows[0]['tool_metadata'], observe_tool_metadata([tool]))
                self.assertEqual(rows[1]['tool_metadata'], rows[0]['tool_metadata'])
                journal = proxy.journal.read_text()
                for secret in ('SYNTHETIC_PRIVATE_DESCRIPTION', 'SYNTHETIC_PRIVATE_COMMAND', 'SYNTHETIC_PRIVATE_ACTUAL_COMMAND', 'SYNTHETIC_PRIVATE_MESSAGE', 'synthetic-relay-key'):
                    self.assertNotIn(secret, journal)
                self.assertEqual(set(rows[0]['tool_metadata'][0]), {'index', 'name', 'description_sha256', 'parameters_sha256', 'go_host_window_claim', 'command_only_schema'})
            finally:
                for server in (proxy, upstream): server.shutdown(); server.server_close()
                for thread in threads: thread.join(timeout=5)
                self.assertTrue(all(not thread.is_alive() for thread in threads))


if __name__ == '__main__':
    unittest.main()
