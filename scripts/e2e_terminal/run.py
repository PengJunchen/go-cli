#!/usr/bin/env python3
"""Drive the real go-cli interactive binary through a PTY and capture xterm.js."""

from __future__ import annotations

import argparse
import base64
import errno
import fcntl
import hashlib
import http.server
import json
import os
import pty
import re
import select
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time
import traceback
from pathlib import Path
from typing import Any


HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
DEFAULT_EVIDENCE = REPO / "docs" / "qa" / "2026-10-14" / "terminal"
FIXTURE_KEY = "terminal-e2e-fixture-key"
STARTUP_TEXT = "Interactive session started. Type 'exit' to quit."
FOLLOWUP_TEXT = "During the active run, keep xterm colors stable and inspect 日志窗口。"
LONG_COMMAND = "\n".join(
    [
        "set -e",
        "printf 'E2E_STAGE_ONE\\n' | tee stage-one.done",
        "sleep 6",
        "printf 'E2E_STAGE_TWO\\n' | tee stage-two.done",
        "sleep 2.5",
        "printf 'E2E_STAGE_THREE\\n' | tee stage-three.done",
        *[
            f"# approval detail row {index:02d} keeps this command longer than the visible review window"
            for index in range(1, 36)
        ],
    ]
)
STAGE_FILES = ("stage-one.done", "stage-two.done", "stage-three.done")


class FixtureHTTPServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, scenario: str):
        self.scenario = scenario
        self.lock = threading.Lock()
        self.request_count = 0
        self.fixture_auth_seen = False
        self.denial_receipt_seen = False
        self.long_output_receipt_seen = False
        self.stage_one_output_seen = False
        self.stage_two_output_seen = False
        self.stage_three_output_seen = False
        self.tool_receipts: list[dict[str, Any]] = []
        self.proposed_tool_name = ""
        self.proposed_command = ""
        self.tool_call_id = ""
        self.tool_response_sha256 = ""
        self.followup_received = False
        self.followup_messages: list[str] = []
        super().__init__(("127.0.0.1", 0), FixtureHandler)

    def next_response(self, handler: FixtureHandler, request: dict[str, Any]) -> None:
        with self.lock:
            self.request_count += 1
            request_number = self.request_count
            messages = request.get("messages", [])
            for message in messages:
                if message.get("role") == "user":
                    content = str(message.get("content", ""))
                    if FOLLOWUP_TEXT in content:
                        self.followup_received = True
                        self.followup_messages.append(content[:500])
            tool_messages = [message for message in messages if message.get("role") == "tool"]
            tool_text = " ".join(str(message.get("content", "")) for message in tool_messages).lower()
            recorded_call_ids = {receipt["tool_call_id"] for receipt in self.tool_receipts}
            for message in tool_messages:
                call_id = str(message.get("tool_call_id", ""))
                if call_id not in recorded_call_ids:
                    content = str(message.get("content", ""))
                    normalized_content = content.lower()
                    self.tool_receipts.append(
                        {
                            "tool_call_id": call_id,
                            "tool_name": str(message.get("name", "")),
                            "content_excerpt": content[:500],
                            "content_bytes": len(content.encode("utf-8")),
                            "content_sha256": hashlib.sha256(content.encode("utf-8")).hexdigest(),
                            "stage_markers_seen": [
                                "e2e_stage_one" in normalized_content,
                                "e2e_stage_two" in normalized_content,
                                "e2e_stage_three" in normalized_content,
                            ],
                        }
                    )
                    recorded_call_ids.add(call_id)
            self.denial_receipt_seen |= "denied" in tool_text
            self.stage_one_output_seen |= "e2e_stage_one" in tool_text
            self.stage_two_output_seen |= "e2e_stage_two" in tool_text
            self.stage_three_output_seen |= "e2e_stage_three" in tool_text
            self.long_output_receipt_seen |= self.stage_one_output_seen and self.stage_two_output_seen and self.stage_three_output_seen
            for message in messages:
                if message.get("role") != "assistant":
                    continue
                for call in message.get("tool_calls", []):
                    function = call.get("function", {})
                    self.proposed_tool_name = str(function.get("name", ""))
                    try:
                        arguments = json.loads(function.get("arguments", "{}"))
                    except (TypeError, json.JSONDecodeError):
                        arguments = {}
                    self.proposed_command = str(arguments.get("command", ""))
            self.fixture_auth_seen |= handler.headers.get("Authorization") == f"Bearer {FIXTURE_KEY}"

        if self.scenario == "answer":
            self._stream_answer(handler)
            return
        if request_number == 1:
            self._tool_call(handler)
        else:
            self._stream_final(handler, self.scenario)

    @staticmethod
    def _write_json(handler: FixtureHandler, value: dict[str, Any]) -> None:
        payload = json.dumps(value, separators=(",", ":")).encode("utf-8")
        handler.send_response(200)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(payload)))
        handler.send_header("Connection", "close")
        handler.end_headers()
        handler.wfile.write(payload)
        handler.wfile.flush()

    @staticmethod
    def _write_sse(handler: FixtureHandler, content_parts: list[str], delay: float) -> None:
        handler.send_response(200)
        handler.send_header("Content-Type", "text/event-stream")
        handler.send_header("Cache-Control", "no-cache")
        handler.send_header("Connection", "close")
        handler.end_headers()
        for part in content_parts:
            data = {
                "id": "chatcmpl-local-fixture",
                "object": "chat.completion.chunk",
                "choices": [{"index": 0, "delta": {"content": part}, "finish_reason": None}],
            }
            handler.wfile.write(
                b"data: " + json.dumps(data, separators=(",", ":"), ensure_ascii=False).encode("utf-8") + b"\n\n"
            )
            handler.wfile.flush()
            if delay:
                time.sleep(delay)
        final = {
            "id": "chatcmpl-local-fixture",
            "object": "chat.completion.chunk",
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
        }
        handler.wfile.write(b"data: " + json.dumps(final, separators=(",", ":")).encode() + b"\n\n")
        handler.wfile.write(b"data: [DONE]\n\n")
        handler.wfile.flush()

    def _stream_answer(self, handler: FixtureHandler) -> None:
        self._write_sse(
            handler,
            ["这是", "本地", "模拟", "模型", "逐段", "返回", "的中文", "答案。"],
            delay=0.34,
        )

    def _tool_call(self, handler: FixtureHandler) -> None:
        arguments = json.dumps({"command": LONG_COMMAND}, separators=(",", ":"))
        response = {
            "id": "chatcmpl-local-tool-fixture",
            "object": "chat.completion",
            "model": "fixture-model",
            "choices": [
                {
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": None,
                        "tool_calls": [
                            {
                                "id": "call-terminal-e2e",
                                "type": "function",
                                "function": {"name": "bash", "arguments": arguments},
                            }
                        ],
                    },
                    "finish_reason": "tool_calls",
                }
            ],
        }
        response_bytes = json.dumps(response, separators=(",", ":")).encode("utf-8")
        with self.lock:
            self.proposed_tool_name = "bash"
            self.proposed_command = LONG_COMMAND
            self.tool_call_id = "call-terminal-e2e"
            self.tool_response_sha256 = hashlib.sha256(response_bytes).hexdigest()
        self._write_json(handler, response)

    def _stream_final(self, handler: FixtureHandler, scenario: str) -> None:
        if scenario == "deny":
            response = "本地模拟器收到拒绝凭据，命令没有运行。"
        else:
            response = (
                "本地模拟器确认三阶段 shell 命令全部完成，并收到 follow-up 指令。"
                if self.followup_received
                else "本地模拟器确认三阶段 shell 命令全部完成。"
            )
        self._write_sse(handler, [response], delay=0)


class FixtureHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if size <= 0 or size > 2_000_000:
                self.send_error(413)
                return
            body = self.rfile.read(size)
            request = json.loads(body)
            if not isinstance(request, dict):
                self.send_error(400)
                return
        except (ValueError, json.JSONDecodeError):
            self.send_error(400)
            return
        self.server.next_response(self, request)  # type: ignore[attr-defined]

    def log_message(self, _format: str, *_args: object) -> None:
        # Request headers and bodies are intentionally never emitted to logs.
        return


class XtermRenderer:
    def __init__(self, node: str, helper: Path, env: dict[str, str]):
        self.proc = subprocess.Popen(
            [node, str(helper)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
            bufsize=1,
            env=env,
        )
        ready = self._read_response(time.monotonic() + 45)
        if not ready.get("ready"):
            raise RuntimeError(f"xterm renderer failed: {ready.get('error', 'no ready response')}")
        self.cols = int(ready["cols"])
        self.rows = int(ready["rows"])

    def _read_response(self, deadline: float) -> dict[str, Any]:
        assert self.proc.stdout is not None
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                error = self.proc.stderr.read() if self.proc.stderr else ""
                raise RuntimeError(f"xterm renderer exited ({self.proc.returncode}): {error[-1000:]}")
            ready, _, _ = select.select([self.proc.stdout], [], [], min(0.25, deadline - time.monotonic()))
            if ready:
                line = self.proc.stdout.readline()
                if line:
                    result = json.loads(line)
                    if not result.get("ok", True):
                        raise RuntimeError(f"xterm renderer error: {result.get('error', 'unknown error')}")
                    return result
        raise TimeoutError("timed out waiting for xterm renderer")

    def request(self, value: dict[str, Any], timeout: float = 20) -> dict[str, Any]:
        if self.proc.poll() is not None or self.proc.stdin is None:
            raise RuntimeError("xterm renderer is not running")
        self.proc.stdin.write(json.dumps(value, separators=(",", ":")) + "\n")
        self.proc.stdin.flush()
        result = self._read_response(time.monotonic() + timeout)
        if not result.get("ok", True):
            raise RuntimeError(f"xterm renderer error: {result.get('error', 'unknown error')}")
        return result

    def write(self, data: bytes) -> None:
        self.request({"op": "write", "data": base64.b64encode(data).decode("ascii")})

    def screen(self) -> list[str]:
        result = self.request({"op": "screen"})
        self.cols = int(result["cols"])
        self.rows = int(result["rows"])
        return list(result.get("lines", []))

    def snapshot(self, path: Path) -> dict[str, Any]:
        path.parent.mkdir(parents=True, exist_ok=True)
        result = self.request({"op": "snapshot", "path": str(path)})
        self.cols = int(result["cols"])
        self.rows = int(result["rows"])
        return result

    def resize(self, width: int, height: int) -> tuple[int, int]:
        result = self.request({"op": "resize", "width": width, "height": height})
        self.cols = int(result["cols"])
        self.rows = int(result["rows"])
        return self.cols, self.rows

    def close(self) -> None:
        if self.proc.poll() is None:
            try:
                self.request({"op": "close"}, timeout=5)
            except (OSError, RuntimeError, TimeoutError):
                self.proc.terminate()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait(timeout=3)


class InteractivePTY:
    def __init__(
        self,
        binary: Path,
        cwd: Path,
        home: Path,
        server: FixtureHTTPServer,
        node: str,
        renderer_path: Path,
        viewport: tuple[int, int] = (1440, 900),
    ):
        self.binary = binary
        self.cwd = cwd
        self.home = home
        self.server = server
        self.node = node
        self.renderer_path = renderer_path
        self.master_fd: int | None = None
        self.pid: int | None = None
        self.viewport = viewport
        self.node_env = self._renderer_env(viewport)
        self.renderer = XtermRenderer(node, renderer_path, self.node_env)
        self.raw_bytes = bytearray()
        child_env = self._child_env()
        pid, master_fd = pty.fork()
        if pid == 0:
            try:
                os.chdir(cwd)
                os.execvpe(
                    str(binary),
                    [str(binary), "interactive", "--no-onboarding", "--provider", "openai", "--model", "fixture-model", "--thinking", "none"],
                    child_env,
                )
            except BaseException as error:  # pragma: no cover - child failure path
                os.write(2, f"e2e exec failed: {type(error).__name__}\n".encode())
                os._exit(127)
        self.pid = pid
        self.master_fd = master_fd
        self._resize(self.renderer.cols, self.renderer.rows)
        self.server_thread = threading.Thread(target=self.server.serve_forever, name="terminal-e2e-mock", daemon=True)
        self.server_thread.start()

    @staticmethod
    def _renderer_env(viewport: tuple[int, int]) -> dict[str, str]:
        env = os.environ.copy()
        env["E2E_VIEWPORT_WIDTH"] = str(viewport[0])
        env["E2E_VIEWPORT_HEIGHT"] = str(viewport[1])
        return env

    def _child_env(self) -> dict[str, str]:
        env = {key: value for key, value in os.environ.items() if not key.startswith("GO_CLI_")}
        for name in (
            "OPENAI_API_KEY", "AZURE_OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY",
            "GEMINI_API_KEY", "MISTRAL_API_KEY", "XAI_API_KEY", "AWS_ACCESS_KEY_ID",
            "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "E2E_CHROME_PATH", "E2E_PLAYWRIGHT_MODULE",
        ):
            env.pop(name, None)
        # The host shell may set NO_COLOR for its own terminal. The harness
        # presents a normal xterm-256color surface and verifies its styling.
        env.pop("NO_COLOR", None)
        env.update(
            {
                "HOME": str(self.home),
                "XDG_CONFIG_HOME": str(self.home / ".config"),
                "TERM": "xterm-256color",
                "COLORTERM": "truecolor",
                "LANG": "en_US.UTF-8",
                "LC_ALL": "en_US.UTF-8",
                "GO_CLI_PROVIDER_NAME": "openai",
                "GO_CLI_PROVIDER_BASE_URL": f"http://127.0.0.1:{self.server.server_port}/v1",
                "GO_CLI_PROVIDER_API_KEY": FIXTURE_KEY,
                "GO_CLI_PROVIDER_MODEL": "fixture-model",
                "GO_CLI_MODEL_NAME": "fixture-model",
                "GO_CLI_TRACING_ENABLED": "false",
                "GO_CLI_EXTENSIONS_ENABLED": "false",
            }
        )
        return env

    def _resize(self, cols: int, rows: int) -> None:
        assert self.master_fd is not None
        fcntl.ioctl(self.master_fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        if self.pid:
            try:
                os.kill(self.pid, signal.SIGWINCH)
            except ProcessLookupError:
                pass

    def resize_viewport(self, width: int, height: int) -> dict[str, int]:
        cols, rows = self.renderer.resize(width, height)
        self.viewport = (width, height)
        self._resize(cols, rows)
        actual_rows, actual_cols, _, _ = struct.unpack(
            "HHHH", fcntl.ioctl(self.master_fd, termios.TIOCGWINSZ, b"\0" * 8)  # type: ignore[arg-type]
        )
        return {
            "viewport_width": width,
            "viewport_height": height,
            "xterm_cols": cols,
            "xterm_rows": rows,
            "pty_cols": actual_cols,
            "pty_rows": actual_rows,
        }

    def send(self, data: bytes) -> None:
        if self.master_fd is None:
            raise RuntimeError("PTY is closed")
        os.write(self.master_fd, data)

    def send_line(self, text: str) -> None:
        self.send(text.encode("utf-8") + b"\r")

    def pump(self, timeout: float = 0.1) -> bool:
        if self.master_fd is None:
            return False
        ready, _, _ = select.select([self.master_fd], [], [], timeout)
        if not ready:
            return False
        try:
            data = os.read(self.master_fd, 65536)
        except OSError as error:
            if error.errno in (errno.EIO, errno.EBADF):
                return False
            raise
        if data:
            self.raw_bytes.extend(data)
            self.renderer.write(data)
            return True
        return False

    @staticmethod
    def _normalize(lines: list[str]) -> str:
        return re.sub(r"\s+", " ", " ".join(lines)).strip()

    def wait_for(self, needle: str, timeout: float = 20) -> str:
        deadline = time.monotonic() + timeout
        last = ""
        while time.monotonic() < deadline:
            self.pump(min(0.1, max(0.01, deadline - time.monotonic())))
            last = self._normalize(self.renderer.screen())
            if needle.lower() in last.lower():
                return last
            if self._poll_exit() is not None:
                break
        excerpt = redact(last[-1200:])
        raise TimeoutError(f"timed out waiting for terminal text {needle!r}; recent screen: {excerpt}")

    def wait_for_any(self, needles: list[str], timeout: float = 20) -> tuple[str, str]:
        deadline = time.monotonic() + timeout
        last = ""
        while time.monotonic() < deadline:
            self.pump(min(0.1, max(0.01, deadline - time.monotonic())))
            last = self._normalize(self.renderer.screen())
            for needle in needles:
                if needle.lower() in last.lower():
                    return needle, last
            if self._poll_exit() is not None:
                break
        raise TimeoutError(f"timed out waiting for {needles!r}; recent screen: {redact(last[-1200:])}")

    def screenshot(
        self,
        name: str,
        output_dir: Path,
        expected: list[str] | None = None,
        require_color: bool = False,
        scenario: str | None = None,
    ) -> dict[str, Any]:
        path = output_dir / name
        capture = self.renderer.snapshot(path)
        lines = list(capture.get("lines", []))
        text = self._normalize(lines)
        image_width, image_height = png_dimensions(path)
        if (image_width, image_height) != self.viewport:
            raise AssertionError(
                f"screenshot {name} pixel dimensions {(image_width, image_height)} "
                f"do not match requested viewport {self.viewport}"
            )
        profile = self.terminal_profile()
        if profile["xterm_cols"] != profile["pty_cols"] or profile["xterm_rows"] != profile["pty_rows"]:
            raise AssertionError(f"screenshot {name} captured mismatched xterm and PTY dimensions: {profile}")
        missing = [needle for needle in (expected or []) if needle.lower() not in text.lower()]
        if missing:
            raise AssertionError(f"screenshot {name} is missing expected screen text: {missing}")
        if require_color and int(capture.get("colored_cells", 0)) == 0:
            raise AssertionError(f"screenshot {name} has no ANSI-colored terminal cells")
        return {
            "file": name,
            "scenario": scenario,
            "source": "Chrome headless rendering xterm.js fed with raw bytes read from the real binary PTY",
            "screen_excerpt": redact(text[-900:]),
            "assertions": {needle: needle.lower() in text.lower() for needle in (expected or [])},
            "terminal": {
                "viewport_width_px": self.viewport[0],
                "viewport_height_px": self.viewport[1],
                "image_width_px": image_width,
                "image_height_px": image_height,
                "xterm_cols": capture["cols"],
                "xterm_rows": capture["rows"],
                "pty_cols": profile["pty_cols"],
                "pty_rows": profile["pty_rows"],
                "colored_cells": capture["colored_cells"],
                "occupied_cells": capture["occupied_cells"],
            },
        }

    def terminal_profile(self) -> dict[str, int | bool]:
        rows, cols, _, _ = struct.unpack(
            "HHHH", fcntl.ioctl(self.master_fd, termios.TIOCGWINSZ, b"\0" * 8)  # type: ignore[arg-type]
        )
        return {
            "viewport_width": self.viewport[0],
            "viewport_height": self.viewport[1],
            "xterm_cols": self.renderer.cols,
            "xterm_rows": self.renderer.rows,
            "pty_cols": cols,
            "pty_rows": rows,
            "no_color_removed": "NO_COLOR" not in self._child_env(),
        }

    def save_raw_transcript(self, output_dir: Path, name: str) -> dict[str, str | int]:
        output_dir.mkdir(parents=True, exist_ok=True)
        content = bytes(self.raw_bytes).replace(FIXTURE_KEY.encode("utf-8"), b"[fixture key redacted]")
        path = output_dir / f"{name}.pty.ansi"
        path.write_bytes(content)
        return {
            "file": path.name,
            "bytes": len(content),
            "sha256": hashlib.sha256(content).hexdigest(),
            "source": "raw bytes read from the real CLI PTY before xterm.js rendering",
        }

    def wait_for_condition(self, predicate: Any, description: str, timeout: float = 20) -> tuple[Any, str]:
        deadline = time.monotonic() + timeout
        last = ""
        while time.monotonic() < deadline:
            self.pump(min(0.1, max(0.01, deadline - time.monotonic())))
            last = self._normalize(self.renderer.screen())
            match = predicate(last)
            if match:
                return match, last
            if self._poll_exit() is not None:
                break
        raise TimeoutError(f"timed out waiting for {description}; recent screen: {redact(last[-1200:])}")

    def _poll_exit(self) -> int | None:
        if self.pid is None:
            return 0
        try:
            pid, status = os.waitpid(self.pid, os.WNOHANG)
        except ChildProcessError:
            return 0
        if pid == 0:
            return None
        self.pid = None
        return os.waitstatus_to_exitcode(status)

    def finish_session(self) -> int:
        if self.pid is None:
            return 0
        self.send_line("exit")
        deadline = time.monotonic() + 12
        last = ""
        while time.monotonic() < deadline:
            self.pump(0.1)
            last = self._normalize(self.renderer.screen())
            status = self._poll_exit()
            if status is not None:
                return status
            if "session ended" in last.lower():
                continue
        try:
            os.write(self.master_fd, b"\x03")  # type: ignore[arg-type]
        except OSError:
            pass
        deadline = time.monotonic() + 4
        while time.monotonic() < deadline:
            self.pump(0.1)
            status = self._poll_exit()
            if status is not None:
                return status
        if self.pid is not None:
            try:
                os.killpg(self.pid, signal.SIGTERM)
            except (ProcessLookupError, PermissionError):
                try:
                    os.kill(self.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
        return self._wait_child(4)

    def _wait_child(self, timeout: float) -> int:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            status = self._poll_exit()
            if status is not None:
                return status
            time.sleep(0.05)
        if self.pid is not None:
            try:
                os.kill(self.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        return self._poll_exit() or 0

    def close(self) -> None:
        if self.pid is not None:
            self.finish_session()
        if self.master_fd is not None:
            try:
                os.close(self.master_fd)
            except OSError:
                pass
            self.master_fd = None
        self.renderer.close()
        self.server.shutdown()
        self.server.server_close()
        self.server_thread.join(timeout=3)


def redact(text: str) -> str:
    return text.replace(FIXTURE_KEY, "[fixture key redacted]")


def png_dimensions(path: Path) -> tuple[int, int]:
    header = path.read_bytes()[:24]
    if len(header) != 24 or header[:8] != b"\x89PNG\r\n\x1a\n" or header[12:16] != b"IHDR":
        raise ValueError(f"screenshot is not a valid PNG: {path.name}")
    return struct.unpack(">II", header[16:24])


def locate_node() -> str:
    candidates = [
        os.environ.get("E2E_NODE", ""),
        shutil.which("node") or "",
    ]
    for candidate in candidates:
        if candidate and os.access(candidate, os.X_OK):
            return candidate
    raise RuntimeError("Node.js not found; set E2E_NODE to the Node executable")


def locate_binary(binary_arg: str | None, build_dir: Path) -> tuple[Path, bool]:
    if binary_arg:
        binary = Path(binary_arg).expanduser().resolve()
        if not binary.is_file() or not os.access(binary, os.X_OK):
            raise RuntimeError(f"binary is not executable: {binary}")
        return binary, False
    binary = build_dir / "go-cli"
    result = subprocess.run(
        ["rtk", "go", "build", "-o", str(binary), "./cmd/cli"],
        cwd=REPO,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )
    if result.returncode:
        raise RuntimeError(f"binary build failed (rtk go build): {redact(result.stdout[-2000:])}")
    return binary, True


def scenario_setup(root: Path, scenario: str) -> tuple[FixtureHTTPServer, Path, Path]:
    home = root / scenario / "home"
    cwd = root / scenario / "workspace"
    home.mkdir(parents=True)
    cwd.mkdir(parents=True)
    return FixtureHTTPServer(scenario), home, cwd


def approval_detail_status(screen: str) -> tuple[int, int, int] | None:
    match = re.search(r"details\s+(\d+)-(\d+)/(\d+)", screen, re.IGNORECASE)
    if not match:
        return None
    return tuple(int(value) for value in match.groups())  # type: ignore[return-value]


def approval_screenshot(
    session: InteractivePTY,
    name: str,
    evidence: Path,
    scenario: str,
    visible_status: tuple[int, int, int] | None = None,
) -> dict[str, Any]:
    expected = (
        [f"details {visible_status[0]}-{visible_status[1]}/{visible_status[2]}"]
        if visible_status is not None
        else ["[approval]", "bash", "scope: unknown", "view truncated", "PgUp/PgDn"]
    )
    return session.screenshot(
        name,
        evidence,
        expected,
        require_color=True,
        scenario=scenario,
    )


def scroll_approval_to_end(session: InteractivePTY, first: tuple[int, int, int]) -> list[tuple[int, int, int]]:
    pages = [first]
    current = first
    while current[1] < current[2]:
        session.send(b"\x1b[6~")  # Page Down
        next_status, _ = session.wait_for_condition(
            lambda screen: (
                status
                if (status := approval_detail_status(screen)) is not None and status[1] > current[1]
                else None
            ),
            "approval details to advance after Page Down",
            timeout=10,
        )
        if next_status[2] != current[2]:
            raise AssertionError(f"approval detail total changed while scrolling: {current} -> {next_status}")
        pages.append(next_status)
        current = next_status
        if len(pages) > 12:
            raise AssertionError(f"approval details did not reach the end: {pages}")
    return pages


def run_answer(root: Path, binary: Path, node: str, evidence: Path) -> dict[str, Any]:
    server, home, cwd = scenario_setup(root, "answer")
    session = InteractivePTY(binary, cwd, home, server, node, HERE / "renderer.cjs", viewport=(560, 360))
    result: dict[str, Any] = {"name": "streaming answer", "checks": [], "screenshots": []}
    try:
        startup = session.wait_for(STARTUP_TEXT, timeout=30)
        result["screenshots"].append(
            session.screenshot("01-startup.png", evidence, ["Interactive session started"], scenario="streaming answer")
        )
        result["checks"].append({"name": "startup banner rendered in TTY", "passed": STARTUP_TEXT.lower() in startup.lower()})
        profile = session.terminal_profile()
        result["terminal_profile"] = profile
        result["checks"].append(
            {
                "name": "narrow xterm viewport and real PTY report identical columns and rows",
                "passed": profile["xterm_cols"] == profile["pty_cols"]
                and profile["xterm_rows"] == profile["pty_rows"]
                and profile["pty_cols"] < 80,
            }
        )
        result["checks"].append({"name": "normal color profile is not suppressed by inherited NO_COLOR", "passed": profile["no_color_removed"]})

        session.send_line("请用本地中文流式回答保持原样。")
        session.wait_for("这是本地模拟", timeout=20)
        result["screenshots"].append(
            session.screenshot(
                "02-chinese-stream-midstream.png", evidence, ["这是本地模拟"], require_color=True, scenario="streaming answer"
            )
        )
        completed = session.wait_for("这是本地模拟模型逐段返回的中文答案。", timeout=20)
        result["screenshots"].append(
            session.screenshot(
                "03-chinese-stream-complete.png",
                evidence,
                ["这是本地模拟模型逐段返回的中文答案。"],
                require_color=True,
                scenario="streaming answer",
            )
        )
        result["checks"].append({"name": "UTF-8 Chinese answer arrived in multiple SSE content chunks", "passed": "这是本地模拟模型逐段返回的中文答案。" in completed})
        with server.lock:
            result["checks"].append({"name": "one local mock request used fixture authorization", "passed": server.request_count == 1 and server.fixture_auth_seen})
        result["mock"] = _server_summary(server)
        code = session.finish_session()
        result["checks"].append({"name": "interactive REPL exited cleanly", "passed": code == 0})
    except Exception as error:
        result["failure"] = f"{type(error).__name__}: {redact(str(error))}"
    finally:
        session.close()
        result["pty_transcript"] = session.save_raw_transcript(evidence, "streaming-answer")
    result["mock"] = _server_summary(server)
    return result


def run_approval(root: Path, binary: Path, node: str, evidence: Path, scenario: str) -> dict[str, Any]:
    server, home, cwd = scenario_setup(root, scenario)
    session = InteractivePTY(binary, cwd, home, server, node, HERE / "renderer.cjs")
    result: dict[str, Any] = {"name": f"approval {scenario}", "checks": [], "screenshots": []}
    try:
        session.wait_for(STARTUP_TEXT, timeout=30)
        session.send_line("执行本地模拟器提交的多阶段 shell 命令。")
        approval_screen = session.wait_for("PgUp/PgDn", timeout=30)
        first = approval_detail_status(approval_screen)
        if first is None or first[0] != 1 or first[1] >= first[2]:
            raise AssertionError(f"long approval command did not start on a paginated first page: {first}")
        result["screenshots"].append(
            approval_screenshot(
                session,
                "04-approval-deny-wide-page-one.png" if scenario == "deny" else "08-approval-allow-wide-page-one.png",
                evidence,
                f"approval {scenario}",
            )
        )
        result["terminal_profile"] = session.terminal_profile()
        with server.lock:
            proposed = (
                server.proposed_tool_name == "bash"
                and server.proposed_command == LONG_COMMAND
                and server.tool_call_id == "call-terminal-e2e"
                and len(server.tool_response_sha256) == 64
            )
            no_receipt = not server.tool_receipts
        no_side_effects = not any((cwd / name).exists() for name in STAGE_FILES)
        result["checks"].append({"name": "mock proposes the complete long bash command", "passed": proposed})
        result["checks"].append({"name": "approval screen appears before any shell side effect or tool receipt", "passed": no_side_effects and no_receipt})
        result["checks"].append(
            {
                "name": "approval command's visible output token is not mistaken for a started task",
                "passed": "e2e_stage_three" in approval_screen.lower() and no_side_effects and no_receipt,
            }
        )
        result["checks"].append(
            {
                "name": "wide xterm viewport and real PTY report matching dimensions with at least 120 columns",
                "passed": result["terminal_profile"]["xterm_cols"] == result["terminal_profile"]["pty_cols"]
                and result["terminal_profile"]["xterm_rows"] == result["terminal_profile"]["pty_rows"]
                and result["terminal_profile"]["pty_cols"] >= 120,
            }
        )
        result["checks"].append({"name": "normal color profile is not suppressed by inherited NO_COLOR", "passed": result["terminal_profile"]["no_color_removed"]})

        session.send(b"\x1b[6~")  # Page Down
        second, _ = session.wait_for_condition(
            lambda screen: (
                status
                if (status := approval_detail_status(screen)) is not None and status[0] > first[0]
                else None
            ),
            "approval details to advance to a second page",
            timeout=10,
        )
        if second[2] != first[2]:
            raise AssertionError(f"approval detail total changed while paging: {first} -> {second}")
        result["screenshots"].append(
            approval_screenshot(
                session,
                "05-approval-deny-wide-page-two.png" if scenario == "deny" else "09-approval-allow-wide-page-two.png",
                evidence,
                f"approval {scenario}",
                visible_status=second,
            )
        )
        result["checks"].append({"name": "Page Down scrolled the long approval details", "passed": second[0] > first[0] and second[1] > first[1]})

        if scenario == "deny":
            before_resize = second
            resized = session.resize_viewport(560, 400)
            dimensions_match = resized["xterm_cols"] == resized["pty_cols"] and resized["xterm_rows"] == resized["pty_rows"]
            result["checks"].append({"name": "resize keeps xterm.js and PTY column/row counts identical", "passed": dimensions_match})
            after_resize, _ = session.wait_for_condition(
                lambda screen: (
                    status
                    if (status := approval_detail_status(screen)) is not None
                    and status[0] == 1
                    and status[2] > before_resize[2]
                    else None
                ),
                "approval details to reflow after terminal resize",
                timeout=10,
            )
            result["screenshots"].append(
                approval_screenshot(
                    session,
                    "06-approval-deny-narrow-after-resize.png",
                    evidence,
                    f"approval {scenario}",
                    visible_status=after_resize,
                )
            )
            result["checks"].append(
                {
                    "name": "narrow resize resets review position and increases wrapped detail rows",
                    "passed": after_resize[0] == 1 and after_resize[2] > before_resize[2],
                }
            )
            pages = scroll_approval_to_end(session, after_resize)
            result["checks"].append({"name": "all approval pages were reachable after resize", "passed": pages[-1][1] == pages[-1][2]})
        else:
            pages = scroll_approval_to_end(session, second)
            result["screenshots"].append(
                approval_screenshot(
                    session,
                    "10-approval-allow-wide-last-page.png",
                    evidence,
                    f"approval {scenario}",
                    visible_status=pages[-1],
                )
            )
            result["checks"].append({"name": "all wide approval pages were reviewed before allow", "passed": pages[-1][1] == pages[-1][2]})

        if scenario == "deny":
            session.send(b"n")
            completion = session.wait_for("本地模拟器收到拒绝凭据，命令没有运行。", timeout=30)
            result["screenshots"].append(
                session.screenshot(
                    "07-approval-denied-complete.png",
                    evidence,
                    ["本地模拟器收到拒绝凭据"],
                    require_color=True,
                    scenario=f"approval {scenario}",
                )
            )
            with server.lock:
                denial_receipt = server.tool_receipts[0] if server.tool_receipts else {}
                result["checks"].append(
                    {
                        "name": "denial tool receipt returned to the mock model",
                        "passed": server.request_count == 2
                        and server.denial_receipt_seen
                        and denial_receipt.get("tool_call_id") == server.tool_call_id,
                    }
                )
                no_stage_receipt = not (server.stage_one_output_seen or server.stage_two_output_seen or server.stage_three_output_seen)
                result["checks"].append({"name": "denied shell tool receipt contains no staged output", "passed": no_stage_receipt})
            result["checks"].append({"name": "denied shell produced no stage files", "passed": not any((cwd / name).exists() for name in STAGE_FILES)})
            result["checks"].append({"name": "denial answer rendered to terminal", "passed": "本地模拟器收到拒绝凭据，命令没有运行。" in completion})
        else:
            session.send(b"y")
            stage_one = wait_for_file(cwd / STAGE_FILES[0], timeout=20)
            running = session.wait_for("working...", timeout=5)
            running = session.wait_for_condition(
                lambda screen: screen if "working..." in screen.lower() and "[tool] bash" in screen.lower() else None,
                "visible running tool state",
                timeout=5,
            )[1]
            if (cwd / STAGE_FILES[1]).exists() or (cwd / STAGE_FILES[2]).exists():
                raise AssertionError("later staged side effects occurred before the running follow-up screenshot")
            with server.lock:
                still_running_before_followup = server.request_count == 1 and not server.long_output_receipt_seen
            result["checks"].append(
                {
                    "name": "wide running view visibly renders the side-by-side conversation and tool panes",
                    "passed": "│" in running,
                }
            )
            session.send(b"f")
            session.wait_for("followup>", timeout=10)
            session.send(FOLLOWUP_TEXT.encode("utf-8"))
            edited_followup = session.wait_for(FOLLOWUP_TEXT, timeout=10)
            result["screenshots"].append(
                session.screenshot(
                    "11-long-bash-running-followup-edited.png",
                    evidence,
                    ["working...", "[tool] bash", "│", "followup>", FOLLOWUP_TEXT],
                    require_color=False,
                    scenario=f"approval {scenario}",
                )
            )
            result["checks"].append(
                {
                    "name": "running task follow-up editor preserves English spaces and Chinese text",
                    "passed": FOLLOWUP_TEXT in edited_followup and still_running_before_followup,
                }
            )
            session.send(b"\r")
            with server.lock:
                still_running = server.request_count == 1 and not server.long_output_receipt_seen
            result["checks"].append({"name": "allow key caused the first filesystem side effect before task completion", "passed": stage_one and (cwd / STAGE_FILES[0]).read_text(encoding="utf-8").strip() == "E2E_STAGE_ONE" and (cwd / STAGE_FILES[1]).exists() is False and (cwd / STAGE_FILES[2]).exists() is False})
            result["checks"].append(
                {
                    "name": "visible running tool view arrived before the tool receipt",
                    "passed": "working..." in running.lower()
                    and "[tool] bash" in running.lower()
                    and still_running_before_followup
                    and still_running,
                }
            )
            wait_for_file(cwd / STAGE_FILES[2], timeout=20)
            completed = session.wait_for("本地模拟器确认三阶段 shell 命令全部完成，并收到 follow-up 指令。", timeout=30)
            result["screenshots"].append(
                session.screenshot(
                    "12-long-bash-complete.png",
                    evidence,
                    ["本地模拟器确认三阶段 shell 命令全部完成，并收到 follow-up 指令。"],
                    require_color=True,
                    scenario=f"approval {scenario}",
                )
            )
            with server.lock:
                tool_receipt = server.tool_receipts[0] if server.tool_receipts else {}
                result["checks"].append(
                    {
                        "name": "tool receipt returned all three stage outputs under the fixture call ID",
                        "passed": server.request_count >= 2
                        and server.long_output_receipt_seen
                        and tool_receipt.get("tool_call_id") == server.tool_call_id
                        and tool_receipt.get("stage_markers_seen") == [True, True, True],
                    }
                )
                result["checks"].append(
                    {
                        "name": "submitted follow-up reached the mock model unchanged in the provider request",
                        "passed": server.followup_received and FOLLOWUP_TEXT in server.followup_messages,
                    }
                )
            expected_stage_contents = ("E2E_STAGE_ONE", "E2E_STAGE_TWO", "E2E_STAGE_THREE")
            result["checks"].append(
                {
                    "name": "all three stage files contain their expected side effects",
                    "passed": all(
                        (cwd / name).is_file() and (cwd / name).read_text(encoding="utf-8").strip() == expected
                        for name, expected in zip(STAGE_FILES, expected_stage_contents, strict=True)
                    ),
                }
            )
            result["checks"].append({"name": "long task completed and final answer rendered", "passed": "本地模拟器确认三阶段 shell 命令全部完成，并收到 follow-up 指令。" in completed})

        result["mock"] = _server_summary(server)
        code = session.finish_session()
        result["checks"].append({"name": "interactive REPL exited cleanly", "passed": code == 0})
    except Exception as error:
        result["failure"] = f"{type(error).__name__}: {redact(str(error))}"
    finally:
        session.close()
        result["pty_transcript"] = session.save_raw_transcript(evidence, f"approval-{scenario}")
    result["mock"] = _server_summary(server)
    return result


def wait_for_file(path: Path, timeout: float) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if path.is_file():
            return True
        time.sleep(0.05)
    raise TimeoutError(f"timed out waiting for command side effect: {path.name}")


def _server_summary(server: FixtureHTTPServer) -> dict[str, Any]:
    with server.lock:
        return {
            "scenario": server.scenario,
            "request_count": server.request_count,
            "fixture_authorization_seen": server.fixture_auth_seen,
            "denial_receipt_seen": server.denial_receipt_seen,
            "long_output_receipt_seen": server.long_output_receipt_seen,
            "stage_output_seen": [server.stage_one_output_seen, server.stage_two_output_seen, server.stage_three_output_seen],
            "proposed_tool_name": server.proposed_tool_name,
            "proposed_command_line_count": len(server.proposed_command.splitlines()),
            "tool_call_id": server.tool_call_id,
            "tool_response_sha256": server.tool_response_sha256,
            "tool_receipts": list(server.tool_receipts),
            "followup_received": server.followup_received,
            "followup_messages": list(server.followup_messages),
        }


def create_run_workdir(scratch_root: Path) -> Path:
    """Create an isolated run directory under the selected scratch parent."""
    return Path(tempfile.mkdtemp(prefix="go-cli-terminal-e2e-", dir=str(scratch_root)))


def rtk_git_bytes(*args: str) -> bytes:
    result = subprocess.run(
        ["rtk", "proxy", "git", *args],
        cwd=REPO,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    return result.stdout if result.returncode == 0 else b""


def rtk_git_paths(*args: str) -> list[str]:
    output = rtk_git_bytes(*args).decode("utf-8", "surrogateescape")
    return [relative for relative in output.split("\0") if relative]


def source_delta_sha256() -> str:
    paths = set(rtk_git_paths("diff", "--name-only", "-z", "HEAD"))
    paths.update(rtk_git_paths("ls-files", "--others", "--exclude-standard", "-z"))
    digest = hashlib.sha256()
    for relative in sorted(path for path in paths if path and not path.startswith("docs/qa/")):
        path = REPO / relative
        digest.update(relative.encode("utf-8", "surrogateescape"))
        digest.update(b"\0")
        if path.is_symlink():
            digest.update(b"symlink\0")
            digest.update(os.readlink(path).encode("utf-8", "surrogateescape"))
        elif path.is_file():
            digest.update(b"file\0")
            digest.update(path.read_bytes())
        else:
            digest.update(b"deleted\0")
        digest.update(b"\0")
    return digest.hexdigest()


def get_revision() -> dict[str, str]:
    def rtk_git(*args: str) -> str:
        result = subprocess.run(
            ["rtk", "proxy", "git", *args],
            cwd=REPO,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        return result.stdout.strip() if result.returncode == 0 else "unknown"

    return {
        "branch": rtk_git("branch", "--show-current"),
        "commit": rtk_git("rev-parse", "HEAD"),
        "working_tree_source_delta_sha256": source_delta_sha256(),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", help="path to an already-built go-cli executable (default: build with rtk go)")
    parser.add_argument("--evidence-dir", type=Path, default=DEFAULT_EVIDENCE, help="where to save genuine xterm.js screenshots and report")
    parser.add_argument(
        "--scratch-root",
        type=Path,
        default=Path(tempfile.gettempdir()),
        help="parent directory for temporary runs (default: Python's tempfile.gettempdir())",
    )
    parser.add_argument("--keep-workdir", action="store_true", help="retain isolated HOME/workspaces and binary under the scratch root")
    args = parser.parse_args()

    evidence = args.evidence_dir.resolve()
    evidence.mkdir(parents=True, exist_ok=True)
    node = locate_node()
    scratch_root = args.scratch_root.expanduser().resolve()
    if not scratch_root.is_dir():
        parser.error(f"scratch root is not a directory: {scratch_root}")
    temp_root = create_run_workdir(scratch_root)
    results: list[dict[str, Any]] = []
    binary: Path | None = None
    built = False
    failure: str | None = None
    try:
        binary, built = locate_binary(args.binary, temp_root / "build")
        results.append(run_answer(temp_root, binary, node, evidence))
        results.append(run_approval(temp_root, binary, node, evidence, "deny"))
        results.append(run_approval(temp_root, binary, node, evidence, "allow"))
    except Exception:  # evidence report captures scenario blockers as a failed run
        failure = redact(traceback.format_exc())
    finally:
        report = {
            "date": "2026-10-14",
            "revision": get_revision(),
            "scratch_root": str(scratch_root),
            "binary": str(binary) if binary else None,
            "binary_built_by_harness": built,
            "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest() if binary and binary.is_file() else None,
            "screenshot_source": "Chrome headless screenshot of the xterm.js visible viewport fed with raw bytes read from the real CLI PTY; not a text-to-image rendering and not a native Terminal window",
            "network": "the provider API endpoint is the localhost mock HTTP fixture; no live model API key is supplied",
            "isolation": "fresh temporary HOME and working directory per scenario; GO_CLI_* inherited values removed; API auth is a fixture value",
            "terminal_color_profile": "TERM=xterm-256color and COLORTERM=truecolor; inherited NO_COLOR removed in child process",
            "transcript_policy": "visible-screen assertions read only xterm.js buffer.viewportY through rows; raw PTY byte streams are retained separately per scenario",
            "results": results,
            "failure": failure,
        }
        (evidence / "run-report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        write_evidence_markdown(evidence, report)
        if args.keep_workdir:
            report["retained_workdir"] = str(temp_root)
            (evidence / "run-report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
            write_evidence_markdown(evidence, report)
        else:
            shutil.rmtree(temp_root, ignore_errors=True)

    print(f"Evidence: {evidence}")
    if failure:
        print(f"E2E FAILED: {failure}", file=sys.stderr)
        return 1
    failed = [
        f"{scenario['name']}: {check['name']}"
        for scenario in results
        for check in scenario["checks"]
        if not check["passed"]
    ]
    failed.extend(f"{scenario['name']}: {scenario['failure']}" for scenario in results if scenario.get("failure"))
    if failed:
        for item in failed:
            print(f"ASSERTION FAILED: {item}", file=sys.stderr)
        return 1
    print(f"E2E PASSED: {sum(len(s['screenshots']) for s in results)} real PTY screenshots; {sum(len(s['checks']) for s in results)} assertions")
    return 0


def write_evidence_markdown(evidence: Path, report: dict[str, Any]) -> None:
    lines = [
        "# Real terminal E2E evidence",
        "",
        f"- Revision: `{report['revision']['branch']}@{report['revision']['commit']}`",
        f"- Working-tree source delta SHA-256: `{report['revision']['working_tree_source_delta_sha256']}` (source paths only; QA evidence is excluded).",
        f"- Built binary SHA-256: `{report['binary_sha256']}`",
        f"- Screenshot renderer: {report['screenshot_source']}",
        f"- HTTP fixture: {report['network']}",
        f"- Process isolation: {report['isolation']}",
        f"- Terminal color profile: {report['terminal_color_profile']}",
        f"- Transcript handling: {report['transcript_policy']}",
        "- Evidence class: real binary PTY end-to-end; UI pixels are from xterm.js in headless Chrome.",
        "",
        "## Scenarios",
        "",
    ]
    for scenario in report["results"]:
        lines.append(f"### {scenario['name']}")
        lines.append("")
        if scenario.get("failure"):
            lines.append(f"- Scenario error: {redact(scenario['failure'])}")
        for check in scenario["checks"]:
            mark = "PASS" if check["passed"] else "FAIL"
            lines.append(f"- {mark}: {check['name']}")
        for shot in scenario["screenshots"]:
            terminal = shot["terminal"]
            lines.append(
                f"- Screenshot: [`{shot['file']}`]({shot['file']}) — {terminal['viewport_width_px']}×"
                f"{terminal['viewport_height_px']} requested px; PNG {terminal['image_width_px']}×"
                f"{terminal['image_height_px']} px; PTY/xterm {terminal['pty_cols']}×{terminal['pty_rows']} cells; "
                f"{terminal['colored_cells']} ANSI-colored cells; assertions: {json.dumps(shot['assertions'], ensure_ascii=False)}."
            )
        if "pty_transcript" in scenario:
            transcript = scenario["pty_transcript"]
            lines.append(
                f"- Raw PTY transcript: [`{transcript['file']}`]({transcript['file']}) — {transcript['bytes']} bytes, "
                f"SHA-256 `{transcript['sha256']}`."
            )
        if "mock" in scenario:
            mock = scenario["mock"]
            lines.append(
                f"- Mock counters: {mock['request_count']} request(s), fixture authorization observed: "
                f"{str(mock['fixture_authorization_seen']).lower()}, denial receipt: "
                f"{str(mock['denial_receipt_seen']).lower()}, staged output receipt: "
                f"{str(mock['long_output_receipt_seen']).lower()}."
            )
            if mock.get("tool_call_id"):
                lines.append(
                    f"- Fixture tool call: `{mock['proposed_tool_name']}` / `{mock['tool_call_id']}`, "
                    f"{mock['proposed_command_line_count']} command lines, response SHA-256 `{mock['tool_response_sha256']}`."
                )
            if mock.get("followup_received"):
                lines.append(f"- Follow-up delivered in provider request: `{FOLLOWUP_TEXT}`.")
        lines.append("")
    if report.get("failure"):
        lines.extend(["## Failure", "", redact(report["failure"]), ""])
    lines.append("The binary is launched with stdin/stdout/stderr attached to a PTY. The capture process forwards the PTY's original byte stream into xterm.js and asks Chromium to screenshot that terminal viewport. The harness does not generate terminal pixels from extracted text.")
    (evidence / "evidence.md").write_text("\n".join(lines).rstrip() + "\n", encoding="utf-8")


if __name__ == "__main__":
    raise SystemExit(main())
