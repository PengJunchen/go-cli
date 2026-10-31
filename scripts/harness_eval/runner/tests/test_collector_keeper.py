import contextlib
import io
import json
import signal
import sys
import types
import unittest
from unittest.mock import patch

import run_trial

CID = 'a' * 64


def process(pid, *, ppid=1, start=123, state='S', exe_inode=800):
    return {'pid': pid, 'ppid': ppid, 'pgid': pid, 'start': start,
            'state': state, 'exe_inode': exe_inode}


def stat_line(row):
    fields = ['0'] * 50
    fields[0] = row['state']
    fields[1] = str(row['ppid'])
    fields[2] = str(row['pgid'])
    fields[19] = str(row['start'])
    return str(row['pid']) + ' (untrusted comm) ' + ' '.join(fields)


class CapturedText(io.StringIO):
    def __init__(self, files, name):
        super().__init__()
        self.files, self.name = files, name

    def fileno(self):
        return 1000

    def close(self):
        self.files[self.name] = self.getvalue()
        super().close()


class CapturedBytes(io.BytesIO):
    def __init__(self, files, name):
        super().__init__()
        self.files, self.name = files, name

    def close(self):
        self.files[self.name] = self.getvalue()
        super().close()


class FakeProc:
    """No host proc, signal, Docker, subprocess, or task file access."""
    def __init__(self, rows):
        self.rows = {r['pid']: r for r in rows}
        self.files = {}

    def open(self, name, mode='r', **kwargs):
        name = str(name)
        if name.startswith('/proc/') and name.endswith('/stat'):
            pid = int(name.split('/')[2])
            if pid not in self.rows:
                raise FileNotFoundError('synthetic disappeared')
            return io.StringIO(stat_line(self.rows[pid]))
        if name.startswith('/logs/agent/'):
            if mode == 'wb':
                return CapturedBytes(self.files, name)
            if mode == 'w':
                return CapturedText(self.files, name)
        raise AssertionError('unmocked file access')

    def exe_stat(self, name, **kwargs):
        pid = int(str(name).split('/')[2])
        if pid not in self.rows:
            raise FileNotFoundError('synthetic disappeared')
        return types.SimpleNamespace(st_dev=2, st_ino=self.rows[pid]['exe_inode'])

    @contextlib.contextmanager
    def context(self):
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch('builtins.open', side_effect=self.open))
            stack.enter_context(patch('run_trial.os.listdir', side_effect=lambda _: ['1', '900', *map(str, self.rows)]))
            stack.enter_context(patch('run_trial.os.getpid', return_value=900))
            stack.enter_context(patch('run_trial.os.readlink', return_value='/usr/bin/sleep'))
            stack.enter_context(patch('run_trial.os.stat', side_effect=self.exe_stat))
            stack.enter_context(patch('run_trial.os.makedirs'))
            stack.enter_context(patch('run_trial.os.fsync'))
            kills = stack.enter_context(patch('run_trial.os.kill'))
            commands = stack.enter_context(patch('run_trial.subprocess.run', return_value=types.SimpleNamespace(stdout=b'patch')))
            yield kills, commands


def anchor():
    return {'schema': 'trusted-init-keeper/v1', 'container_id': CID, 'init': True,
            'pid': 7, 'ppid': 1, 'pgid': 7, 'starttime': 123,
            'exe': '/usr/bin/sleep', 'exe_device': 2, 'exe_inode': 800}


def collect(binding=None, cid=CID):
    args = ['collector', 'baseline']
    if binding is not None:
        args += [json.dumps(binding), cid]
    with patch.object(sys, 'argv', args):
        exec(run_trial.COLLECT, {'__name__': 'synthetic_collector'})


class KeeperContract(unittest.TestCase):
    def test_legacy_init_false_retains_exact_stop_kill_sequence(self):
        proc = FakeProc([process(7), process(8)])
        with proc.context() as (kills, commands):
            collect()
        self.assertEqual(kills.call_args_list,
                         [unittest.mock.call(7, signal.SIGSTOP), unittest.mock.call(8, signal.SIGSTOP),
                          unittest.mock.call(7, signal.SIGKILL), unittest.mock.call(8, signal.SIGKILL)])
        self.assertEqual(commands.call_count, 3)

    def test_valid_init_keeper_excluded_but_other_ppid1_leftover_killed(self):
        proc = FakeProc([process(7), process(8)])
        with proc.context() as (kills, commands):
            collect(anchor())
        self.assertEqual(kills.call_args_list,
                         [unittest.mock.call(8, signal.SIGSTOP), unittest.mock.call(8, signal.SIGKILL)])
        self.assertEqual(commands.call_count, 3)
        before = json.loads(proc.files['/logs/agent/processes-before-cleanup.json'])
        self.assertEqual([r['pid'] for r in before['processes']], [7, 8])
        self.assertEqual(before['excluded_pids'], [1, 900])

    def test_bad_keeper_identity_rejects_before_any_signal_or_git(self):
        variants = {
            'missing': [], 'start_replaced': [process(7, start=124)],
            'parent_replaced': [process(7, ppid=2)], 'exe_replaced': [process(7, exe_inode=801)],
            'dead': [process(7, state='Z')],
        }
        for label, rows in variants.items():
            with self.subTest(label=label):
                proc = FakeProc(rows)
                with proc.context() as (kills, commands):
                    with self.assertRaises(RuntimeError):
                        collect(anchor())
                    kills.assert_not_called()
                    commands.assert_not_called()
                self.assertIn('/logs/agent/processes-before-cleanup.json', proc.files)
        proc = FakeProc([process(7)])
        with proc.context() as (kills, commands):
            with self.assertRaises(RuntimeError):
                collect(anchor(), cid='b' * 64)
            kills.assert_not_called()
            commands.assert_not_called()

    def test_unknown_or_malformed_anchor_rejects_with_snapshot_and_no_signals(self):
        variants = [None, {}, {**anchor(), 'init': False}, {**anchor(), 'pid': '7'},
                    {**anchor(), 'container_id': 'b' * 64}]
        for binding in variants:
            with self.subTest(binding=binding):
                proc = FakeProc([process(7)])
                with proc.context() as (kills, commands), patch.object(sys, 'argv',
                        ['collector', 'baseline', json.dumps(binding), CID]):
                    with self.assertRaises(RuntimeError):
                        exec(run_trial.COLLECT, {'__name__': 'synthetic_collector'})
                    kills.assert_not_called()
                    commands.assert_not_called()
                self.assertIn('/logs/agent/processes-before-cleanup.json', proc.files)
        for failure in (PermissionError('synthetic denied'), FileNotFoundError('synthetic missing')):
            proc = FakeProc([process(7)])
            with proc.context() as (kills, commands), patch('run_trial.os.readlink', side_effect=failure):
                with self.assertRaises(RuntimeError):
                    collect(anchor())
                kills.assert_not_called()
                commands.assert_not_called()

    def test_unknown_proc_enumeration_never_signals_or_claims_capture(self):
        proc = FakeProc([process(7)])
        with proc.context() as (kills, commands), patch('run_trial.os.listdir', side_effect=PermissionError('synthetic denied')):
            with self.assertRaises(RuntimeError):
                run_trial.capture_init_keeper('/proc', CID)
            with self.assertRaises(RuntimeError):
                collect(anchor())
            kills.assert_not_called()
            commands.assert_not_called()
        before = json.loads(proc.files['/logs/agent/processes-before-cleanup.json'])
        self.assertEqual(before['enumeration_status'], 'unknown')
        self.assertIsNone(before['processes'])

    def test_capture_only_trusted_unique_pre_native_init_child(self):
        proc = FakeProc([process(7)])
        with proc.context() as (kills, commands):
            self.assertEqual(run_trial.capture_init_keeper('/proc', CID), anchor())
            kills.assert_not_called()
            commands.assert_not_called()
        for rows in ([], [process(7), process(8)]):
            with self.subTest(rows=len(rows)):
                proc = FakeProc(rows)
                with proc.context() as (kills, commands):
                    with self.assertRaises(RuntimeError):
                        run_trial.capture_init_keeper('/proc', CID)
                    kills.assert_not_called()
                    commands.assert_not_called()


if __name__ == '__main__':
    unittest.main()
