#!/usr/bin/env python3
"""Bounded native PTY transcript journey using a local deterministic provider."""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import http.server
import json
import os
import pathlib
import pty
import re
import select
import signal
import struct
import subprocess
import tempfile
import termios
import threading
import time

import pyte


SOURCE_COMMIT = "eed9fff60bb552aab1d4d3fb1261a0d2a4ee942b"
CANARY_KEY = "r8-local-provider-fixture-only"
ANSI_RE = re.compile(
    rb"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\)|[@-Z\\-_])"
)


class Provider(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address):
        super().__init__(address, ProviderHandler)
        self.requests = []
        self.lock = threading.Lock()
        self.workspace = None
        self.active_stream_started = threading.Event()
        self.active_stream_rendered = threading.Event()
        self.provider_event_log = []
        self.active_stream_continue = threading.Event()
        self.active_stream_second_sent = threading.Event()
        self.active_stream_continue_again = threading.Event()
        self.active_stream_third_sent = threading.Event()
        self.active_stream_final_continue = threading.Event()
        self.active_stream_final_sent = threading.Event()
        self.active_stream_finish = threading.Event()

    def log_provider_event(self, event, marker, flushed):
        with self.lock:
            self.provider_event_log.append({
                "event": event,
                "marker": marker,
                "flushed": flushed,
                "monotonic_ns": time.monotonic_ns(),
                "wall_time": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
            })


class ProviderHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format, *_args):
        return

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        try:
            body = json.loads(self.rfile.read(length))
        except Exception as exc:  # pragma: no cover - fixture diagnostics
            self.send_error(400, str(exc))
            return

        messages = body.get("messages", [])
        last_user_index = next(
            (index for index in range(len(messages) - 1, -1, -1)
             if messages[index].get("role") == "user"),
            -1,
        )
        last_user = messages[last_user_index].get("content", "") if last_user_index >= 0 else ""
        recent_tool_results = [
            message for message in messages[last_user_index + 1:]
            if message.get("role") == "tool"
        ]
        calls = []
        if last_user == "R8 tool receipt" and not recent_tool_results:
            calls = [tool_call("r8-read-call", "read", {"path": str(self.server.workspace / "tool-source.txt")})]
        answer = answer_for(last_user)
        record = {
            "path": self.path,
            "auth_ok": self.headers.get("Authorization", "") == "Bearer " + CANARY_KEY,
            "stream": bool(body.get("stream")),
            "last_user": last_user,
            "tool_results_after_latest_user": len(recent_tool_results),
            "response_tools": [call["function"]["name"] for call in calls],
        }
        with self.server.lock:
            self.server.requests.append(record)
        if not record["auth_ok"]:
            self.send_error(401, "fixture credential mismatch")
            return
        if not self.path.endswith("/chat/completions"):
            self.send_error(404, "unexpected fixture route")
            return
        if body.get("stream"):
            self.write_stream(calls, answer, last_user)
        else:
            self.write_json(calls, answer)

    def write_json(self, calls, answer):
        if calls:
            message = {"role": "assistant", "content": None, "tool_calls": calls}
            finish_reason = "tool_calls"
        else:
            message = {"role": "assistant", "content": answer}
            finish_reason = "stop"
        payload = json.dumps({"choices": [{"index": 0, "message": message,
                                            "finish_reason": finish_reason}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)
        self.close_connection = True

    def write_stream(self, calls, answer, last_user):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        self.chunk({"role": "assistant"}, None)
        if calls:
            for index, call in enumerate(calls):
                delta = dict(call)
                delta["index"] = index
                self.chunk({"tool_calls": [delta]}, None)
            self.chunk({}, "tool_calls")
        elif last_user == "R8 active stream":
            self.chunk({"content": "R8-ACTIVE-PARTIAL initial live marker. "}, None, "R8-ACTIVE-PARTIAL")
            self.server.active_stream_started.set()
            # Keep the client's ordinary chunk-idle budget alive using real,
            # non-empty provider deltas. The client timeout remains unchanged.
            for index in range(1, 9):
                if self.server.active_stream_rendered.wait(1.5):
                    break
                marker = f"R8-RENDER-KEEPALIVE-{index} "
                self.chunk({"content": marker}, None, marker.strip())
            if not self.server.active_stream_rendered.wait(0.2):
                self.server.log_provider_event("render_wait_expired", "", flushed=False)
                return
            for gate, marker, sent in (
                (self.server.active_stream_continue, "R8-CHUNK-2 ", self.server.active_stream_second_sent),
                (self.server.active_stream_continue_again, "R8-CHUNK-3 ", self.server.active_stream_third_sent),
            ):
                while not gate.wait(2):
                    # Empty chunks keep the CLI's chunk-idle deadline alive
                    # while the driver performs real terminal input.
                    self.chunk({}, None)
                self.chunk({"content": marker}, None, marker.strip())
                sent.set()
            while not self.server.active_stream_final_continue.wait(2):
                self.chunk({}, None)
            for offset in range(0, len(answer), 12):
                self.chunk({"content": answer[offset:offset + 12]}, None, answer[offset:offset + 12])
            self.server.active_stream_final_sent.set()
            if not self.server.active_stream_finish.wait(30):
                raise TimeoutError("driver did not finish the bounded active stream")
            self.chunk({}, "stop")
        else:
            for offset in range(0, len(answer), 12):
                self.chunk({"content": answer[offset:offset + 12]}, None)
            self.chunk({}, "stop")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True

    def chunk(self, delta, finish_reason, marker=""):
        payload = {
            "id": "r8-native-pty-fixture",
            "object": "chat.completion.chunk",
            "created": 1,
            "model": "r8-local-fixture",
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish_reason}],
        }
        self.wfile.write(b"data: " + json.dumps(payload, ensure_ascii=False).encode() + b"\n\n")
        self.wfile.flush()
        if marker:
            self.server.log_provider_event("chunk_flushed", marker, flushed=True)


def tool_call(call_id, name, arguments):
    return {
        "id": call_id,
        "type": "function",
        "function": {"name": name, "arguments": json.dumps(arguments, ensure_ascii=False)},
    }


def answer_for(user_text):
    match = re.fullmatch(r"R8 seed (\d{2})", user_text)
    if match:
        return f"R8-ANSWER-{match.group(1)} durable viewport marker."
    if user_text == "R8 append while scrolled":
        return "R8-ANSWER-APPEND newest durable viewport marker."
    if user_text == "R8 active stream":
        return "R8-FINAL-CHUNK."
    if user_text == "R8 tool receipt":
        return "R8-ANSWER-TOOL final answer after tool receipt."
    return "R8-ANSWER-OTHER deterministic response."


class PtyCLI:
    def __init__(self, binary, cwd, env, rows=24, columns=80):
        self.master, self.slave = pty.openpty()
        self.original_termios = termios.tcgetattr(self.slave)
        self.final_termios = None
        self.buffer = bytearray()
        self.rows, self.columns = rows, columns
        self.closed = False
        self.screen = pyte.Screen(columns, rows)
        self.stream = pyte.Stream(self.screen)
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

    def set_size(self, rows, columns, send_signal=True):
        fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, columns, 0, 0))
        self.screen.resize(rows, columns)
        self.rows, self.columns = rows, columns
        if send_signal and self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGWINCH)

    def pump(self, timeout=0.05):
        ready, _, _ = select.select([self.master], [], [], timeout)
        if not ready:
            return b""
        try:
            data = os.read(self.master, 65536)
        except OSError:
            return b""
        if data:
            self.buffer.extend(data)
            self.stream.feed(data.decode("utf-8", "replace"))
        return data

    def wait_for(self, needle, start=0, timeout=10.0):
        target = needle.encode("utf-8") if isinstance(needle, str) else needle
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if target in self.buffer[start:]:
                return len(self.buffer)
            if self.process.poll() is not None:
                raise AssertionError(f"CLI exited {self.process.returncode} before {needle!r}")
            self.pump(min(0.1, deadline - time.monotonic()))
        raise AssertionError(f"timeout waiting for {needle!r}; recent={self.plain()[-2500:]!r}")

    def wait_prompt(self, start=0, timeout=10.0):
        return self.wait_for(b"\x1b[?2004h> ", start=start, timeout=timeout)

    def send(self, value, settle=0.15):
        os.write(self.master, value.encode("utf-8") if isinstance(value, str) else value)
        deadline = time.monotonic() + settle
        while time.monotonic() < deadline:
            self.pump(min(0.05, deadline - time.monotonic()))

    def resize_and_wait(self, rows, columns):
        before = len(self.buffer)
        self.set_size(rows, columns)
        self.wait_for_output(before, timeout=3.0)

    def wait_for_output(self, before, timeout):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if len(self.buffer) > before:
                return
            if self.process.poll() is not None:
                raise AssertionError(f"CLI exited {self.process.returncode} during redraw")
            self.pump(min(0.1, deadline - time.monotonic()))
        raise AssertionError("PTY did not redraw after resize/scroll event")

    def wait_frame_for(self, needle, timeout=10.0):
        deadline = time.monotonic() + timeout
        frame = self.frame()
        while time.monotonic() < deadline:
            frame = self.frame()
            if needle in frame:
                return frame
            if self.process.poll() is not None:
                raise AssertionError(f"CLI exited {self.process.returncode} before rendered {needle!r}")
            self.pump(min(0.1, deadline - time.monotonic()))
        raise AssertionError(f"timeout waiting for rendered {needle!r}; frame={frame!r}")

    def frame(self):
        return "\n".join(self.screen.display).rstrip()

    def plain(self):
        return ANSI_RE.sub(b"", bytes(self.buffer)).decode("utf-8", "replace")

    def close(self):
        if self.closed:
            return
        self.closed = True
        if self.process.poll() is None:
            try:
                os.killpg(self.process.pid, signal.SIGTERM)
            except OSError:
                self.process.terminate()
            try:
                self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(self.process.pid, signal.SIGKILL)
                except OSError:
                    self.process.kill()
                self.process.wait(timeout=3)
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
    parser.add_argument("--binary", required=True)
    parser.add_argument("--evidence-dir", required=True)
    parser.add_argument("--source-commit", default=SOURCE_COMMIT)
    args = parser.parse_args()
    binary = pathlib.Path(args.binary).resolve()
    evidence = pathlib.Path(args.evidence_dir).resolve()
    evidence.mkdir(parents=True, exist_ok=True)
    if not binary.is_file():
        raise SystemExit(f"CLI binary does not exist: {binary}")

    provider = Provider(("127.0.0.1", 0))
    provider_thread = threading.Thread(target=provider.serve_forever, daemon=True)
    provider_thread.start()
    observations = []
    cli = None
    success = False
    try:
        with tempfile.TemporaryDirectory(prefix="go-cli-r8-pty-") as temporary:
            root = pathlib.Path(temporary)
            home, xdg, workspace = root / "home", root / "xdg", root / "workspace"
            for path in (home / ".go-cli", xdg, workspace, root / "cache", root / "state", root / "data"):
                path.mkdir(parents=True, exist_ok=True)
            provider.workspace = workspace
            (workspace / "tool-source.txt").write_text("R8_TOOL_PANE_MARKER structured read output\n", encoding="utf-8")
            host, port = provider.server_address
            base_url = f"http://{host}:{port}/v1"
            config = (
                "provider:\n"
                "  name: openai\n"
                f"  api_key: {CANARY_KEY}\n"
                f"  base_url: {base_url}\n"
                "  model: r8-local-fixture\n"
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
            cli = PtyCLI(str(binary), workspace, env)
            cli.wait_prompt(timeout=15)
            observations.append("startup_prompt=observed at 80x24")

            for index in range(24):
                user = "R8 tool receipt" if index == 12 else f"R8 seed {index:02d}"
                expected = "R8-ANSWER-TOOL" if index == 12 else f"R8-ANSWER-{index:02d}"
                start = len(cli.buffer)
                cli.send(user + "\r", settle=0.1)
                cli.wait_for(expected, start=start, timeout=12)
                cli.wait_prompt(start=start, timeout=12)
            observations.append("seed_turns=24 deterministic user turns, including one real read-tool receipt cycle")

            # Keep a real Bubble Tea run active while scrolling/resizing. The
            # fake provider pauses an incremental response at a deterministic
            # boundary so no wall-clock delay stands in for a render event.
            active_start = len(cli.buffer)
            cli.send("R8 active stream\r", settle=0.1)
            if not provider.active_stream_started.wait(10):
                raise AssertionError("local provider did not pause the active stream")
            try:
                initial_frame = cli.wait_frame_for("R8-ACTIVE-PARTIAL", timeout=11)
            except AssertionError as exc:
                provider_events = list(provider.provider_event_log)
                raw = bytes(cli.buffer[active_start:])
                current_frame = cli.frame()
                (evidence / "r8-pty-active-render-observation.txt").write_text(
                    "provider_event_log=" + json.dumps(provider_events, ensure_ascii=False, indent=2) + "\n"
                    + f"provider_initial_chunk_flushed={any(e['marker'] == 'R8-ACTIVE-PARTIAL' and e['flushed'] for e in provider_events)}\n"
                    + f"provider_nonempty_keepalives_flushed={sum(e['marker'].startswith('R8-RENDER-KEEPALIVE-') for e in provider_events)}\n"
                    + f"cli_pty_raw_contains_initial_marker={b'R8-ACTIVE-PARTIAL' in raw}\n"
                    + f"cli_tui_current_frame_contains_initial_marker={'R8-ACTIVE-PARTIAL' in current_frame}\n"
                    + f"cli_process_alive_after_render_wait={cli.process.poll() is None}\n"
                    + f"wait_error={exc}\n\nCURRENT_FRAME:\n{current_frame}\n\nRAW_ACTIVE_INTERVAL:\n{raw.decode('utf-8', 'replace')}\n",
                    encoding="utf-8",
                )
                raise
            provider.active_stream_rendered.set()
            if cli.process.poll() is not None:
                raise AssertionError(f"CLI exited {cli.process.returncode} while the provider held the active stream")
            observations.append(
                "active_stream_initial_text=R8-ACTIVE-PARTIAL visible in decoded PTY frame while provider withheld later chunks"
            )
            (evidence / "r8-pty-active-initial-frame.txt").write_text(initial_frame + "\n", encoding="utf-8")

            scroll_before = len(cli.buffer)
            for _ in range(12):
                cli.send(b"\x1b[5~", settle=0.12)
                frame = cli.frame()
                if "R8-ANSWER-23" not in frame and "R8-ACTIVE-PARTIAL" not in frame:
                    break
            cli.wait_for_output(scroll_before, timeout=3)
            frame = cli.frame()
            visible_answers = re.findall(r"R8-ANSWER-\d{2}", frame)
            if not visible_answers:
                raise AssertionError(f"PageUp did not reveal a previous canonical assistant row:\n{frame}")
            anchor = visible_answers[-1]
            if "R8-ANSWER-23" in frame or "R8-ACTIVE-PARTIAL" in frame:
                raise AssertionError("PageUp in the active Bubble Tea run did not leave the live stream tail")
            observations.append(f"active_scroll_away=PageUp; visible anchor token={anchor!r}; newest seeded row not visible")

            frames = []
            for columns in (60, 80, 120, 200):
                cli.resize_and_wait(24, columns)
                frame = cli.frame()
                if anchor not in frame:
                    raise AssertionError(f"resize to {columns} columns lost visible anchor {anchor!r}:\n{frame}")
                has_split = "│" in frame
                if has_split != (columns >= 120):
                    raise AssertionError(f"split-pane state at {columns} columns was {has_split}, expected {columns >= 120}")
                if columns >= 120 and "R8_TOOL_PANE_MARKER" not in frame:
                    raise AssertionError(f"wide layout omitted the independent tool pane content at {columns} columns:\n{frame}")
                frames.append(f"--- {columns}x24 scrolled; split={has_split}; anchor={anchor} ---\n{frame}\n")
                observations.append(f"resize={columns}x24; anchor preserved; split={has_split}")

            cli.send(b"\x1b[4~", settle=0.25)
            immediate_end_frame = cli.wait_frame_for("R8-ACTIVE-PARTIAL", timeout=5)
            if provider.active_stream_continue.is_set():
                raise AssertionError("provider sent later content before the immediate End check")
            observations.append("immediate_end=End revealed the already-rendered active partial before later chunks")
            (evidence / "r8-pty-immediate-end-frame.txt").write_text(immediate_end_frame + "\n", encoding="utf-8")

            provider.active_stream_continue.set()
            if not provider.active_stream_second_sent.wait(10):
                raise AssertionError("provider did not send active chunk 2")
            chunk2_frame = cli.wait_frame_for("R8-CHUNK-2", timeout=8)
            if provider.active_stream_continue_again.is_set():
                raise AssertionError("provider advanced past chunk 2 before its frame was captured")
            observations.append("follow_tail_after_end=R8-CHUNK-2 appeared while the stream remained active")
            (evidence / "r8-pty-after-chunk-2-frame.txt").write_text(chunk2_frame + "\n", encoding="utf-8")

            for _ in range(12):
                cli.send(b"\x1b[5~", settle=0.12)
                frame = cli.frame()
                if "R8-CHUNK-2" not in frame:
                    break
            if "R8-CHUNK-2" in cli.frame():
                raise AssertionError("second PageUp did not leave the active stream tail")

            provider.active_stream_continue_again.set()
            if not provider.active_stream_third_sent.wait(10):
                raise AssertionError("provider did not send active chunk 3")
            cli.send(b"\x1b[4~", settle=0.25)
            chunk3_frame = cli.wait_frame_for("R8-CHUNK-3", timeout=8)
            if provider.active_stream_final_continue.is_set() or provider.active_stream_final_sent.is_set():
                raise AssertionError("provider completed the response before the active End frame was captured")
            observations.append("end_during_more_chunks=End revealed R8-CHUNK-3 while the provider still held the stream")
            (evidence / "r8-pty-end-during-more-chunks-frame.txt").write_text(chunk3_frame + "\n", encoding="utf-8")

            provider.active_stream_final_continue.set()
            if not provider.active_stream_final_sent.wait(10):
                raise AssertionError("provider did not send the final response chunks")
            final_active_frame = cli.wait_frame_for("R8-FINAL-CHUNK", timeout=10)
            if provider.active_stream_finish.is_set():
                raise AssertionError("provider closed the stream before final active text was captured")
            observations.append("follow_tail_after_end=R8-FINAL-CHUNK appeared before stream completion")
            (evidence / "r8-pty-follow-tail-before-completion-frame.txt").write_text(final_active_frame + "\n", encoding="utf-8")

            (evidence / "r8-pty-resize-frames.txt").write_text("\n".join(frames), encoding="utf-8")
            provider.active_stream_finish.set()
            try:
                cli.wait_for("R8-FINAL-CHUNK", start=active_start, timeout=12)
                cli.wait_prompt(start=active_start, timeout=12)
            except AssertionError as exc:
                observations.append(f"active_turn_completion={type(exc).__name__}: {exc}")

            final_frame = cli.frame()
            (evidence / "r8-pty-after-completion-200x24.txt").write_text(final_frame + "\n", encoding="utf-8")
            if "R8-FINAL-CHUNK" not in final_frame:
                raise AssertionError("final streamed answer was not visible after the provider completed")
            if "R8_TOOL_PANE_MARKER" not in final_frame:
                raise AssertionError(f"final frame lost the durable read tool row:\n{final_frame}")
            observations.append("stream_completion=final answer and read-tool pane visible after the active stream closed")

            # Save the exact bytes from the native PTY for independent replay.
            (evidence / "r8-pty-transcript.raw").write_bytes(bytes(cli.buffer))
            (evidence / "r8-pty-transcript.txt").write_text(cli.plain(), encoding="utf-8")

            before_lflag = cli.original_termios[3] & (termios.ICANON | termios.ECHO | termios.ISIG)
            cli.send("/exit\r", settle=0.1)
            deadline = time.monotonic() + 12
            while cli.process.poll() is None and time.monotonic() < deadline:
                cli.pump(0.05)
            if cli.process.poll() is None:
                raise AssertionError("CLI did not exit after /exit")
            if cli.process.returncode != 0:
                raise AssertionError(f"CLI exited with {cli.process.returncode}")
            cli.close()
            after_lflag = cli.final_termios[3] & (termios.ICANON | termios.ECHO | termios.ISIG) if cli.final_termios else None
            if after_lflag != before_lflag:
                raise AssertionError(f"terminal state was not restored: before={before_lflag}, after={after_lflag}")
            observations.append("exit=/exit status=0; ICANON/ECHO/ISIG restored")

            requests = list(provider.requests)
            if len(requests) != 26:
                raise AssertionError(f"expected 26 local provider requests, got {len(requests)}")
            if any(not request["auth_ok"] or not request["path"].endswith("/chat/completions") for request in requests):
                raise AssertionError("provider request auth or route did not match the local fixture")
            if any(not request["stream"] for request in requests):
                raise AssertionError("expected the actual CLI to stream every deterministic response")
            if not any(request["response_tools"] == ["read"] for request in requests):
                raise AssertionError("provider did not receive the requested read tool cycle")
            observations.append("provider=26 requests; all local/canary authenticated/streamed; one read tool cycle")
            observations.append("source_commit=" + args.source_commit)
            observations.append("binary_sha256=" + hashlib.sha256(binary.read_bytes()).hexdigest())
            observations.append("pty=Darwin native openpty; TERM=xterm-256color; LANG/LC_CTYPE=en_US.UTF-8; NO_COLOR=1")
            observations.append("geometry=60x24, 80x24, 120x24, 200x24; real SIGWINCH resize and key input")
            observations.append("memory_scope=bounded 25-turn REPL fixture; 1MiB/1000-chunk workload measured separately by actual App.Update/View benchmark")
            observations.append("unsupported=R8-BLD-02 capability/help/doctor/persistence promises not exercised or claimed")
            success = True
    except Exception as exc:
        observations.append(f"error={type(exc).__name__}: {exc}")
        if cli is not None:
            (evidence / "r8-pty-failure-frame.txt").write_text(cli.frame() + "\n", encoding="utf-8")
        raise
    finally:
        provider.active_stream_continue.set()
        provider.active_stream_continue_again.set()
        provider.active_stream_final_continue.set()
        provider.active_stream_finish.set()
        if cli is not None:
            (evidence / "r8-pty-transcript.raw").write_bytes(bytes(cli.buffer))
            (evidence / "r8-pty-transcript.txt").write_text(cli.plain(), encoding="utf-8")
            try:
                cli.close()
            except Exception as close_exc:  # pragma: no cover - failure cleanup
                observations.append(f"cleanup_error={type(close_exc).__name__}: {close_exc}")
        provider.shutdown()
        provider.server_close()
        provider_thread.join(timeout=2)
        observations.append("provider_requests=" + json.dumps(provider.requests, ensure_ascii=False, indent=2))
        observations.append("provider_event_log=" + json.dumps(provider.provider_event_log, ensure_ascii=False, indent=2))
        (evidence / "r8-pty-summary.txt").write_text("\n".join(observations) + "\n", encoding="utf-8")

    print(f"{'PASS' if success else 'FAIL'} source={args.source_commit} requests={len(provider.requests)}")
    print(f"summary={evidence / 'r8-pty-summary.txt'}")
    if not success:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
