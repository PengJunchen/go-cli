#!/usr/bin/env python3
"""Run bounded real-CLI PTY checks using only a local synthetic provider."""

from __future__ import annotations

import argparse
import fcntl
import http.server
import json
import os
import pathlib
import pty
import re
import select
import signal
import shutil
import struct
import subprocess
import tempfile
import termios
import threading
import time
import urllib.parse


SOURCE_COMMIT = "working-tree"
API_KEY = "pty-fixture-canary-not-a-real-provider-key"
LONG_CONTENT = "PTY_ONCE_BEGIN " + ("safe-review-row-0123456789-" * 24) + " PTY_ONCE_END"
ANSWER = "X保留队列：交互通过 ✅ 🙂"
MULTILINE = "PTY multiline first line\nPTY multiline second line"
ANSI_RE = re.compile(
    rb"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\)|[@-Z\\-_])"
)


class FakeProvider(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address):
        super().__init__(address, FakeProviderHandler)
        self.lock = threading.Lock()
        self.requests = []
        self.workspace = None

    def snapshot(self):
        with self.lock:
            return list(self.requests)


class FakeProviderHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format, *_args):
        return

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        try:
            body = json.loads(self.rfile.read(length))
        except Exception as exc:  # pragma: no cover - printed in fixture record
            self.send_error(400, str(exc))
            return

        messages = body.get("messages", [])
        last_user_index = next(
            (index for index in range(len(messages) - 1, -1, -1)
             if messages[index].get("role") == "user"),
            -1,
        )
        last_user = (
            messages[last_user_index].get("content", "")
            if last_user_index >= 0 else ""
        )
        # A prior turn's tool receipts must not suppress the scripted call for
        # a new REPL input. Only receipts after the latest user message close
        # this fixture's current response/tool cycle.
        tool_results = [
            {"tool_call_id": message.get("tool_call_id", ""),
             "name": message.get("name", ""),
             "content": message.get("content", "")}
            for message in messages[last_user_index + 1:]
            if message.get("role") == "tool"
        ]
        calls = []
        if not tool_results:
            if last_user == "PTY cancel active":
                calls = [tool_call("call_cancel_active", "write", {
                    "path": str(self.server.workspace / "cancel-active.txt"),
                    "content": "CANCEL_ACTIVE_MUST_NOT_BE_WRITTEN",
                })]
            elif last_user == "PTY deny":
                calls = [tool_call("call_deny", "write", {
                    "path": str(self.server.workspace / "denied.txt"),
                    "content": "DENY_MUST_NOT_BE_WRITTEN",
                })]
            elif last_user == "PTY once":
                calls = [tool_call("call_once", "write", {
                    "path": str(self.server.workspace / "once.txt"),
                    "content": LONG_CONTENT,
                })]
            elif last_user == "PTY ask":
                calls = [tool_call("call_ask", "ask_user", {
                    "question": "请用中文确认临时测试方案。",
                })]
            elif last_user == "PTY ask cancel":
                calls = [tool_call("call_ask_cancel", "ask_user", {
                    "question": "Esc 取消后不应生成回答。",
                })]
            elif last_user == "PTY queue allow":
                calls = [
                    tool_call("call_queue_a", "write", {
                        "path": str(self.server.workspace / "queue-a.txt"),
                        "content": "QUEUE_A_SINGLE_EFFECT",
                    }),
                    tool_call("call_queue_b", "write", {
                        "path": str(self.server.workspace / "queue-b.txt"),
                        "content": "QUEUE_B_SINGLE_EFFECT",
                    }),
                ]

        auth_ok = self.headers.get("Authorization", "") == "Bearer " + API_KEY
        record = {
            "path": self.path,
            "auth_ok": auth_ok,
            "stream": bool(body.get("stream")),
            "last_user": last_user,
            "tool_results": tool_results,
            "response_tools": [call["function"]["name"] for call in calls],
            "response_tool_ids": [call["id"] for call in calls],
        }
        with self.server.lock:
            self.server.requests.append(record)

        if not auth_ok:
            self.send_error(401, "fixture credential mismatch")
            return
        if not self.path.endswith("/chat/completions"):
            self.send_error(404, "unexpected fixture route")
            return
        if body.get("stream"):
            self.write_stream(calls, last_user, bool(tool_results))
        else:
            self.write_json(calls, last_user, bool(tool_results))

    def write_json(self, calls, last_user, has_tool_result):
        if calls:
            message = {"role": "assistant", "content": None, "tool_calls": calls}
            reason = "tool_calls"
        else:
            message = {"role": "assistant", "content": final_text(last_user, has_tool_result)}
            reason = "stop"
        payload = json.dumps({"choices": [{"index": 0, "message": message,
                                            "finish_reason": reason}]},
                             ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)
        self.close_connection = True

    def write_stream(self, calls, last_user, has_tool_result):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()

        if calls:
            self.event({"role": "assistant"})
            for index, call in enumerate(calls):
                delta_call = dict(call)
                delta_call["index"] = index
                self.event({"tool_calls": [delta_call]})
            self.chunk({}, "tool_calls")
        else:
            self.event({"role": "assistant"})
            self.event({"content": final_text(last_user, has_tool_result)})
            self.chunk({}, "stop")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True

    def event(self, delta):
        self.chunk(delta, None)

    def chunk(self, delta, finish_reason):
        payload = {
            "id": "r5-pty-fixture",
            "object": "chat.completion.chunk",
            "created": 1,
            "model": "pty-review-model",
            "choices": [{"index": 0, "delta": delta,
                         "finish_reason": finish_reason}],
        }
        self.wfile.write(b"data: " + json.dumps(payload, ensure_ascii=False).encode() + b"\n\n")
        self.wfile.flush()


def tool_call(call_id, name, arguments):
    return {
        "id": call_id,
        "type": "function",
        "function": {"name": name,
                     "arguments": json.dumps(arguments, ensure_ascii=False)},
    }


def final_text(user_text, has_tool_result):
    if has_tool_result:
        return "PTY fixture completed the requested interaction."
    return "PTY fixture received the input."


class PtyCLI:
    def __init__(self, binary, cwd, env, rows=6, columns=20):
        self.master, self.slave = pty.openpty()
        self.original_termios = termios.tcgetattr(self.slave)
        self.final_termios = None
        self.termios_samples = []
        self.last_termios_state = None
        self.buffer = bytearray()
        self.snapshots = []
        self.set_size(rows, columns, send_signal=False)

        slave_fd = self.slave

        def acquire_tty():
            os.setsid()
            fcntl.ioctl(slave_fd, termios.TIOCSCTTY, 0)
            os.tcsetpgrp(slave_fd, os.getpid())

        self.process = subprocess.Popen(
            [binary], stdin=self.slave, stdout=self.slave, stderr=self.slave,
            cwd=cwd, env=env, close_fds=True, preexec_fn=acquire_tty,
        )
        os.set_blocking(self.master, False)
        self.sample_termios("after-start")

    def set_size(self, rows, columns, send_signal=True):
        fcntl.ioctl(self.master, termios.TIOCSWINSZ,
                    struct.pack("HHHH", rows, columns, 0, 0))
        if send_signal and self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGWINCH)

    def pump(self, timeout=0.05):
        ready, _, _ = select.select([self.master], [], [], timeout)
        if not ready:
            self.sample_termios("pump-idle")
            return b""
        try:
            data = os.read(self.master, 65536)
        except OSError:
            return b""
        if data:
            self.buffer.extend(data)
        self.sample_termios("pump-data")
        return data

    def sample_termios(self, label):
        for fd_name, fd in (("slave", self.slave), ("master", self.master)):
            try:
                attrs = termios.tcgetattr(fd)
            except termios.error:
                continue
            state = (fd_name, attrs[3])
            if state != self.last_termios_state:
                self.termios_samples.append((label, fd_name, attrs[3]))
                self.last_termios_state = state
            return attrs
        return None

    def wait_for(self, needle, start=0, timeout=8.0):
        needle_bytes = needle.encode("utf-8") if isinstance(needle, str) else needle
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if needle_bytes in self.buffer[start:]:
                return len(self.buffer)
            if self.process.poll() is not None:
                raise AssertionError(f"CLI exited {self.process.returncode} before {needle!r}")
            self.pump(min(0.1, deadline - time.monotonic()))
        raise AssertionError(f"timeout waiting for {needle!r}; recent={self.plain()[-5000:]!r}")

    def wait_prompt(self, start=0, timeout=8.0):
        return self.wait_for(b"\x1b[?2004h> ", start=start, timeout=timeout)

    def send(self, data, settle=0.12):
        self.pump(0)
        os.write(self.master, data.encode("utf-8") if isinstance(data, str) else data)
        self.sample_termios("after-write")
        deadline = time.monotonic() + settle
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            self.pump(min(0.05, remaining))

    def plain(self):
        return ANSI_RE.sub(b"", bytes(self.buffer)).decode("utf-8", "replace")

    def plain_since(self, start):
        return ANSI_RE.sub(b"", bytes(self.buffer[start:])).decode("utf-8", "replace")

    def snapshot(self, label, start=0):
        text = self.plain_since(start)
        self.snapshots.append(f"--- {label} ---\n{text}\n")
        return text

    def terminate_group(self):
        if self.process.poll() is None:
            try:
                os.killpg(self.process.pid, signal.SIGTERM)
            except OSError:
                try:
                    self.process.terminate()
                except OSError:
                    pass
            try:
                self.process.wait(timeout=1.0)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(self.process.pid, signal.SIGKILL)
                except OSError:
                    try:
                        self.process.kill()
                    except OSError:
                        pass
                try:
                    self.process.wait(timeout=2.0)
                except subprocess.TimeoutExpired:
                    try:
                        os.kill(self.process.pid, signal.SIGKILL)
                    except OSError:
                        pass
                    self.process.wait(timeout=2.0)
        while self.pump(0.01):
            pass

    def close(self):
        self.terminate_group()
        try:
            self.final_termios = termios.tcgetattr(self.slave)
        except termios.error:
            try:
                self.final_termios = termios.tcgetattr(self.master)
            except termios.error:
                self.final_termios = None
        os.close(self.master)
        os.close(self.slave)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", default="/tmp/go-cli-r5-pty-84324")
    parser.add_argument("--evidence-dir", default=str(pathlib.Path(__file__).resolve().parent))
    parser.add_argument("--source-commit", default=SOURCE_COMMIT)
    args = parser.parse_args()
    binary = str(pathlib.Path(args.binary).resolve())
    if not pathlib.Path(binary).is_file():
        raise SystemExit(f"CLI binary does not exist: {binary}")
    evidence_dir = pathlib.Path(args.evidence_dir)
    evidence_dir.mkdir(parents=True, exist_ok=True)

    observations = []
    tty_restored = False
    raw_path = evidence_dir / "r5-pty-transcript.raw"
    plain_path = evidence_dir / "r5-pty-transcript.txt"
    summary_path = evidence_dir / "r5-pty-summary.txt"
    cancel_raw_path = evidence_dir / "r5-pty-cancel-transcript.raw"
    cancel_plain_path = evidence_dir / "r5-pty-cancel-transcript.txt"
    cancel_summary_path = evidence_dir / "r5-pty-cancel-summary.txt"
    provider = FakeProvider(("127.0.0.1", 0))
    server_thread = threading.Thread(target=provider.serve_forever, daemon=True)
    server_thread.start()

    with tempfile.TemporaryDirectory(prefix="go-cli-r5-pty-") as temp_root:
        root = pathlib.Path(temp_root)
        home = root / "home"
        xdg = root / "xdg-config"
        workspace = root / "workspace"
        for path in (home / ".go-cli", xdg, workspace, root / "cache", root / "state", root / "data"):
            path.mkdir(parents=True, exist_ok=True)
        provider.workspace = workspace
        host, port = provider.server_address
        base_url = f"http://{host}:{port}/v1"
        config = (
            "provider:\n"
            "  name: openai\n"
            f"  api_key: {API_KEY}\n"
            f"  base_url: {base_url}\n"
            "  model: pty-review-model\n"
            "tools:\n"
            "  parallel: true\n"
            "session:\n"
            f"  store_path: {workspace / 'sessions'}\n"
            "sandbox:\n"
            "  allowed_paths:\n"
            f"    - {workspace}\n"
            "tracing:\n"
            "  enabled: false\n"
            "  exporter: none\n"
            "tui:\n"
            "  theme: dark\n"
            "  word_wrap: 0\n"
        )
        (home / ".go-cli" / "config.yaml").write_text(config, encoding="utf-8")

        env = {
            "HOME": str(home),
            "XDG_CONFIG_HOME": str(xdg),
            "XDG_CACHE_HOME": str(root / "cache"),
            "XDG_STATE_HOME": str(root / "state"),
            "XDG_DATA_HOME": str(root / "data"),
            "TMPDIR": str(root),
            "PATH": "/bin:/usr/bin",
            "TERM": "xterm-256color",
            "LANG": "en_US.UTF-8",
            "LC_CTYPE": "en_US.UTF-8",
            "NO_COLOR": "1",
            "GO_CLI_NO_ONBOARDING": "1",
            "NO_PROXY": "127.0.0.1,localhost",
        }
        cli = PtyCLI(binary, workspace, env, rows=24, columns=80)
        exit_code = None
        try:
            cli.wait_prompt(timeout=12)
            observations.append("startup_prompt=observed")

            cli.set_size(24, 80)
            start = len(cli.buffer)
            cli.send("PTY deny\r")
            cli.wait_for("[approval]", start=start)
            cli.wait_for("[y] once", start=start)
            cli.set_size(4, 20)
            narrow_start = len(cli.buffer)
            cli.wait_for("scope unknown", start=narrow_start, timeout=4)
            observations.append("deny_modal_resize=80x24_to_20x4; compact scope row visible")
            observations.append(cli.snapshot("deny-modal-20x4", start))
            prompt_start = len(cli.buffer)
            cli.send("n")
            try:
                cli.wait_prompt(start=prompt_start, timeout=0.8)
                observations.append("deny_first_key=n was handled")
            except AssertionError:
                observations.append("deny_first_key=n was swallowed; retrying only to inspect subsequent flow")
                prompt_start = len(cli.buffer)
                cli.send("n")
                cli.wait_prompt(start=prompt_start, timeout=10)
            if (workspace / "denied.txt").exists():
                raise AssertionError("denied write reached disk")
            observations.append("approval_deny=n; denied file absent")

            # At 20x4 the compact modal exposes no details; early y must stay locked.
            cli.set_size(24, 80)
            start = len(cli.buffer)
            cli.send("PTY once\r")
            cli.wait_for("[approval]", start=start)
            cli.wait_for("[y] review details", start=start)
            cli.set_size(4, 20)
            narrow_start = len(cli.buffer)
            cli.wait_for("scope unknown", start=narrow_start, timeout=4)
            observations.append(cli.snapshot("once-modal-20x4", start))
            request_count_before = len(provider.snapshot())
            cli.send("y", settle=0.25)
            observations.append("once_first_key=y sent while details were not reviewed")
            cli.send("y", settle=0.25)
            if (workspace / "once.txt").exists():
                raise AssertionError("early y bypassed the unseen-detail review gate")
            if len(provider.snapshot()) != request_count_before:
                raise AssertionError("early y reached the provider before review completed")
            observations.append("one_or_two_early_y_attempts=still_locked; no once.txt and no follow-up provider request")

            cli.set_size(24, 80)
            resize_start = len(cli.buffer)
            cli.wait_for("details", start=resize_start, timeout=4)
            observations.append("resize=20x4_to-80x24; approval details visible")
            observations.append(cli.snapshot("once-modal-resized-80x24", resize_start))
            cli.set_size(30, 120)
            wide_start = len(cli.buffer)
            cli.wait_for("details", start=wide_start, timeout=4)
            observations.append("resize=80x24-to-120x30; approval remains active")
            observations.append(cli.snapshot("once-modal-resized-120x30", wide_start))
            cli.set_size(12, 60)
            # Scrolling is through actual Bubble Tea key events, with a fresh y
            # attempt before accepting the eventual on-disk write.
            scroll_start = len(cli.buffer)
            for _ in range(48):
                cli.send(b"\x1b[B", settle=0.015)
            scroll_text = cli.plain_since(scroll_start)
            status_rows = re.findall(r"details (\d+)-(\d+)/(\d+)", scroll_text)
            if not status_rows or status_rows[-1][1] != status_rows[-1][2]:
                raise AssertionError(f"approval details did not reach the final wrapped row: {status_rows!r}")
            observations.append(f"approval_scroll=last visible row {status_rows[-1][1]}/{status_rows[-1][2]}")
            scroll_end = len(cli.buffer)
            once_prompt_start = len(cli.buffer)
            cli.send("y", settle=0.3)
            once_path = workspace / "once.txt"
            deadline = time.monotonic() + 8
            while time.monotonic() < deadline and not once_path.exists():
                if cli.process.poll() is not None:
                    raise AssertionError(f"CLI exited {cli.process.returncode} before once write")
                cli.pump(0.05)
            if not once_path.exists():
                # A bounded retry catches slow PTY event processing without
                # relaxing the fact that approval must first be visibly reviewed.
                for _ in range(48):
                    cli.send(b"\x1b[B", settle=0.015)
                once_prompt_start = len(cli.buffer)
                cli.send("y", settle=0.3)
                deadline = time.monotonic() + 8
                while time.monotonic() < deadline and not once_path.exists():
                    cli.pump(0.05)
            if not once_path.exists():
                raise AssertionError("once approval did not execute after complete detail scrolling")
            if once_path.read_text(encoding="utf-8") != LONG_CONTENT:
                raise AssertionError("once write content differs from the approved synthetic payload")
            observations.append("approval_once=y after scrolling; once.txt exactly matches synthetic payload")
            observations.append(cli.snapshot("once-approved-and-completed", scroll_end))
            cli.send(b"\x1b[F")  # End resumes transcript follow-tail from the actual REPL.
            observations.append(cli.snapshot("transcript-end-after-durable-refresh", once_prompt_start))
            cli.wait_for("[receipt: success]", start=once_prompt_start, timeout=12)
            observations.append("tool_receipt=success displayed in the durable session transcript after approval")
            cli.wait_prompt(start=once_prompt_start, timeout=12)
            (evidence_dir / "r5-pty-success-frame.raw").write_bytes(bytes(cli.buffer))

            # Actual ask_user modal: enter a Chinese/emoji free-text answer.
            start = len(cli.buffer)
            cli.send("PTY ask\r")
            cli.wait_for("[approval] ask_user", start=start, timeout=12)
            cli.wait_for("[y] once", start=start)
            ask_modal_start = len(cli.buffer)
            cli.send("y")
            try:
                cli.wait_for("[ask_user]", start=ask_modal_start, timeout=0.8)
                observations.append("ask_user_tool_approval=first y accepted")
            except AssertionError:
                observations.append("ask_user_tool_approval=first y swallowed; second key sent to reach HITL")
                ask_modal_start = len(cli.buffer)
                cli.send("y")
                cli.wait_for("[ask_user]", start=ask_modal_start, timeout=12)
            cli.wait_for("请用中文确认临时测试方案。", start=ask_modal_start)
            observations.append(cli.snapshot("ask-user-before-answer", start))
            prompt_start = len(cli.buffer)
            cli.send(ANSWER)
            cli.send(b"\r")
            cli.wait_prompt(start=prompt_start, timeout=12)
            requests = provider.snapshot()
            ask_results = [
                result for record in requests
                if record["last_user"] == "PTY ask"
                for result in record["tool_results"]
                if result["tool_call_id"] == "call_ask"
            ]
            if len(ask_results) == 1 and ask_results[0]["content"] == ANSWER:
                observations.append(f"ask_user_answer=provider received exact {ANSWER!r}")
            else:
                observations.append(
                    "ask_user_answer_mismatch="
                    + json.dumps(ask_results, ensure_ascii=False)
                    + f"; expected {ANSWER!r}"
                )

            # Esc in the actual ask_user modal must cancel the question without
            # fabricating an answer, then return one correlated tool error.
            start = len(cli.buffer)
            cli.send("PTY ask cancel\r")
            cli.wait_for("[approval] ask_user", start=start, timeout=12)
            cli.wait_for("Esc 取消后不应生成回答。", start=start, timeout=8)
            cancel_modal_start = len(cli.buffer)
            cli.send("y")
            cli.wait_for("[ask_user]", start=cancel_modal_start, timeout=12)
            cli.wait_for("Esc 取消后不应生成回答。", start=cancel_modal_start, timeout=8)
            cli.send(b"\x1b")
            cli.wait_prompt(start=start, timeout=12)
            ask_cancel_results = [
                result for record in provider.snapshot()
                if record["last_user"] == "PTY ask cancel"
                for result in record["tool_results"]
                if result["tool_call_id"] == "call_ask_cancel"
            ]
            if len(ask_cancel_results) != 1 or "canceled" not in ask_cancel_results[0]["content"].lower():
                raise AssertionError(f"ask_user Esc cancellation was not returned once as a correlated tool error: {ask_cancel_results!r}")
            if ANSWER in ask_cancel_results[0]["content"]:
                raise AssertionError("ask_user cancellation persisted an answer value")
            observations.append("ask_user_cancel=Esc returned one correlated canceled tool result and no answer")

            # Two provider calls are emitted together. The TUI must serialize
            # their approvals, hold both effects, and resume only after each
            # exact approval decision.
            start = len(cli.buffer)
            cli.send("PTY queue allow\r")
            cli.wait_for("[approval] write", start=start, timeout=12)
            queue_a = workspace / "queue-a.txt"
            queue_b = workspace / "queue-b.txt"
            if queue_a.exists() or queue_b.exists():
                raise AssertionError("queued writes executed before their approvals")
            queued_calls = [
                call_id for record in provider.snapshot()
                if record["last_user"] == "PTY queue allow"
                for call_id in record["response_tool_ids"]
            ]
            if queued_calls != ["call_queue_a", "call_queue_b"]:
                raise AssertionError(f"provider did not enqueue both exact write calls before approval: {queued_calls!r}")
            observations.append("concurrent_approval_queue=both exact write calls arrived; neither effect ran before approval")
            queued_modal_start = start
            for index in range(2):
                modal_start = queued_modal_start
                expected_token = f"local interaction #{index + 1}"
                expected_content = ("QUEUE_A_SINGLE_EFFECT" if index == 0 else "QUEUE_B_SINGLE_EFFECT")
                cli.wait_for("[approval] write", start=modal_start, timeout=12)
                cli.wait_for("[y] once", start=modal_start, timeout=8)
                cli.wait_for(expected_token, start=modal_start, timeout=8)
                cli.wait_for(expected_content, start=modal_start, timeout=8)
                # Scroll to the final details row so the UI's review gate is
                # satisfied before approving either queued write. Send explicit
                # scroll input for every modal, including details that fit.
                for _ in range(24):
                    cli.send(b"\x1b[B", settle=0.01)
                rows = re.findall(r"details (\d+)-(\d+)/(\d+)", cli.plain_since(modal_start))
                if not rows or rows[-1][1] != rows[-1][2]:
                    raise AssertionError(f"queued approval details were not fully reviewed: {rows!r}")
                next_modal_start = len(cli.buffer)
                cli.send("y", settle=0.25)
                if index == 0:
                    cli.wait_for("local interaction #2", start=next_modal_start, timeout=12)
                    cli.wait_for("QUEUE_B_SINGLE_EFFECT", start=next_modal_start, timeout=8)
                    if queue_a.exists() or queue_b.exists():
                        raise AssertionError("a queued write executed while the second approval decision was still pending")
                    observations.append("queue approval mapping=modal #1 showed queue-a; modal #2 showed queue-b; both effects remained held until both decisions")
                    queued_modal_start = next_modal_start
            deadline = time.monotonic() + 8
            def completed_write_count():
                return int(queue_a.exists()) + int(queue_b.exists())
            while time.monotonic() < deadline and completed_write_count() < 2:
                cli.pump(0.05)
            if completed_write_count() != 2:
                raise AssertionError(f"both granted queue approvals produced {completed_write_count()} of two writes")
            if queue_a.read_text(encoding="utf-8") != "QUEUE_A_SINGLE_EFFECT" or queue_b.read_text(encoding="utf-8") != "QUEUE_B_SINGLE_EFFECT":
                raise AssertionError("queued approval results did not match their exact fixture contents")
            queue_results = [
                result for record in provider.snapshot()
                if record["last_user"] == "PTY queue allow"
                for result in record["tool_results"]
                if result["tool_call_id"] in {"call_queue_a", "call_queue_b"}
            ]
            if sorted(result["tool_call_id"] for result in queue_results) != ["call_queue_a", "call_queue_b"]:
                raise AssertionError(f"queued approvals did not return one result per exact call ID: {queue_results!r}")
            observations.append("concurrent_approval_queue=two writes held, approved in FIFO order, one exact result per call ID")
            cli.send(b"\x1b[F")
            cli.wait_for("PTY fixture completed the requested interaction.", start=start, timeout=12)
            cli.wait_prompt(start=start, timeout=12)

            # Chinese/emoji cursor movement plus insertion and grapheme backspace.
            start = len(cli.buffer)
            cli.send("测试🙂B", settle=0.05)
            cli.send(b"\x1b[D", settle=0.05)  # before B
            cli.send("X", settle=0.05)
            cli.send(b"\x1b[D", settle=0.05)  # between emoji and X
            cli.send(b"\x7f", settle=0.05)     # remove the emoji grapheme
            prompt_start = len(cli.buffer)
            cli.send(b"\r")
            cli.wait_prompt(start=prompt_start, timeout=12)
            requests = provider.snapshot()
            if not any(record["last_user"] == "测试XB" for record in requests):
                seen = [record["last_user"] for record in requests]
                raise AssertionError(f"UTF-8 caret result mismatch; provider saw {seen!r}")
            observations.append("utf8_caret=left/insert/left/backspace submitted exact '测试XB'")
            observations.append(cli.snapshot("utf8-caret-input", start))

            cli.set_size(30, 120)
            multiline_start = len(cli.buffer)
            cli.send(b"\x1b[200~" + MULTILINE.encode("utf-8") + b"\x1b[201~", settle=0.1)
            cli.send(b"\r", settle=0.1)
            cli.wait_prompt(start=multiline_start, timeout=12)
            requests = provider.snapshot()
            if not any(record["last_user"] == MULTILINE for record in requests):
                seen = [record["last_user"] for record in requests]
                raise AssertionError(f"bracketed multiline input mismatch; provider saw {seen!r}")
            observations.append("multiline_bracketed_paste=exact two-line payload submitted at 120x30")
            observations.append(cli.snapshot("multiline-input-120x30", multiline_start))

            session_dir = workspace / "sessions" / "sessions"
            session_files = list(session_dir.glob("*.jsonl"))
            if len(session_files) != 1:
                raise AssertionError(f"expected one durable physical-session file, found {session_files!r}")
            persisted = []
            for raw_line in session_files[0].read_text(encoding="utf-8").splitlines():
                try:
                    persisted.append(json.loads(raw_line))
                except json.JSONDecodeError as exc:
                    raise AssertionError(f"invalid durable session JSON line: {exc}") from exc
            user_entries = [entry for entry in persisted if entry.get("type") == "user"]
            assistant_entries = [entry for entry in persisted if entry.get("type") == "assistant"]
            successful_receipts = [
                entry for entry in persisted
                if entry.get("type") == "tool" and entry.get("receipt_outcome") == "success"
            ]
            tool_rows = [entry for entry in persisted if entry.get("type") == "tool"]
            tool_receipts = [entry for entry in tool_rows if entry.get("receipt_outcome")]
            expected_receipts = {
                "call_deny": "error",
                "call_once": "success",
                "call_ask": "success",
                "call_ask_cancel": "error",
                "call_queue_a": "success",
                "call_queue_b": "success",
            }
            receipts_by_id = {}
            for receipt in tool_receipts:
                call_id = receipt.get("tool_call_id", "")
                if call_id in expected_receipts:
                    receipts_by_id.setdefault(call_id, []).append(receipt)
            for call_id, expected_outcome in expected_receipts.items():
                rows = receipts_by_id.get(call_id, [])
                if len(rows) != 1 or rows[0].get("receipt_outcome") != expected_outcome:
                    raise AssertionError(f"durable receipt mismatch for {call_id}: expected one {expected_outcome}, got {rows!r}")
            queue_intents = {}
            queue_executions = {}
            for call_id in ("call_queue_a", "call_queue_b"):
                queue_intents[call_id] = [
                    entry for entry in tool_rows
                    if entry.get("tool_call_id") == call_id and entry.get("action_state") == "intent"
                ]
                queue_executions[call_id] = [
                    entry for entry in tool_rows
                    if entry.get("tool_call_id") == call_id and entry.get("action_state") == "execution"
                ]
                if len(queue_intents[call_id]) != 1 or len(queue_executions[call_id]) != 1:
                    raise AssertionError(
                        f"durable pending/execution IDs are incomplete for {call_id}: "
                        f"intents={queue_intents[call_id]!r} executions={queue_executions[call_id]!r}"
                    )
                intent = queue_intents[call_id][0]
                execution = queue_executions[call_id][0]
                if (
                    intent.get("action_id") != execution.get("action_id")
                    or intent.get("action_batch_id") != execution.get("action_batch_id")
                    or execution.get("dispatch_state") != "dispatched"
                    or execution.get("resource_state") != "quiescent"
                    or execution.get("completion_proof") != "queued_job_joined"
                ):
                    raise AssertionError(f"durable action identity or completion proof mismatch for {call_id}: {intent!r} / {execution!r}")
            queue_action_ids = [queue_intents[call_id][0].get("action_id") for call_id in ("call_queue_a", "call_queue_b")]
            queue_batches = [queue_intents[call_id][0].get("action_batch_id") for call_id in ("call_queue_a", "call_queue_b")]
            if len(set(queue_action_ids)) != 2 or len(set(queue_batches)) != 1:
                raise AssertionError(f"queued calls did not retain distinct action IDs in one approval batch: ids={queue_action_ids!r} batches={queue_batches!r}")
            cancel_execution = [
                entry for entry in tool_rows
                if entry.get("tool_call_id") == "call_ask_cancel" and entry.get("action_state") == "execution"
            ]
            cancel_resolutions = [
                entry for entry in persisted
                if entry.get("type") == "checkpoint"
                and entry.get("tool_call_id") == "call_ask_cancel"
                and entry.get("checkpoint_kind") == "execution_resolution"
            ]
            if (
                len(cancel_execution) != 1
                or cancel_execution[0].get("resource_state") != "quiescent"
                or len(cancel_resolutions) != 1
                or cancel_resolutions[0].get("execution_resolution", {}).get("evidence") != "local_resources_quiescent"
                or cancel_resolutions[0].get("execution_resolution", {}).get("action_id") != cancel_execution[0].get("action_id")
            ):
                raise AssertionError(f"ask_user decline lacks one joined durable execution resolution: {cancel_execution!r} / {cancel_resolutions!r}")
            provider_results_by_id = {}
            for request in provider.snapshot():
                for result in request["tool_results"]:
                    call_id = result.get("tool_call_id", "")
                    if call_id in expected_receipts:
                        provider_results_by_id.setdefault(call_id, []).append(result)
            for call_id in expected_receipts:
                if len(provider_results_by_id.get(call_id, [])) != 1:
                    raise AssertionError(f"provider did not receive exactly one correlated result for {call_id}: {provider_results_by_id.get(call_id)!r}")
            final_requests_by_user = {}
            for request in provider.snapshot():
                if request["last_user"] in {"PTY deny", "PTY once", "PTY ask", "PTY ask cancel", "PTY queue allow"} and request["tool_results"] and not request["response_tool_ids"]:
                    final_requests_by_user[request["last_user"]] = final_requests_by_user.get(request["last_user"], 0) + 1
            if any(final_requests_by_user.get(user, 0) != 1 for user in ("PTY deny", "PTY once", "PTY ask", "PTY ask cancel", "PTY queue allow")):
                raise AssertionError(f"tool cycles did not produce exactly one final provider response: {final_requests_by_user!r}")
            final_answers = [entry for entry in assistant_entries if entry.get("content") == "PTY fixture completed the requested interaction."]
            if len(final_answers) != 5:
                raise AssertionError(f"expected five canonical final assistant answers, got {len(final_answers)}")
            if not any(MULTILINE == entry.get("content") for entry in user_entries):
                raise AssertionError("durable selected-session transcript omitted the exact multiline user entry")
            if not assistant_entries:
                raise AssertionError("durable selected-session transcript omitted assistant rows")
            if not successful_receipts:
                raise AssertionError("durable selected-session transcript omitted the approved write receipt")
            observations.append(
                f"durable_session_file={session_files[0].name}; "
                f"entries={len(persisted)} user={len(user_entries)} assistant={len(assistant_entries)} "
                f"successful_tool_receipts={len(successful_receipts)}"
            )
            (evidence_dir / "r5-session.jsonl").write_bytes(session_files[0].read_bytes())
            (evidence_dir / "r5-provider-requests.json").write_text(
                json.dumps(provider.snapshot(), ensure_ascii=False, indent=2), encoding="utf-8"
            )
            observations.append("durable_ids=one joined receipt per exact call ID; queued calls have distinct action IDs in one batch; ask_user decline has a quiescent execution-resolution checkpoint")

            # Exit through the actual REPL command, then inspect the shared PTY state.
            cli.exit_sample_start = len(cli.termios_samples)
            cli.send("/exit\r")
            deadline = time.monotonic() + 10
            while cli.process.poll() is None and time.monotonic() < deadline:
                cli.pump(0.05)
            if cli.process.poll() is None:
                raise AssertionError("CLI did not exit after /exit")
            exit_code = cli.process.returncode
            if exit_code != 0:
                raise AssertionError(f"CLI exited with {exit_code}")
            observations.append("exit=/exit returned status 0")
        except Exception as exc:
            diagnostic = {
                "error": f"{type(exc).__name__}: {exc}",
                "source_commit": args.source_commit,
                "provider_requests": provider.snapshot(),
                "transcript": cli.plain(),
                "observations": observations,
            }
            (evidence_dir / "r5-pty-failure.json").write_text(
                json.dumps(diagnostic, ensure_ascii=False, indent=2), encoding="utf-8"
            )
            try:
                shutil.copytree(workspace, evidence_dir / "failed-workspace", dirs_exist_ok=True)
            except OSError as copy_exc:
                (evidence_dir / "r5-pty-failure-copy-error.txt").write_text(str(copy_exc), encoding="utf-8")
            raise
        finally:
            try:
                cli.close()
            except Exception as exc:
                observations.append(f"pty_cleanup_error={type(exc).__name__}: {exc}")
            lflag_mask = termios.ICANON | termios.ECHO | termios.ISIG
            before_lflag = cli.original_termios[3] & lflag_mask
            post_exit_samples = cli.termios_samples[
                getattr(cli, "exit_sample_start", len(cli.termios_samples)):
            ]
            after_flags = (
                cli.final_termios[3]
                if cli.final_termios is not None
                else next((sample[2] for sample in reversed(post_exit_samples)), None)
            )
            if after_flags is not None:
                after_lflag = after_flags & lflag_mask
                tty_restored = before_lflag == after_lflag
                observations.append(
                    f"tty_lflag_samples_after_exit={post_exit_samples!r}; "
                    f"before={before_lflag} after={after_lflag}"
                )
                observations.append(f"tty_restore={'true' if tty_restored else 'false'}")
            else:
                observations.append(f"tty_restore=unobservable; samples={post_exit_samples!r}")
            raw_path.write_bytes(bytes(cli.buffer))
            plain_path.write_text(cli.plain(), encoding="utf-8")
            summary_path.write_text("\n".join(observations + cli.snapshots) + "\n", encoding="utf-8")

        if not tty_restored:
            raise AssertionError("TTY cooked flags were not observed restored to the original PTY state")
        observations.append("tty_restore=ICANON/ECHO/ISIG match original PTY state")

        cancel_workspace = root / "cancel-workspace"
        cancel_home = root / "cancel-home"
        cancel_xdg = root / "cancel-xdg-config"
        for path in (cancel_home / ".go-cli", cancel_xdg, cancel_workspace):
            path.mkdir(parents=True, exist_ok=True)
        (cancel_home / ".go-cli" / "config.yaml").write_text(
            config.replace(str(workspace), str(cancel_workspace)), encoding="utf-8"
        )
        provider.workspace = cancel_workspace
        cancel_env = dict(env)
        cancel_env.update({
            "HOME": str(cancel_home),
            "XDG_CONFIG_HOME": str(cancel_xdg),
        })
        cancel_cli = PtyCLI(binary, cancel_workspace, cancel_env, rows=24, columns=80)
        cancel_exit_code = None
        cancel_tty_restored = False
        cancel_observations = []
        try:
            cancel_cli.wait_prompt(timeout=12)
            cancel_observations.append("startup_prompt=observed")
            cancel_start = len(cancel_cli.buffer)
            cancel_cli.send("PTY cancel active\r")
            cancel_cli.wait_for("[approval]", start=cancel_start, timeout=12)
            cancel_cli.set_size(4, 20)
            cancel_resize_start = len(cancel_cli.buffer)
            cancel_cli.wait_for("scope unknown", start=cancel_resize_start, timeout=4)
            cancel_observations.append("active_approval_resize=80x24_to_20x4")
            cancel_observations.append(cancel_cli.snapshot("cancel-modal-20x4", cancel_start))
            cancel_cli.exit_sample_start = len(cancel_cli.termios_samples)
            cancel_prompt_start = len(cancel_cli.buffer)
            cancel_cli.send(b"\x03")
            cancel_cli.wait_prompt(start=cancel_prompt_start, timeout=15)
            if (cancel_workspace / "cancel-active.txt").exists():
                raise AssertionError("canceling the active approval dispatched the write")
            cancel_observations.append("active_approval_cancel=Ctrl+C; same REPL returned to prompt after durable refresh; write file absent")
            cancel_cli.send("/exit\r")
            cancel_deadline = time.monotonic() + 10
            while cancel_cli.process.poll() is None and time.monotonic() < cancel_deadline:
                cancel_cli.pump(0.05)
            if cancel_cli.process.poll() is None:
                raise AssertionError("CLI did not stop after /exit following canceled active approval")
            cancel_exit_code = cancel_cli.process.returncode
            if cancel_exit_code != 0:
                raise AssertionError(f"cancel PTY exited with {cancel_exit_code}")
            cancel_observations.append("post-cancel /exit returned status 0")
        finally:
            try:
                cancel_cli.close()
            except Exception as exc:
                cancel_observations.append(f"pty_cleanup_error={type(exc).__name__}: {exc}")
            lflag_mask = termios.ICANON | termios.ECHO | termios.ISIG
            before_lflag = cancel_cli.original_termios[3] & lflag_mask
            after_lflag = (
                cancel_cli.final_termios[3] & lflag_mask
                if cancel_cli.final_termios is not None else None
            )
            cancel_tty_restored = after_lflag == before_lflag
            cancel_observations.append(
                f"tty_restore={'true' if cancel_tty_restored else 'false'}; "
                f"before={before_lflag} after={after_lflag}"
            )
            cancel_raw_path.write_bytes(bytes(cancel_cli.buffer))
            cancel_plain_path.write_text(cancel_cli.plain(), encoding="utf-8")
            cancel_summary_path.write_text(
                "\n".join(cancel_observations + cancel_cli.snapshots) + "\n", encoding="utf-8"
            )
        if not cancel_tty_restored:
            raise AssertionError("cancel PTY did not restore its original terminal flags")
        observations.append("cancel_pty=active approval canceled before write dispatch; terminal restored")

        requests = provider.snapshot()
        if not requests:
            raise AssertionError("fake provider received no requests")
        if any(not record["auth_ok"] for record in requests):
            raise AssertionError("one or more provider requests did not use the synthetic canary credential")
        if any(not record["path"].endswith("/chat/completions") for record in requests):
            raise AssertionError("CLI attempted an unexpected provider route")
        if not all(record["stream"] for record in requests):
            observations.append("provider_streaming=" + repr([record["stream"] for record in requests]))
        observations.append(f"fake_provider_requests={len(requests)}; all local; all canary-authenticated")
        observations.append("provider_requests=" + json.dumps(requests, ensure_ascii=False, indent=2))
        observations.append(f"source_commit={args.source_commit}")
        observations.append("execution_limits=HOME/XDG/CWD/PATH isolated; only writes under temporary workspace")
        observations.append("limitation=two Darwin PTYs with local fake provider; no R2 crash/restart or cross-platform claim")
        observations.extend(cli.snapshots)
        raw_path.write_bytes(bytes(cli.buffer))
        plain_path.write_text(cli.plain(), encoding="utf-8")
        summary_path.write_text("\n".join(observations) + "\n", encoding="utf-8")

    provider.shutdown()
    provider.server_close()
    server_thread.join(timeout=2)
    findings = [
        item for item in observations
        if "first_post_submit_key=q had no visible effect" in item
        or "first_post_submit_key=n had no visible effect" in item
        or "deny_first_key=n was swallowed" in item
        or item.startswith("ask_user_answer_mismatch=")
    ]
    if findings:
        observations.append("result=FAIL")
        observations.append("confirmed_findings=" + json.dumps(findings, ensure_ascii=False))
        summary_path.write_text("\n".join(observations + cli.snapshots) + "\n", encoding="utf-8")
    status = "FAIL" if findings else "PASS"
    print(f"{status} source={args.source_commit} exit={exit_code} requests={len(provider.snapshot())}")
    print(f"raw={raw_path}\nplain={plain_path}\nsummary={summary_path}")
    print(f"cancel_raw={cancel_raw_path}\ncancel_plain={cancel_plain_path}\ncancel_summary={cancel_summary_path}")
    if findings:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
