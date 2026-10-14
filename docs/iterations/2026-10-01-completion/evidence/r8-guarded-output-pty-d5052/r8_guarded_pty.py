#!/usr/bin/env python3
"""Real CLI PTY acceptance for guarded whole-response output on d5052."""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import pathlib
import re
import select
import socket
import subprocess
import sys
import threading
import time

sys.dont_write_bytecode = True
REPO = pathlib.Path(__file__).resolve().parents[5]
EVIDENCE: pathlib.Path
BINARY: pathlib.Path
LEGACY = (
    REPO
    / "docs/iterations/2026-09-30-runtime/evidence/"
    / "r8-active-stream-guard-boundary-eed9fff/r8-active-stream-guard-pty.py"
)
SOURCE_COMMIT = ""
SYNTHETIC_KEY = "AKIA1234567890ABCDEF"
GUARDED_PREFIX = "R8-GUARDED-PREFIX "
GUARDED_SUFFIX = " R8-GUARDED-SUFFIX"
SANITIZED = GUARDED_PREFIX + "[REDACTED]" + GUARDED_SUFFIX
PROGRESS_LABEL = "being generated and checked"
ANSI_RE = re.compile(
    rb"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\)|[@-Z\\-_])"
)

spec = importlib.util.spec_from_file_location("legacy_r8_pty", LEGACY)
if spec is None or spec.loader is None:
    raise RuntimeError(f"could not import existing PTY harness: {LEGACY}")
legacy = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = legacy
spec.loader.exec_module(legacy)


def record(server, event, **fields):
    row = {
        "event": event,
        "monotonic_ns": time.monotonic_ns(),
        "wall_time": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        **fields,
    }
    with server.lock:
        server.test_events.append(row)


class GuardProviderHandler(legacy.ProviderHandler):
    def _headers(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        self.chunk({"role": "assistant"}, None)

    def _peer_closed(self):
        try:
            readable, _, _ = select.select([self.connection], [], [], 0)
            if not readable:
                return False
            data = self.connection.recv(1, socket.MSG_PEEK)
            return data == b""
        except (BlockingIOError, InterruptedError):
            return False
        except OSError:
            return True

    def _held_stream(self, mode):
        self._headers()
        candidate = GUARDED_PREFIX + SYNTHETIC_KEY + GUARDED_SUFFIX
        if mode in ("success", "cancel"):
            # Split the synthetic API-key canary across provider deltas. The
            # whole-response guard must assess the assembled candidate.
            self.chunk({"content": GUARDED_PREFIX + "AKIA"}, None)
            self.chunk({"content": "1234567890ABCDEF"}, None)
            self.chunk({"content": GUARDED_SUFFIX}, None)
            record(
                self.server,
                "candidate_flushed",
                mode=mode,
                bytes=len(candidate.encode("utf-8")),
                sha256=hashlib.sha256(candidate.encode("utf-8")).hexdigest(),
                synthetic_canary_value_omitted=True,
            )
        held = self.server.success_release if mode == "success" else self.server.cancel_release
        if mode == "success":
            self.server.success_held.set()
        else:
            self.server.cancel_held.set()
        record(self.server, "response_held", mode=mode)

        while not held.wait(0.25):
            if self._peer_closed():
                self.server.client_disconnected.set()
                record(self.server, "client_socket_closed", mode=mode)
                return
            try:
                # Empty SSE chunks keep the existing CLI chunk-idle budget
                # alive without adding response text to the candidate.
                self.chunk({}, None)
            except (BrokenPipeError, ConnectionResetError, OSError):
                self.server.client_disconnected.set()
                record(self.server, "client_socket_closed", mode=mode)
                return

        if mode == "success":
            self.chunk({}, "stop")
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            record(self.server, "response_completed", mode=mode)

    def _viewport_stream(self):
        self._headers()
        self.server.viewport_held.set()
        record(self.server, "viewport_probe_held")
        while not self.server.viewport_release.wait(0.25):
            if self._peer_closed():
                self.server.client_disconnected.set()
                record(self.server, "client_socket_closed", mode="viewport")
                return
            try:
                self.chunk({}, None)
            except (BrokenPipeError, ConnectionResetError, OSError):
                self.server.client_disconnected.set()
                record(self.server, "client_socket_closed", mode="viewport")
                return
        answer = "R8-VIEWPORT-ANSWER completed transcript tail remains navigable."
        for offset in range(0, len(answer), 12):
            self.chunk({"content": answer[offset:offset + 12]}, None)
        self.chunk({}, "stop")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        record(self.server, "viewport_response_completed", answer_sha256=hashlib.sha256(answer.encode()).hexdigest())

    def write_stream(self, calls, answer, last_user):
        if calls:
            return super().write_stream(calls, answer, last_user)
        if last_user == "R8 guarded success":
            return self._held_stream("success")
        if last_user == "R8 cancellation probe":
            return self._held_stream("cancel")
        if last_user == "R8 viewport probe":
            return self._viewport_stream()
        return super().write_stream(calls, answer, last_user)


legacy.ProviderHandler = GuardProviderHandler


class GuardProvider(legacy.Provider):
    def __init__(self, address):
        super().__init__(address)
        self.success_held = threading.Event()
        self.success_release = threading.Event()
        self.cancel_held = threading.Event()
        self.cancel_release = threading.Event()
        self.viewport_held = threading.Event()
        self.viewport_release = threading.Event()
        self.client_disconnected = threading.Event()
        self.test_events = []
        self.workspace = None


def load_pyte():
    try:
        import pyte  # noqa: F401
    except ImportError as exc:
        raise RuntimeError("the existing PTY harness dependency pyte is unavailable") from exc


def start_fixture():
    provider = GuardProvider(("127.0.0.1", 0))
    thread = threading.Thread(target=provider.serve_forever, daemon=True)
    thread.start()
    return provider, thread


def start_cli(provider, root):
    home, xdg, workspace = root / "home", root / "xdg", root / "workspace"
    for path in (home / ".go-cli", xdg, workspace, root / "cache", root / "state", root / "data"):
        path.mkdir(parents=True, exist_ok=True)
    provider.workspace = workspace
    host, port = provider.server_address
    config = (
        "provider:\n"
        "  name: openai\n"
        f"  api_key: {legacy.CANARY_KEY}\n"
        f"  base_url: http://{host}:{port}/v1\n"
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
    cli = legacy.PtyCLI(str(BINARY), workspace, env)
    cli.wait_prompt(timeout=15)
    return cli, workspace, root


def sanitize_bytes(data):
    return data.replace(SYNTHETIC_KEY.encode(), b"[SYNTHETIC_CANARY_REMOVED]")


def safe_marker_name(marker):
    return {
        SYNTHETIC_KEY: "synthetic_api_key_canary",
        GUARDED_PREFIX.strip(): "candidate_prefix",
        GUARDED_SUFFIX.strip(): "candidate_suffix",
        SANITIZED: "sanitized_answer",
    }.get(marker, marker)


def save_raw_interval(cli, start, path):
    data = bytes(cli.buffer[start:])
    leaked = SYNTHETIC_KEY.encode() in data
    path.write_bytes(sanitize_bytes(data))
    return leaked


def frame_text(cli):
    return cli.frame()


def wait_held_frame(cli, provider, held_event, start, label):
    if not held_event.wait(12):
        raise AssertionError(f"provider did not reach held boundary for {label}")
    frame = cli.wait_frame_for(PROGRESS_LABEL, timeout=12)
    if cli.process.poll() is not None:
        raise AssertionError(f"CLI exited while {label} provider response was held")
    (EVIDENCE / f"{label}-held-frame.txt").write_text(frame + "\n", encoding="utf-8")
    raw_leak = save_raw_interval(cli, start, EVIDENCE / f"{label}-held-raw.txt")
    if raw_leak or SYNTHETIC_KEY in frame:
        raise AssertionError(f"synthetic canary reached PTY during held {label} response")
    return frame


def session_receipt(workspace, label, forbidden=(), required=()):
    store = workspace / "sessions"
    records = []
    collected = []
    for path in sorted(store.rglob("*.jsonl")):
        raw = path.read_bytes()
        collected.append(raw)
        data = {
            "relative_path": str(path.relative_to(store)),
            "bytes": len(raw),
            "sha256": hashlib.sha256(raw).hexdigest(),
            "contains_synthetic_canary": SYNTHETIC_KEY.encode() in raw,
            "redacted_placeholder_count": raw.count(b"[REDACTED]"),
            "required_markers_present": {safe_marker_name(marker): marker.encode() in raw for marker in required},
            "forbidden_markers_present": {safe_marker_name(marker): marker.encode() in raw for marker in forbidden},
        }
        records.append(data)
        out = EVIDENCE / f"{label}-session-receipts"
        out.mkdir(parents=True, exist_ok=True)
        (out / path.name).write_bytes(sanitize_bytes(raw))
    all_data = b"\n".join(collected)
    meta = {
        "files": records,
        "any_synthetic_canary": SYNTHETIC_KEY.encode() in all_data,
        "forbidden_markers_present": {safe_marker_name(marker): marker.encode() in all_data for marker in forbidden},
        "required_markers_present": {safe_marker_name(marker): marker.encode() in all_data for marker in required},
    }
    (EVIDENCE / f"{label}-session-receipts.json").write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")
    return meta


def finish_cli(cli, label):
    before = cli.original_termios[3] & (legacy.termios.ICANON | legacy.termios.ECHO | legacy.termios.ISIG)
    cli.send("/exit\r", settle=0.1)
    deadline = time.monotonic() + 15
    while cli.process.poll() is None and time.monotonic() < deadline:
        cli.pump(0.05)
    if cli.process.poll() is None:
        raise AssertionError(f"{label}: CLI did not exit after /exit")
    rc = cli.process.returncode
    cli.close()
    after = cli.final_termios[3] & (legacy.termios.ICANON | legacy.termios.ECHO | legacy.termios.ISIG) if cli.final_termios else None
    if rc != 0:
        raise AssertionError(f"{label}: /exit returned {rc}")
    if after != before:
        raise AssertionError(f"{label}: terminal flags changed: before={before}, after={after}")
    return {"exit_code": rc, "terminal_flags_before": before, "terminal_flags_after": after, "restored": True}


def provider_cleanup(provider, thread):
    provider.success_release.set()
    provider.cancel_release.set()
    provider.viewport_release.set()
    provider.shutdown()
    provider.server_close()
    thread.join(timeout=3)


def test_success():
    provider, thread = start_fixture()
    cli = None
    root_holder = None
    try:
        with __import__("tempfile").TemporaryDirectory(prefix="r8-d505-success-") as tmp:
            root = pathlib.Path(tmp)
            cli, workspace, root_holder = start_cli(provider, root)
            start = len(cli.buffer)
            cli.send("R8 guarded success\r", settle=0.1)
            held = wait_held_frame(cli, provider, provider.success_held, start, "success")
            if GUARDED_PREFIX.strip() in held or GUARDED_SUFFIX.strip() in held:
                raise AssertionError("raw candidate text appeared in the held success frame")
            before = session_receipt(
                workspace,
                "success-before-release",
                forbidden=(GUARDED_PREFIX.strip(), SYNTHETIC_KEY, GUARDED_SUFFIX.strip(), SANITIZED),
            )
            if before["any_synthetic_canary"] or any(before["forbidden_markers_present"].values()):
                raise AssertionError("held candidate was persisted before OutputGuard completion")
            provider.success_release.set()
            cli.wait_frame_for(SANITIZED, timeout=15)
            final_frame = frame_text(cli)
            if final_frame.count(SANITIZED) != 1:
                raise AssertionError(f"expected one sanitized assistant row in current frame, found {final_frame.count(SANITIZED)}")
            if SYNTHETIC_KEY in final_frame or GUARDED_PREFIX + SYNTHETIC_KEY in cli.plain():
                raise AssertionError("raw synthetic API key appeared after success completion")
            cli.wait_prompt(start=start, timeout=15)
            final_receipts = session_receipt(
                workspace,
                "success-after-release",
                forbidden=(SYNTHETIC_KEY,),
                required=(SANITIZED,),
            )
            if final_receipts["any_synthetic_canary"] or any(final_receipts["forbidden_markers_present"].values()):
                raise AssertionError("session receipt contains the raw synthetic key")
            if not any(final_receipts["required_markers_present"].values()):
                raise AssertionError("session receipt omitted the sanitized final answer")
            if PROGRESS_LABEL in frame_text(cli):
                raise AssertionError("generation/check status remained visible after successful completion")
            (EVIDENCE / "success-final-frame.txt").write_text(final_frame + "\n", encoding="utf-8")
            raw_leak = save_raw_interval(cli, start, EVIDENCE / "success-after-release-raw.txt")
            if raw_leak:
                raise AssertionError("raw synthetic API key appeared in successful PTY bytes")
            exit_result = finish_cli(cli, "success")
            cli = None
            return {
                "journey": "held stream success and final redaction",
                "provider_events": list(provider.test_events),
                "provider_requests": list(provider.requests),
                "held_frame_status_visible": PROGRESS_LABEL in held,
                "final_frame_sanitized_answer_count": final_frame.count(SANITIZED),
                "session_before_release": before,
                "session_after_release": final_receipts,
                "process_and_terminal": exit_result,
            }
    finally:
        if cli is not None:
            cli.close()
        provider_cleanup(provider, thread)


def test_cancellation():
    provider, thread = start_fixture()
    cli = None
    try:
        with __import__("tempfile").TemporaryDirectory(prefix="r8-d505-cancel-") as tmp:
            cli, workspace, _ = start_cli(provider, pathlib.Path(tmp))
            start = len(cli.buffer)
            cli.send("R8 cancellation probe\r", settle=0.1)
            held = wait_held_frame(cli, provider, provider.cancel_held, start, "cancel")
            if GUARDED_PREFIX.strip() in held or SYNTHETIC_KEY in held:
                raise AssertionError("cancellation candidate was visible while provider was held")
            before = session_receipt(
                workspace,
                "cancel-before-esc",
                forbidden=(GUARDED_PREFIX.strip(), SYNTHETIC_KEY, GUARDED_SUFFIX.strip(), SANITIZED),
            )
            if before["any_synthetic_canary"] or any(before["forbidden_markers_present"].values()):
                raise AssertionError("cancellation candidate was persisted before Esc")
            # In the actual TTY app a standalone Esc is the active-turn cancel
            # key. Durable sessions defer app quit until canonical refresh.
            cli.send(b"\x1b", settle=0.2)
            if not provider.client_disconnected.wait(8):
                raise AssertionError("provider did not observe the client stream socket closing after Esc")
            cli.wait_prompt(start=start, timeout=15)
            if cli.process.poll() is not None:
                raise AssertionError(f"durable TTY process unexpectedly exited after Esc: {cli.process.returncode}")
            after_frame = frame_text(cli)
            if PROGRESS_LABEL in after_frame:
                raise AssertionError("generation/check status remained visible after Esc cancellation")
            (EVIDENCE / "cancel-after-esc-frame.txt").write_text(after_frame + "\n", encoding="utf-8")
            raw_leak = save_raw_interval(cli, start, EVIDENCE / "cancel-after-esc-raw.txt")
            if raw_leak:
                raise AssertionError("raw synthetic API key appeared in cancelled PTY bytes")
            final_receipts = session_receipt(
                workspace,
                "cancel-after-esc",
                forbidden=(GUARDED_PREFIX.strip(), SYNTHETIC_KEY, GUARDED_SUFFIX.strip(), SANITIZED),
            )
            if final_receipts["any_synthetic_canary"] or any(final_receipts["forbidden_markers_present"].values()):
                raise AssertionError("cancelled candidate or synthetic key was persisted")
            exit_result = finish_cli(cli, "cancel")
            cli = None
            return {
                "journey": "held stream canceled by standalone Esc",
                "provider_events": list(provider.test_events),
                "provider_requests": list(provider.requests),
                "held_frame_status_visible": PROGRESS_LABEL in held,
                "provider_observed_socket_close": provider.client_disconnected.is_set(),
                "process_alive_after_cancel": True,
                "session_before_esc": before,
                "session_after_esc": final_receipts,
                "process_and_terminal": exit_result,
            }
    finally:
        if cli is not None:
            cli.close()
        provider_cleanup(provider, thread)


def test_long_transcript_viewport():
    provider, thread = start_fixture()
    cli = None
    try:
        with __import__("tempfile").TemporaryDirectory(prefix="r8-d505-viewport-") as tmp:
            cli, workspace, _ = start_cli(provider, pathlib.Path(tmp))
            for index in range(24):
                user = f"R8 seed {index:02d}"
                expected = f"R8-ANSWER-{index:02d}"
                start = len(cli.buffer)
                cli.send(user + "\r", settle=0.08)
                cli.wait_for(expected, start=start, timeout=15)
                cli.wait_prompt(start=start, timeout=15)

            start = len(cli.buffer)
            cli.send("R8 viewport probe\r", settle=0.1)
            initial = wait_held_frame(cli, provider, provider.viewport_held, start, "viewport")
            if "R8-VIEWPORT-ANSWER" in initial or GUARDED_PREFIX.strip() in initial:
                raise AssertionError("viewport candidate appeared before provider release")

            for _ in range(12):
                before = len(cli.buffer)
                cli.send(b"\x1b[5~", settle=0.12)
                if "R8-ANSWER-23" not in frame_text(cli):
                    break
                if cli.process.poll() is not None:
                    raise AssertionError("CLI exited during PageUp")
                cli.pump(0.05)
            pageup_frame = frame_text(cli)
            if "R8-ANSWER-23" in pageup_frame:
                raise AssertionError("PageUp did not leave the completed transcript tail")
            visible = re.findall(r"R8-ANSWER-\d{2}", pageup_frame)
            if not visible:
                raise AssertionError(f"PageUp did not reveal a completed assistant row:\n{pageup_frame}")
            anchor = visible[-1]
            (EVIDENCE / "viewport-pageup-frame.txt").write_text(pageup_frame + "\n", encoding="utf-8")

            resize_frames = []
            for columns in (60, 80, 120, 200):
                cli.resize_and_wait(24, columns)
                frame = frame_text(cli)
                if anchor not in frame:
                    raise AssertionError(f"resize to {columns} columns lost the completed transcript anchor {anchor!r}:\n{frame}")
                resize_frames.append({"columns": columns, "anchor": anchor, "frame": frame})
            (EVIDENCE / "viewport-resize-frames.json").write_text(json.dumps(resize_frames, indent=2) + "\n", encoding="utf-8")

            cli.send(b"\x1b[4~", settle=0.2)
            end_frame = cli.wait_frame_for("R8-ANSWER-23", timeout=8)
            if "R8-VIEWPORT-ANSWER" in end_frame:
                raise AssertionError("End displayed probe content before provider release")
            (EVIDENCE / "viewport-end-before-release-frame.txt").write_text(end_frame + "\n", encoding="utf-8")

            raw_leak = save_raw_interval(cli, start, EVIDENCE / "viewport-before-release-raw.txt")
            if raw_leak:
                raise AssertionError("synthetic canary unexpectedly appeared during viewport probe")
            provider.viewport_release.set()
            cli.wait_frame_for("R8-VIEWPORT-ANSWER", timeout=15)
            cli.wait_prompt(start=start, timeout=15)
            final_frame = frame_text(cli)
            if PROGRESS_LABEL in final_frame:
                raise AssertionError("generation/check status remained visible after viewport probe completion")
            (EVIDENCE / "viewport-final-frame.txt").write_text(final_frame + "\n", encoding="utf-8")
            final_receipts = session_receipt(
                workspace,
                "viewport-after-completion",
                forbidden=(SYNTHETIC_KEY,),
                required=("R8-VIEWPORT-ANSWER",),
            )
            if final_receipts["any_synthetic_canary"] or not any(final_receipts["required_markers_present"].values()):
                raise AssertionError("viewport probe session receipt was missing or unsafe")
            exit_result = finish_cli(cli, "viewport")
            cli = None
            return {
                "journey": "completed long transcript PageUp/resize/End while next guarded response was held without content",
                "provider_events": list(provider.test_events),
                "provider_requests": list(provider.requests),
                "seed_turns": 24,
                "initial_status_visible": PROGRESS_LABEL in initial,
                "pageup_anchor": anchor,
                "resize_columns": [frame["columns"] for frame in resize_frames],
                "end_revealed_latest_completed_answer": "R8-ANSWER-23" in end_frame,
                "candidate_not_released_during_viewport_checks": "R8-VIEWPORT-ANSWER" not in end_frame,
                "session_after_completion": final_receipts,
                "process_and_terminal": exit_result,
            }
    finally:
        if cli is not None:
            cli.close()
        provider_cleanup(provider, thread)


def main():
    global BINARY, EVIDENCE, SOURCE_COMMIT
    parser = argparse.ArgumentParser(description="Run guarded-output acceptance against the real CLI in a PTY.")
    parser.add_argument("--binary", type=pathlib.Path, required=True, help="CLI binary built from the source under test")
    parser.add_argument("--evidence-dir", type=pathlib.Path, required=True, help="external directory for raw PTY/session evidence")
    parser.add_argument("--source-commit", default="", help="source commit label; defaults to the checked-out HEAD")
    args = parser.parse_args()
    BINARY = args.binary.resolve()
    EVIDENCE = args.evidence_dir.resolve()
    SOURCE_COMMIT = args.source_commit or subprocess.check_output(
        ["git", "-C", str(REPO), "rev-parse", "HEAD"], text=True
    ).strip()
    EVIDENCE.mkdir(parents=True, exist_ok=True)
    load_pyte()
    if not BINARY.is_file():
        raise SystemExit(f"build CLI first: {BINARY}")
    binary_hash = hashlib.sha256(BINARY.read_bytes()).hexdigest()
    evidence = {
        "source_commit": SOURCE_COMMIT,
        "binary_path": str(BINARY),
        "binary_sha256": binary_hash,
        "fixture": "local deterministic HTTP/SSE provider; synthetic AWS-key-shaped output only",
        "worktree": str(REPO),
        "journeys": [],
    }
    results = []
    for test in (test_success, test_cancellation, test_long_transcript_viewport):
        name = test.__name__
        print(f"RUN {name}", flush=True)
        result = test()
        results.append(result)
        print(f"PASS {name}", flush=True)
    evidence["journeys"] = results
    (EVIDENCE / "acceptance-results.json").write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")
    (EVIDENCE / "acceptance-summary.txt").write_text(
        "\n".join(
            [
                f"source_commit={SOURCE_COMMIT}",
                f"binary_sha256={binary_hash}",
                "success=all three real CLI PTY journeys passed",
                f"journeys={len(results)}",
                "canary=synthetic only; raw values omitted from provider event trace and sanitized out of stored capture files",
                f"results={EVIDENCE / 'acceptance-results.json'}",
            ]
        ) + "\n",
        encoding="utf-8",
    )
    print(f"PASS all journeys={len(results)} evidence={EVIDENCE}", flush=True)


if __name__ == "__main__":
    main()
