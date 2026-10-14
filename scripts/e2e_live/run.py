#!/usr/bin/env python3
"""Exercise the real go-cli PTY against the signed-in Magpie Codex gateway."""

from __future__ import annotations

import argparse
import base64
import errno
import fcntl
import hashlib
import json
import os
import pty
import re
import select
import shutil
import signal
import struct
import subprocess
import sys
import termios
import threading
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
TERMINAL_HARNESS = REPO / "scripts" / "e2e_terminal"
RENDERER = TERMINAL_HARNESS / "renderer.cjs"
EVIDENCE_ROOT = REPO / "docs" / "qa" / "2026-10-14" / "live" / "terminal"
WORKSPACE_ROOT = REPO / "docs" / "qa" / "2026-10-14" / "live" / "workspaces"
DEFAULT_GATEWAY = "http://127.0.0.1:3425"
MODEL = "codex/gpt-6.1-sol"
SENTINEL_API_KEY = "magpie"
APPROVAL_RE = re.compile(r"details\s+(\d+)-(\d+)/(\d+)", re.IGNORECASE)
APPROVAL_TOOL_RE = re.compile(r"\[approval\]\s+([a-z][a-z0-9_-]*)", re.IGNORECASE)
APPROVAL_DECISION_RE = re.compile(r"\[(?:y)\]\s*(?:once|review details|locked)[^\[]*\[(?:n)\]\s*(?:reject|no)", re.IGNORECASE)
APPROVAL_ONCE_RE = re.compile(r"\[(?:y)\]\s*once\b", re.IGNORECASE)
TURN_STATUS_RE = re.compile(r"\bturn\s+#\d+\b", re.IGNORECASE)
ASSISTANT_BUSY_LABEL = "[Assistant response is being generated and checked]"
UNKNOWN_CANCEL_WARNING = "[interrupted] The run outcome remains unknown; this is not a rollback."
LEGACY_CANCEL_NOTICE = "[interrupted]"
MAX_NONSTREAM_METADATA_BYTES = 16 * 1024 * 1024


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def normalize(lines: list[str]) -> str:
    return re.sub(r"\s+", " ", " ".join(lines)).strip()


def as_text(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        parts = []
        for item in value:
            if isinstance(item, dict):
                parts.append(str(item.get("text", "")))
        return " ".join(parts)
    return ""


def classify_request_kind(stream: Any, expected_input_markers: set[str], user_markers: list[str]) -> tuple[str, list[str]]:
    matches = sorted(expected_input_markers.intersection(user_markers))
    if stream is not True:
        return "internal-nonstream", matches
    if expected_input_markers and not matches:
        return "streamed-unmatched", matches
    return "interactive-user-turn", matches


class LiveForwarder(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, gateway: str, session_id: str, memory_canary: str = "", expected_fixture_facts: dict[str, str] | None = None):
        self.gateway = gateway.rstrip("/")
        self.session_id = session_id
        self.memory_canary = memory_canary
        self.expected_fixture_facts = dict(expected_fixture_facts or {})
        self.lock = threading.RLock()
        self.sequence = 0
        self.current_label = "setup"
        self.current_expected_markers: set[str] = set()
        self.current_expected_input_markers: set[str] = set()
        self.turn_labels: list[dict[str, Any]] = []
        self.inflight = 0
        self.last_completed_at = 0.0
        self.events: list[dict[str, Any]] = []
        self.requests: list[dict[str, Any]] = []
        super().__init__(("127.0.0.1", 0), LiveForwarderHandler)

    def label_turn(
        self,
        label: str,
        expected_response_markers: list[str] | None = None,
        expected_input_markers: list[str] | None = None,
        expect_provider: bool = True,
    ) -> None:
        with self.lock:
            self.current_label = label
            self.current_expected_markers = set(expected_response_markers or [])
            self.current_expected_input_markers = set(expected_input_markers or [])
            self.turn_labels.append({
                "label": label,
                "expectedInputMarkers": sorted(self.current_expected_input_markers),
                "providerExpected": expect_provider,
            })

    def begin(self, method: str, path: str, body: bytes) -> tuple[int, str]:
        metadata = request_metadata(body, self.memory_canary, self.expected_fixture_facts)
        with self.lock:
            self.sequence += 1
            index = self.sequence
            self.inflight += 1
            request_kind, input_marker_matches = classify_request_kind(
                metadata.get("stream"), self.current_expected_input_markers, metadata.get("userMarkers", [])
            )
            record = {
                "sequence": index,
                "turn": self.current_label,
                "requestKind": request_kind,
                "method": method,
                "path": path,
                "startedAt": now(),
                "requestBytes": len(body),
                "requestSha256": sha256(body) if body else None,
                "askedModel": metadata.get("model"),
                "stream": metadata.get("stream"),
                "toolReceipts": metadata.get("toolReceipts", []),
                "userMarkers": metadata.get("userMarkers", []),
                "expectedInputMarkers": sorted(self.current_expected_input_markers),
                "inputMarkerMatches": input_marker_matches,
                "memoryCanaryPresent": metadata.get("memoryCanaryPresent", False),
                "memoryCanaryMessageRoles": metadata.get("memoryCanaryMessageRoles", []),
                "toolCalls": [],
                "priorToolCalls": metadata.get("priorToolCalls", []),
                "provider": None,
                "servedModel": None,
                "requestId": None,
                "responseId": None,
                "usage": None,
                "finishReasons": [],
                "terminalStreamMarker": False,
                "routeSnapshot": None,
                "status": None,
                "completedAt": None,
                "expectedArtifactReferenced": metadata.get("expectedArtifactReferenced", False),
                "expectedResponseMarkers": sorted(self.current_expected_markers),
                "responseMarkerMatches": {marker: False for marker in self.current_expected_markers},
                "responseExplainsPolicyRejection": False,
                "responseBytes": 0,
                "_responseHasher": hashlib.sha256(),
                "_assistantText": "",
                "_nonstreamBody": bytearray(),
                "_nonstreamBodyComplete": True,
                "responseJsonParsed": False,
                "responseMetadataError": None,
            }
            self.requests.append(record)
        return index, self.current_label

    def response_chunk(self, index: int, payload: bytes) -> None:
        if not payload:
            return
        with self.lock:
            record = self.requests[index - 1]
            record["responseBytes"] += len(payload)
            record["_responseHasher"].update(payload)
        with self.lock:
            record = self.requests[index - 1]
            is_stream = record.get("stream")
            if not is_stream:
                body = record["_nonstreamBody"]
                if len(body) + len(payload) <= MAX_NONSTREAM_METADATA_BYTES:
                    body.extend(payload)
                else:
                    record["_nonstreamBodyComplete"] = False
                    body.clear()
        if is_stream:
            try:
                for line in payload.splitlines():
                    if not line.startswith(b"data:"):
                        continue
                    raw = line[5:].strip()
                    if raw == b"[DONE]":
                        with self.lock:
                            record["terminalStreamMarker"] = True
                        continue
                    self.merge_response(index, json.loads(raw))
            except (ValueError, json.JSONDecodeError, IndexError):
                # Response bytes are still forwarded exactly. Metadata capture
                # is best-effort and does not persist response content.
                return

    def merge_response(self, index: int, value: dict[str, Any]) -> None:
        with self.lock:
            record = self.requests[index - 1]
            record["responseJsonParsed"] = True
            record["responseId"] = value.get("id") or record.get("responseId")
            record["servedModel"] = value.get("model") or record.get("servedModel")
            usage = value.get("usage")
            if isinstance(usage, dict):
                record["usage"] = {
                    key: usage[key]
                    for key in ("prompt_tokens", "completion_tokens", "total_tokens", "input_tokens", "output_tokens")
                    if key in usage
                }
            choices = value.get("choices") or []
            for choice in choices:
                finish_reason = choice.get("finish_reason")
                if finish_reason and finish_reason not in record["finishReasons"]:
                    record["finishReasons"].append(finish_reason)
                message = choice.get("message") or {}
                for call in message.get("tool_calls") or []:
                    self._record_tool_call(record, call, call.get("index"))
                delta = choice.get("delta") or {}
                for call in delta.get("tool_calls") or []:
                    self._record_tool_call(record, call, call.get("index"))
                content = delta.get("content")
                if isinstance(content, str) and content:
                    record["_assistantText"] += content
                message_content = message.get("content")
                if isinstance(message_content, str) and message_content:
                    record["_assistantText"] += message_content
                for marker in record["expectedResponseMarkers"]:
                    record["responseMarkerMatches"][marker] = marker in record["_assistantText"]
                answer = record["_assistantText"].lower()
                record["responseExplainsPolicyRejection"] = any(
                    phrase in answer for phrase in (
                        "command blocked", "blocked by policy", "python3 is blocked",
                        "python3 is blacklisted", "blacklisted", "did not run", "was not run",
                    )
                )

    @staticmethod
    def _record_tool_call(record: dict[str, Any], call: dict[str, Any], call_index: Any = None) -> None:
        identifier = call.get("id")
        function = call.get("function") or {}
        name = function.get("name")
        arguments = function.get("arguments", "")
        if not isinstance(arguments, str):
            arguments = json.dumps(arguments, separators=(",", ":"), ensure_ascii=False)
        existing = next(
            (
                item
                for item in record["toolCalls"]
                if (identifier and item.get("id") == identifier)
                or (call_index is not None and item.get("index") == call_index)
            ),
            None,
        )
        if existing is None:
            record["toolCalls"].append({"id": identifier, "index": call_index, "name": name, "argumentsSha256": sha256(arguments.encode()), "_arguments": arguments})
        else:
            # Chat-completions streams may emit an ID/name and argument text in
            # separate chunks. Update the digest from the accumulated text held
            # only in memory by the event handler, never the evidence report.
            existing["name"] = name or existing.get("name")
            existing["id"] = identifier or existing.get("id")
            existing["argumentsSha256"] = sha256((existing.get("_arguments", "") + arguments).encode())
            existing["_arguments"] = existing.get("_arguments", "") + arguments

    def finish(self, index: int, status: int, response_headers: Any) -> None:
        route_snapshot = None
        request_row = self.requests[index - 1]
        if not request_row.get("stream"):
            body = bytes(request_row.get("_nonstreamBody", b""))
            complete = request_row.get("_nonstreamBodyComplete") is True
            if complete and body:
                try:
                    parsed = json.loads(body)
                    if isinstance(parsed, dict):
                        self.merge_response(index, parsed)
                    else:
                        request_row["responseMetadataError"] = "JSON root was not an object"
                except (ValueError, json.JSONDecodeError):
                    request_row["responseMetadataError"] = "invalid or incomplete JSON response"
            elif not complete:
                request_row["responseMetadataError"] = "metadata body exceeded capture limit"
            request_row["_nonstreamBody"] = bytearray()
        if request_row.get("method") == "POST" and "chat/completions" in request_row.get("path", ""):
            try:
                query = urllib.parse.urlencode({"session": self.session_id})
                with urllib.request.urlopen(f"{self.gateway}/v1/magpie/route?{query}", timeout=10) as response:
                    route_snapshot = json.loads(response.read())
                route_snapshot = redact_route_snapshot(route_snapshot, index)
            except Exception as error:
                route_snapshot = {"captureError": type(error).__name__, "bridgeSequence": index}
        with self.lock:
            record = self.requests[index - 1]
            record["status"] = status
            record["provider"] = response_headers.get("X-Magpie-Provider")
            record["servedModel"] = response_headers.get("X-Magpie-Model") or record.get("servedModel")
            record["requestId"] = (
                response_headers.get("X-Request-Id")
                or response_headers.get("Request-Id")
                or response_headers.get("X-Magpie-Request-Id")
            )
            hasher = record.get("_responseHasher")
            record["responseSha256"] = hasher.hexdigest() if hasher is not None else None
            record["routeSnapshot"] = route_snapshot
            record["completedAt"] = now()
            self.inflight = max(0, self.inflight - 1)
            self.last_completed_at = time.monotonic()
            self.events.append({"sequence": index, "turn": record["turn"], "kind": "upstream_complete", "status": status})

    def snapshot(self) -> dict[str, Any]:
        with self.lock:
            # Drop any transient streaming accumulator before persisting.
            sanitized = []
            for request in self.requests:
                row = dict(request)
                row["toolCalls"] = [
                    {
                        **{key: value for key, value in item.items() if not key.startswith("_")},
                        "safeArguments": summarize_tool_arguments(item.get("name", ""), item.get("_arguments", "")),
                    }
                    for item in row.get("toolCalls", [])
                ]
                hasher = row.pop("_responseHasher", None)
                if hasher is not None:
                    row["responseSha256"] = hasher.hexdigest()
                row.pop("_assistantText", None)
                row.pop("_nonstreamBody", None)
                sanitized.append(row)
            return {"sessionId": self.session_id, "requests": sanitized, "events": list(self.events), "turnLabels": list(self.turn_labels)}


def request_metadata(body: bytes, memory_canary: str = "", expected_fixture_facts: dict[str, str] | None = None) -> dict[str, Any]:
    if not body:
        return {}
    try:
        data = json.loads(body)
    except (ValueError, json.JSONDecodeError):
        return {}
    messages = data.get("messages") or []
    receipts: list[dict[str, Any]] = []
    prior_calls: list[dict[str, Any]] = []
    markers: list[str] = []
    canary_roles: list[str] = []
    expected_artifact = os.environ.get("LIVE_E2E_EXPECTED_ARTIFACT", "")
    for message in messages:
        role = message.get("role")
        content = as_text(message.get("content"))
        if memory_canary and memory_canary in content and role not in canary_roles:
            canary_roles.append(str(role))
        if role == "tool":
            call_id = message.get("tool_call_id")
            receipts.append({
                "toolCallId": call_id,
                "tool": message.get("name"),
                "bytes": len(content.encode("utf-8")),
                "sha256": sha256(content.encode("utf-8")),
                "mentionsDenied": "denied" in content.lower(),
                "mentionsError": any(term in content.lower() for term in ("error", "failed", "blocked", "denied")),
                "mentionsPolicyBlocked": "command blocked" in content.lower(),
                "fixtureFactsMatched": sorted(
                    label for label, fact in (expected_fixture_facts or {}).items() if fact and fact in content
                ),
            })
        if role == "assistant":
            for call in message.get("tool_calls") or []:
                function = call.get("function") or {}
                arguments = function.get("arguments", "")
                prior_calls.append({
                    "toolCallId": call.get("id"),
                    "tool": function.get("name"),
                    "argumentsSha256": sha256(str(arguments).encode("utf-8")),
                })
        if role == "user":
            for marker in re.findall(r"\b(?:LIVE_[A-Z0-9_]+|FOLLOWUP_[A-Z0-9_]+)\b", content):
                markers.append(marker)
    joined = "\n".join(as_text(message.get("content")) for message in messages)
    return {
        "model": data.get("model"),
        "stream": data.get("stream"),
        "toolReceipts": receipts,
        "priorToolCalls": prior_calls,
        "userMarkers": markers,
        "memoryCanaryPresent": bool(memory_canary and memory_canary in joined),
        "memoryCanaryMessageRoles": canary_roles,
        "expectedArtifactReferenced": bool(expected_artifact and expected_artifact in joined),
    }


def summarize_tool_arguments(tool: str, raw_arguments: str) -> dict[str, Any]:
    """Keep useful path/nonce evidence while excluding arbitrary command text."""
    try:
        value = json.loads(raw_arguments)
    except (TypeError, ValueError, json.JSONDecodeError):
        return {}
    if not isinstance(value, dict):
        return {}
    result: dict[str, Any] = {}
    path_value = next((value.get(key) for key in ("path", "file_path", "filename", "file") if isinstance(value.get(key), str)), None)
    if path_value:
        path = Path(path_value)
        result["pathBasename"] = path.name
        if not path.is_absolute() and ".." not in path.parts and len(path_value) <= 160:
            result["relativePath"] = path_value
    pattern = next((value.get(key) for key in ("pattern", "search", "old_text", "old") if isinstance(value.get(key), str)), "")
    markers = sorted(set(re.findall(r"\bLIVE_(?:FACT|REVIEW)_[A-Z0-9_]+\b", pattern)))
    if markers:
        result["fixtureMarkers"] = markers
    expected_artifact = os.environ.get("LIVE_E2E_EXPECTED_ARTIFACT", "")
    if expected_artifact:
        result["expectedArtifactMentioned"] = expected_artifact in raw_arguments
    expected_helper = os.environ.get("LIVE_E2E_EXPECTED_HELPER", "")
    if expected_helper:
        result["expectedHelperMentioned"] = expected_helper in raw_arguments
    if tool == "bash":
        # Never store Bash's command body: it can contain arbitrary content.
        result["commandSha256"] = sha256(raw_arguments.encode("utf-8"))
        command = value.get("command")
        if not isinstance(command, str):
            command = ""
        if expected_helper:
            result["expectedHelperExecuted"] = expected_helper in command and bool(re.search(r"\bgo\s+run\b", command))
        if expected_artifact:
            result["expectedArtifactCommandArgument"] = expected_artifact in command
        expected_policy_command = os.environ.get("LIVE_E2E_EXPECTED_POLICY_COMMAND", "")
        if expected_policy_command:
            result["policyCommandMatchesExact"] = command == expected_policy_command
    return result


def redact_route_snapshot(payload: Any, bridge_sequence: int) -> dict[str, Any]:
    if not isinstance(payload, (dict, list)):
        return {"bridgeSequence": bridge_sequence, "payloadType": type(payload).__name__}
    candidates = payload if isinstance(payload, list) else payload.get("rows", payload.get("routes", [payload]))
    if not isinstance(candidates, list):
        candidates = [payload]
    candidate = next((item for item in reversed(candidates) if isinstance(item, dict)), payload)
    route = candidate.get("route", candidate)
    if not isinstance(route, dict):
        route = {}
    result = {key: route.get(key) for key in ("id", "time", "ms", "asked", "model", "served", "tier_served", "status", "done", "tries") if key in route}
    result.update({"captureBridgeSequence": bridge_sequence, "session": candidate.get("session", payload.get("session") if isinstance(payload, dict) else None)})
    if "seq" in candidate:
        result["routeStateSequence"] = candidate.get("seq")
    elif isinstance(payload, dict) and "seq" in payload:
        result["routeStateSequence"] = payload.get("seq")
    return result


class LiveForwarderHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        self._forward(b"")

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        length = int(self.headers.get("Content-Length", "0"))
        if length < 0 or length > 32_000_000:
            self.send_error(413)
            return
        self._forward(self.rfile.read(length))

    def _forward(self, body: bytes) -> None:
        bridge: LiveForwarder = self.server  # type: ignore[assignment]
        index, _ = bridge.begin(self.command, self.path, body)
        url = bridge.gateway + self.path
        headers = {"X-Magpie-Session": bridge.session_id, "X-Magpie-Agent": "go-cli-live-e2e"}
        for name in ("Authorization", "Content-Type", "Accept", "OpenAI-Organization", "OpenAI-Project"):
            if self.headers.get(name):
                headers[name] = self.headers[name]
        request = urllib.request.Request(url, data=body if self.command in {"POST", "PUT", "PATCH"} else None, headers=headers, method=self.command)
        try:
            upstream = urllib.request.urlopen(request, timeout=900)
        except urllib.error.HTTPError as error:
            upstream = error
        except Exception:
            bridge.finish(index, 502, {})
            self.send_error(502, "Magpie forwarder could not reach the configured loopback gateway")
            return

        status = int(getattr(upstream, "status", getattr(upstream, "code", 502)))
        content_type = upstream.headers.get("Content-Type", "application/octet-stream")
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Connection", "close")
        for name in ("Cache-Control", "X-Magpie-Provider", "X-Magpie-Model", "X-Request-Id", "Request-Id", "X-Magpie-Request-Id"):
            value = upstream.headers.get(name)
            if value:
                self.send_header(name, value)
        self.end_headers()
        try:
            while True:
                chunk = upstream.readline(131072) if "text/event-stream" in content_type else upstream.read(65536)
                if not chunk:
                    break
                bridge.response_chunk(index, chunk)
                self.wfile.write(chunk)
                self.wfile.flush()
                if "text/event-stream" not in content_type:
                    # A single JSON body is consumed in one pass; stop after
                    # the first read only when EOF arrives on the next read.
                    pass
        except (BrokenPipeError, ConnectionResetError, OSError):
            bridge.events.append({"sequence": index, "kind": "client_stream_closed"})
        finally:
            upstream.close()
            bridge.finish(index, status, upstream.headers)
            self.close_connection = True

    def log_message(self, _format: str, *_args: object) -> None:
        # HTTP headers, including Authorization, are intentionally never logged.
        return


class Renderer:
    def __init__(self, node: str, helper: Path, env: dict[str, str]):
        self.proc = subprocess.Popen([node, str(helper)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, encoding="utf-8", bufsize=1, env=env)
        ready = self._read(time.monotonic() + 60)
        if not ready.get("ready"):
            raise RuntimeError(f"xterm renderer failed: {ready.get('error', 'no ready response')}")
        self.cols = int(ready["cols"])
        self.rows = int(ready["rows"])

    def _read(self, deadline: float) -> dict[str, Any]:
        assert self.proc.stdout is not None
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                error = self.proc.stderr.read() if self.proc.stderr else ""
                raise RuntimeError(f"xterm renderer exited ({self.proc.returncode}): {error[-1200:]}")
            ready, _, _ = select.select([self.proc.stdout], [], [], min(0.25, deadline - time.monotonic()))
            if ready:
                line = self.proc.stdout.readline()
                if line:
                    result = json.loads(line)
                    if not result.get("ok", True):
                        raise RuntimeError(f"xterm renderer error: {result.get('error', 'unknown error')}")
                    return result
        raise TimeoutError("timed out waiting for xterm renderer")

    def ask(self, value: dict[str, Any], timeout: float = 30) -> dict[str, Any]:
        if self.proc.stdin is None:
            raise RuntimeError("renderer stdin is closed")
        self.proc.stdin.write(json.dumps(value, separators=(",", ":")) + "\n")
        self.proc.stdin.flush()
        return self._read(time.monotonic() + timeout)

    def write(self, data: bytes) -> None:
        self.ask({"op": "write", "data": base64.b64encode(data).decode("ascii")})

    def screen(self) -> list[str]:
        result = self.ask({"op": "screen"})
        self.cols, self.rows = int(result["cols"]), int(result["rows"])
        return list(result.get("lines", []))

    def screenshot(self, path: Path) -> dict[str, Any]:
        result = self.ask({"op": "snapshot", "path": str(path)})
        self.cols, self.rows = int(result["cols"]), int(result["rows"])
        return result

    def close(self) -> None:
        if self.proc.poll() is None:
            try:
                self.ask({"op": "close"}, timeout=10)
            except (OSError, RuntimeError, TimeoutError):
                self.proc.terminate()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait(timeout=3)


class LivePTY:
    def __init__(self, binary: Path, home: Path, cwd: Path, bridge: LiveForwarder, node: str, model: str, max_tokens: int, viewport: tuple[int, int] = (1440, 900)):
        self.binary, self.home, self.cwd, self.bridge, self.model = binary, home, cwd, bridge, model
        self.viewport = viewport
        helper_env = os.environ.copy()
        helper_env["E2E_VIEWPORT_WIDTH"] = str(viewport[0])
        helper_env["E2E_VIEWPORT_HEIGHT"] = str(viewport[1])
        module = TERMINAL_HARNESS / "node_modules" / "playwright"
        if module.exists():
            helper_env["E2E_PLAYWRIGHT_MODULE"] = str(module)
        self.renderer = Renderer(node, RENDERER, helper_env)
        child_env = self.child_env()
        pid, master_fd = pty.fork()
        if pid == 0:
            try:
                os.chdir(cwd)
                args = [str(binary), "interactive", "--no-onboarding", "--provider", "openai", "--model", model, "--thinking", "none", "--max-tokens", str(max_tokens)]
                os.execvpe(str(binary), args, child_env)
            except BaseException as error:  # pragma: no cover - subprocess failure path
                os.write(2, f"live e2e exec failure: {type(error).__name__}\n".encode())
                os._exit(127)
        self.pid = pid
        self.exit_code: int | None = None
        self.master_fd = master_fd
        self.raw_bytes = bytearray()
        self.input_deliveries: list[dict[str, Any]] = []
        self.decided_tool_call_ids: set[str] = set()
        self.session_store_path: Path | None = None
        self.turn_terminal_baseline: set[str] = set()
        self._resize(self.renderer.cols, self.renderer.rows)

    def child_env(self) -> dict[str, str]:
        # Bash tool calls run with this environment. Use an explicit allowlist
        # so host credentials, CODEX_HOME, SSH agents, and cloud tokens cannot
        # be inherited by the model-directed shell.
        system_path = [str(Path(sys.executable).parent), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"]
        path_entries = list(dict.fromkeys(item for item in system_path if item and Path(item).is_dir()))
        env = {
            "PATH": os.pathsep.join(path_entries),
            "HOME": str(self.home),
            "XDG_CONFIG_HOME": str(self.home / ".config"),
            "TMPDIR": str(self.home / "tmp"),
            "TERM": "xterm-256color",
            "COLORTERM": "truecolor",
            "LANG": "en_US.UTF-8",
            "LC_ALL": "en_US.UTF-8",
            "GO_CLI_PROVIDER_NAME": "openai",
            "GO_CLI_PROVIDER_BASE_URL": f"http://127.0.0.1:{self.bridge.server_port}/v1",
            "GO_CLI_PROVIDER_API_KEY": SENTINEL_API_KEY,
            "GO_CLI_PROVIDER_MODEL": self.model,
            "GO_CLI_MODEL_NAME": self.model,
            "GO_CLI_TRACING_ENABLED": "true",
            "GO_CLI_TRACING_EXPORTER": "jsonl",
            "GO_CLI_TRACING_FILE_PATH": os.environ.get("LIVE_E2E_TRACE_DIR", ""),
            "GO_CLI_EXTENSIONS_ENABLED": "false",
            "GO_CLI_APPROVAL_MODE": "ask",
            "LIVE_E2E_EXPECTED_ARTIFACT": os.environ.get("LIVE_E2E_EXPECTED_ARTIFACT", ""),
        }
        Path(env["TMPDIR"]).mkdir(parents=True, exist_ok=True)
        return env

    def _resize(self, cols: int, rows: int) -> None:
        fcntl.ioctl(self.master_fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        try:
            os.kill(self.pid, signal.SIGWINCH)
        except ProcessLookupError:
            pass

    def send(self, value: bytes) -> None:
        os.write(self.master_fd, value)

    def send_line(self, value: str) -> None:
        user_bytes = value.encode("utf-8")
        # The CLI enables xterm bracketed paste in its actual line editor.
        # Sending the large pressure prompts through that supported input path
        # avoids a redraw for each typed character while preserving the exact
        # user-supplied bytes; Enter remains a separate final byte.
        payload = b"\x1b[200~" + user_bytes + b"\x1b[201~\r"
        total_bytes = len(payload)
        was_blocking = os.get_blocking(self.master_fd)
        if was_blocking:
            os.set_blocking(self.master_fd, False)
        started = time.monotonic()
        deadline = started + 120
        offset = 0
        written_total = 0
        failure = None
        try:
            while offset < total_bytes:
                if time.monotonic() >= deadline:
                    raise TimeoutError(f"PTY bracketed-paste write timed out after {written_total}/{total_bytes} bytes")
                try:
                    written = os.write(self.master_fd, payload[offset:offset + 4096])
                except BlockingIOError:
                    self.pump(0.01)
                    continue
                if written <= 0:
                    raise OSError("PTY prompt write returned no progress")
                written_total += written
                offset += written
                self.pump(0.005)
        except BaseException as error:
            failure = f"{type(error).__name__}: {error}"
            raise
        finally:
            if was_blocking:
                os.set_blocking(self.master_fd, True)
            self.input_deliveries.append({
                "method": "xterm-bracketed-paste-then-enter",
                "userInputBytes": len(user_bytes),
                "userInputSha256": sha256(user_bytes),
                "framedBytes": total_bytes,
                "framedBytesWritten": written_total,
                "complete": offset == total_bytes,
                "elapsedSeconds": round(time.monotonic() - started, 3),
                "failure": failure,
            })

    def pump(self, timeout: float = 0.1) -> bool:
        ready, _, _ = select.select([self.master_fd], [], [], timeout)
        if not ready:
            return False
        try:
            data = os.read(self.master_fd, 65536)
        except BlockingIOError:
            return False
        except OSError as error:
            if error.errno in {errno.EIO, errno.EBADF}:
                return False
            raise
        if data:
            self.raw_bytes.extend(data)
            self.renderer.write(data)
            return True
        return False

    def screen(self) -> str:
        return normalize(self.renderer.screen())

    def screen_lines(self) -> list[str]:
        return self.renderer.screen()

    def wait_for(self, needle: str, timeout: float) -> str:
        deadline = time.monotonic() + timeout
        last = ""
        while time.monotonic() < deadline:
            self.pump(min(0.1, max(0.01, deadline - time.monotonic())))
            last = self.screen()
            if needle.lower() in last.lower():
                return last
            if self.exit_status() is not None:
                break
        raise TimeoutError(f"timed out waiting for {needle!r}; last screen: {last[-1000:]}")

    def wait_until(self, predicate: Any, description: str, timeout: float) -> tuple[Any, list[str]]:
        deadline = time.monotonic() + timeout
        last = ""
        while time.monotonic() < deadline:
            self.pump(min(0.1, max(0.01, deadline - time.monotonic())))
            lines = self.screen_lines()
            last = normalize(lines)
            value = predicate(lines)
            if value:
                return value, lines
            if self.exit_status() is not None:
                break
        raise TimeoutError(f"timed out waiting for {description}; last screen: {last[-1000:]}")

    def exit_status(self) -> int | None:
        if self.pid is None:
            return self.exit_code
        try:
            pid, status = os.waitpid(self.pid, os.WNOHANG)
        except ChildProcessError:
            self.pid = None
            return self.exit_code
        if pid == 0:
            return None
        self.pid = None
        self.exit_code = os.waitstatus_to_exitcode(status)
        return self.exit_code

    def close(self) -> int:
        status = self.exit_status()
        if status is None:
            self.send_line("exit")
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                self.pump(0.1)
                status = self.exit_status()
                if status is not None:
                    break
            if status is None and self.pid is not None:
                try:
                    os.killpg(self.pid, signal.SIGTERM)
                except (ProcessLookupError, PermissionError):
                    pass
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    self.pump(0.1)
                    status = self.exit_status()
                    if status is not None:
                        break
        try:
            os.close(self.master_fd)
        except OSError:
            pass
        self.renderer.close()
        return status if status is not None else 1

    def capture(self, output_dir: Path, name: str, assertions: list[str] | None = None) -> dict[str, Any]:
        path = output_dir / f"{name}.png"
        capture = self.renderer.screenshot(path)
        screen = normalize(capture.get("lines", []))
        width, height = png_dimensions(path)
        if (width, height) != self.viewport:
            raise AssertionError(f"PNG viewport mismatch for {name}: got {(width, height)} expected {self.viewport}")
        missing = [item for item in (assertions or []) if item.lower() not in screen.lower()]
        if missing:
            raise AssertionError(f"screenshot {name} missing visible assertions {missing}: {screen[-800:]}")
        return {
            "file": path.name,
            "sha256": sha256_file(path),
            "source": "headless Chromium rendering xterm.js fed with raw bytes read from the real CLI PTY",
            "assertions": {item: item.lower() in screen.lower() for item in assertions or []},
            "screenExcerpt": screen[-1000:],
            "terminal": {"widthPx": width, "heightPx": height, "cols": capture.get("cols"), "rows": capture.get("rows"), "coloredCells": capture.get("colored_cells")},
        }

    def save_transcript(self, output: Path) -> dict[str, Any]:
        data = bytes(self.raw_bytes).replace(SENTINEL_API_KEY.encode(), b"[local API sentinel redacted]")
        output.write_bytes(data)
        return {"path": str(output), "bytes": len(data), "sha256": sha256(data), "source": "real CLI PTY raw bytes before xterm rendering"}


def png_dimensions(path: Path) -> tuple[int, int]:
    header = path.read_bytes()[:24]
    if len(header) != 24 or header[:8] != b"\x89PNG\r\n\x1a\n" or header[12:16] != b"IHDR":
        raise ValueError(f"not a PNG file: {path}")
    return struct.unpack(">II", header[16:24])


def locate_node() -> str:
    node = os.environ.get("E2E_NODE") or shutil.which("node")
    if not node or not os.access(node, os.X_OK):
        raise RuntimeError("Node.js is unavailable; set E2E_NODE")
    return node


def run_rtk(args: list[str], cwd: Path, timeout: int = 300) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["rtk", "proxy", *args], cwd=cwd, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout, check=False)


def ensure_renderer_dependencies(node: str) -> None:
    package_dir = TERMINAL_HARNESS / "node_modules" / "playwright"
    if package_dir.exists():
        return
    result = run_rtk(["npm", "ci"], TERMINAL_HARNESS, timeout=300)
    if result.returncode:
        raise RuntimeError(f"pinned xterm/Playwright install failed: {result.stdout[-2000:]}")
    if not package_dir.exists():
        raise RuntimeError("pinned renderer dependencies were not installed")


def build_binary(run_dir: Path) -> tuple[Path, str]:
    binary = run_dir / "bin" / "go-cli-race"
    binary.parent.mkdir(parents=True, exist_ok=True)
    result = run_rtk(["go", "build", "-race", "-o", str(binary), "./cmd/cli"], REPO, timeout=900)
    if result.returncode:
        raise RuntimeError(f"real go-cli -race build failed: {result.stdout[-3000:]}")
    return binary, sha256(binary.read_bytes())


def reuse_binary(source_binary: Path, run_dir: Path) -> tuple[Path, str, dict[str, Any]]:
    source = source_binary.expanduser().resolve(strict=True)
    if not source.is_file() or not os.access(source, os.X_OK):
        raise RuntimeError(f"pinned reuse binary is not an executable file: {source}")
    target = run_dir / "bin" / "go-cli-race"
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, target)
    source_hash = sha256(source.read_bytes())
    target_hash = sha256(target.read_bytes())
    if target_hash != source_hash:
        raise RuntimeError("reused race binary hash changed while copying")
    source_run = source.parent.parent
    origin_report = EVIDENCE_ROOT / source_run.name / "run-report.json"
    origin: dict[str, Any] = {
        "sourceBinary": str(source),
        "sourceRunDirectory": str(source_run),
        "sourceEvidenceReport": str(origin_report),
        "sha256": source_hash,
        "copiedByteIdentically": True,
    }
    if origin_report.is_file():
        try:
            report = json.loads(origin_report.read_text(encoding="utf-8"))
            origin["sourceRunId"] = report.get("runId")
            origin["sourceRevision"] = report.get("revision")
            origin["sourceRaceBuildSha256"] = (report.get("raceBuild") or {}).get("sha256")
            origin["sourceGoSourceSnapshot"] = report.get("goSourceSnapshot")
            if origin["sourceRaceBuildSha256"] and origin["sourceRaceBuildSha256"] != source_hash:
                raise RuntimeError("origin report race-build hash does not match the binary being reused")
        except json.JSONDecodeError as error:
            raise RuntimeError(f"pinned binary origin report is invalid JSON: {origin_report}") from error
    return target, target_hash, origin


def gateway_health(gateway: str, model: str) -> dict[str, Any]:
    url = gateway.rstrip("/") + "/v1/models"
    with urllib.request.urlopen(url, timeout=20) as response:
        payload = json.loads(response.read())
    names = [row.get("id", "") for row in payload.get("data", []) if isinstance(row, dict)]
    if model not in names:
        raise RuntimeError(f"Magpie /v1/models did not expose configured model {model!r}; ids received: {names[:30]}")
    return {"httpStatus": 200, "modelListed": model in names, "modelCount": len(names), "gateway": gateway}


def approval_status(screen: str | list[str]) -> tuple[int, int, int] | None:
    block = active_approval_block(screen)
    if not block:
        return None
    value = normalize(block)
    match = APPROVAL_RE.search(value)
    if not match:
        return None
    return tuple(int(value) for value in match.groups())  # type: ignore[return-value]


def active_approval_tool(screen: str | list[str]) -> str | None:
    """Return the tool only for the current footer-attached approval modal.

    Transcript rows can retain old approval text. The live modal is rendered
    immediately above the status bar, with a fixed decision row at its end.
    Requiring that layout plus an unresolved provider tool ID avoids treating
    old transcript text as a fresh approval.
    """
    block = active_approval_block(screen)
    if not block:
        return None
    header = APPROVAL_TOOL_RE.search(block[0])
    return header.group(1).lower() if header else None


def active_approval_block(screen: str | list[str]) -> list[str] | None:
    lines = screen if isinstance(screen, list) else screen.splitlines()
    normalized = [line.strip() for line in lines]
    last_index = next((i for i in range(len(normalized) - 1, -1, -1) if normalized[i]), None)
    if last_index is None:
        return None

    # The full TUI status bar can be clipped when a review modal is taller than
    # the terminal. In that case the decision row is the last occupied viewport
    # row; when visible, it sits directly above the current status bar. Old
    # transcript text is not adjacent to either live footer location.
    if APPROVAL_DECISION_RE.search(normalized[last_index]):
        footer_index = last_index
    else:
        status_index = next((i for i in range(last_index, -1, -1) if TURN_STATUS_RE.search(normalized[i])), None)
        if status_index is None or status_index == 0 or not APPROVAL_DECISION_RE.search(normalized[status_index - 1]):
            return None
        footer_index = status_index - 1
    header_index = next(
        (i for i in range(footer_index - 1, -1, -1) if APPROVAL_TOOL_RE.search(normalized[i])),
        None,
    )
    if header_index is None:
        return None
    return normalized[header_index:footer_index + 1]


def approval_is_fully_reviewed(screen: str | list[str]) -> bool:
    block = active_approval_block(screen)
    return bool(block and any(APPROVAL_ONCE_RE.search(line) for line in block))


def active_busy_footer(lines: list[str]) -> bool:
    """Check only TUI's actual footer indicators, never transcript wording."""
    return active_busy_marker(lines) is not None


def active_busy_marker(lines: list[str]) -> str | None:
    """Return the exact current TUI footer marker, if one is visible."""
    for line in reversed(lines[-10:]):
        stripped = line.strip()
        if stripped == ASSISTANT_BUSY_LABEL:
            return ASSISTANT_BUSY_LABEL
        if re.search(r"\bworking\.\.\.\s*$", stripped, re.IGNORECASE):
            return "working..."
    return None


def active_followup_input(lines: list[str]) -> bool:
    """Recognize follow-up mode in the live footer next to this turn's status."""
    status_index = next((i for i in range(len(lines) - 1, -1, -1) if TURN_STATUS_RE.search(lines[i])), None)
    if status_index is None:
        return False
    footer_start = max(0, status_index - 10)
    return any(
        re.match(r"^\s*>\s*followup>\s*.*█\s*$", line, re.IGNORECASE)
        for line in lines[footer_start:status_index]
    )


def current_terminal_notice(lines: list[str], notice: str) -> bool:
    """Match a terminal notification as a whole current viewport line."""
    return any(line.strip() == notice for line in lines[-10:])


def current_viewport_notice(
    lines: list[str], phrase: str, start_prefix: str, recent_rows: int = 12
) -> bool:
    """Match a current viewport notice anchored at its emitted line start."""
    recent = lines[-recent_rows:]
    target = phrase.casefold()
    compact_target = re.sub(r"\s+", "", target)
    for index, line in enumerate(recent):
        if not line.strip().casefold().startswith(start_prefix.casefold()):
            continue
        # The next physical rows may continue the same xterm-wrapped notice.
        visible = normalize(recent[index : index + 5]).casefold()
        if target in visible or compact_target in re.sub(r"\s+", "", visible):
            return True
    return False


def current_unknown_cancel_warning(lines: list[str]) -> bool:
    """Match the new unknown-outcome notice in the current viewport only.

    The message may occupy several xterm rows. Joining recent visible rows
    recognizes ordinary visual wrapping while avoiding a match in old
    transcript or raw-PTY history.
    """
    return current_viewport_notice(lines, UNKNOWN_CANCEL_WARNING, "[interrupted]")


def current_legacy_cancel_notice(lines: list[str]) -> bool:
    """Keep the older known-cancel marker separate from unknown outcomes."""
    return current_terminal_notice(lines, LEGACY_CANCEL_NOTICE)


def idle_input_prompt(lines: list[str]) -> bool:
    last_nonempty = next((line.strip() for line in reversed(lines) if line.strip()), "")
    return last_nonempty == ">"


def unresolved_tool_call_ids(bridge: LiveForwarder, label: str, tool_name: str) -> set[str]:
    snapshot = bridge.snapshot()
    calls = {
        str(call.get("id"))
        for row in snapshot.get("requests", []) if row.get("turn") == label
        for call in row.get("toolCalls", [])
        if call.get("name") == tool_name and call.get("id")
    }
    receipts = {
        str(receipt.get("toolCallId"))
        for row in snapshot.get("requests", [])
        for receipt in row.get("toolReceipts", [])
        if receipt.get("toolCallId")
    }
    return calls - receipts


def wait_for_stable_approval_frame(
    session: LivePTY,
    expected_status: tuple[int, int, int],
    require_reviewed_footer: bool,
    timeout: float = 8,
) -> list[str]:
    """Drain PTY redraws until the exact approval page/footer is stable twice."""
    deadline = time.monotonic() + timeout
    previous: tuple[str, ...] | None = None
    stable_frames = 0
    latest: list[str] = []
    while time.monotonic() < deadline:
        session.pump(min(0.05, max(0.01, deadline - time.monotonic())))
        latest = session.screen_lines()
        status = approval_status(latest)
        ready = (
            active_approval_tool(latest) is not None
            and status == expected_status
            and (not require_reviewed_footer or approval_is_fully_reviewed(latest))
        )
        if not ready:
            previous = None
            stable_frames = 0
            continue
        signature = tuple(latest)
        if signature == previous:
            stable_frames += 1
            if stable_frames >= 1:
                return latest
        else:
            previous = signature
            stable_frames = 0
    expectation = "unlocked reviewed footer" if require_reviewed_footer else f"approval page {expected_status}"
    raise TimeoutError(f"approval did not reach a stable {expectation}: {normalize(latest)[-1000:]}")


def drive_approval(session: LivePTY, evidence: Path, label: str, decision: str, screenshots: list[dict[str, Any]], timeout: float = 300, screen: list[str] | None = None) -> dict[str, Any]:
    if decision not in {"allow", "deny"}:
        raise ValueError(f"invalid approval decision: {decision}")
    if screen is None or active_approval_tool(screen) is None:
        _active_name, screen = session.wait_until(
            lambda lines: (tool_name, lines) if (tool_name := active_approval_tool(lines)) and unresolved_tool_call_ids(session.bridge, session.bridge.current_label, tool_name) else None,
            "active approval modal with an unresolved provider tool call",
            timeout,
        )
    tool_name = active_approval_tool(screen)
    if tool_name is None:
        raise AssertionError(f"approval decision screen lacks an active decision footer: {screen[-1000:]}")
    tool_call_ids = sorted(unresolved_tool_call_ids(session.bridge, session.bridge.current_label, tool_name))
    if not tool_call_ids:
        raise AssertionError(f"active approval for {tool_name} has no unresolved provider tool call")
    screen_text = normalize(screen)
    if "scope: unknown" not in screen_text.lower():
        # The UI may vary the scope wording; continue only after the live modal
        # itself has been positively observed.
        if not re.search(r"\[approval\]\s+\w+", screen_text, re.IGNORECASE):
            raise AssertionError(f"approval modal was not recognized: {screen_text[-1000:]}")
    initial = approval_status(screen)
    pages = [initial] if initial else []
    page_shots = []
    reviewed_rows: set[int] = set()
    page_number = 1
    if initial is None:
        raise AssertionError(f"active approval modal did not expose its detail-page state: {normalize(screen)[-1000:]}")
    while True:
        current_status = approval_status(screen)
        if current_status is None:
            raise AssertionError(f"approval detail-page status disappeared before capture: {normalize(screen)[-1000:]}")
        page_start, page_end, page_total = current_status
        if page_start < 1 or page_end < page_start or page_end > page_total:
            raise AssertionError(f"approval detail-page range is invalid: {current_status}")
        screen = wait_for_stable_approval_frame(
            session,
            current_status,
            require_reviewed_footer=page_end >= page_total,
        )
        current_status = approval_status(screen)
        if current_status is None:
            raise AssertionError("stable approval viewport lost its current detail-page range")
        page_start, page_end, page_total = current_status
        page_assertions = ["[approval]", f"{page_start}-{page_end}/{page_total}"]
        if page_end >= page_total:
            page_assertions.append("[y] once")
        shot = session.capture(evidence, f"{label}-approval-{decision}-page-{page_number}", page_assertions)
        screenshots.append(shot)
        page_shots.append(shot["file"])
        reviewed_rows.update(range(page_start, page_end + 1))
        if page_end >= page_total:
            break
        previous = current_status
        session.send(b"\x1b[6~")
        _next_status, screen = session.wait_until(
            lambda lines: (status if (status := approval_status(lines)) and status[1] > previous[1] else None),
            "approval review to advance to next detail page",
            15,
        )
        pages.append(approval_status(screen))
        page_number += 1
        if len(pages) > 20:
            raise AssertionError("approval detail paging did not reach the final page")
    if len(reviewed_rows) != page_total or reviewed_rows != set(range(1, page_total + 1)):
        missing = sorted(set(range(1, page_total + 1)) - reviewed_rows)
        raise AssertionError(f"approval detail review did not cover every row; missing={missing[:20]} page_total={page_total}")
    footer_unlocked = approval_is_fully_reviewed(screen)
    if not footer_unlocked:
        raise AssertionError("approval reached the final detail page without showing the exact unlocked [y] once footer")
    decision_footer = next((line.strip() for line in reversed(screen) if APPROVAL_ONCE_RE.search(line)), "")
    if not decision_footer:
        decision_footer = next((line.strip() for line in reversed(screen) if APPROVAL_DECISION_RE.search(line)), "")
    session.send(b"y" if decision == "allow" else b"n")
    session.decided_tool_call_ids.update(tool_call_ids)
    if decision == "allow":
        _cleared, screen_after = session.wait_until(
            lambda lines: True if active_approval_tool(lines) is None else None,
            "reviewed approval decision to close the active modal",
            15,
        )
    else:
        screen_after = screen
    return {"decision": decision, "decidedAt": now(), "toolName": tool_name, "toolCallIds": tool_call_ids, "reviewedPages": pages, "reviewedRows": len(reviewed_rows), "detailRows": page_total, "detailRowsFullyCovered": True, "decisionBasis": "unlocked-footer" if footer_unlocked else "complete-captured-detail-coverage", "pageScreenshots": page_shots, "decisionFooter": decision_footer, "screen": normalize(screen)[-700:], "screenAfterDecision": normalize(screen_after)[-700:]}


def terminal_run_checkpoints(session_store: Path | None) -> list[dict[str, Any]]:
    if session_store is None or not session_store.exists():
        return []
    rows: list[dict[str, Any]] = []
    for path in sorted(session_store.rglob("*.jsonl")):
        try:
            with path.open("r", encoding="utf-8") as stream:
                for line in stream:
                    try:
                        row = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    if row.get("type") == "checkpoint" and row.get("checkpoint_kind") == "run_terminal":
                        rows.append({key: row.get(key) for key in ("id", "timestamp", "run_outcome", "session_id", "run_id") if key in row})
        except OSError:
            continue
    return rows


def wait_turn(session: LivePTY, bridge: LiveForwarder, label: str, evidence: Path, approvals: list[dict[str, Any]], screenshots: list[dict[str, Any]], timeout: float = 900, capture_progress: str | None = None) -> dict[str, Any]:
    started = time.monotonic()
    requests_before = max((int(item["sequence"]) for item in bridge.snapshot()["requests"]), default=0)
    baseline_terminal_ids = session.turn_terminal_baseline.copy()
    approvals_for_turn: list[dict[str, Any]] = []
    progress_captured = False
    last_count = requests_before
    quiet_since: float | None = None
    while time.monotonic() - started < timeout:
        session.pump(0.12)
        screen_lines = session.screen_lines()
        screen = normalize(screen_lines)
        approval_tool = active_approval_tool(screen_lines)
        pending_approval_ids = unresolved_tool_call_ids(bridge, label, approval_tool) if approval_tool else set()
        if approval_tool and pending_approval_ids - session.decided_tool_call_ids:
            result = drive_approval(session, evidence, f"{label}-{len(approvals_for_turn)+1}", "allow", screenshots, timeout=min(300, timeout), screen=screen_lines)
            approvals_for_turn.append(result)
            approvals.append({"turn": label, **result})
            quiet_since = None
            continue
        with bridge.lock:
            current_count = bridge.sequence
            inflight = bridge.inflight
            last_complete = bridge.last_completed_at
            has_request_for_turn = any(row["turn"] == label for row in bridge.requests)
        working = active_busy_footer(screen_lines)
        if working and capture_progress and not progress_captured:
            visible_marker = active_busy_marker(screen_lines)
            if visible_marker is None:
                raise AssertionError("busy footer disappeared before progress screenshot")
            screenshots.append(session.capture(evidence, capture_progress, [visible_marker]))
            progress_captured = True
        if current_count != last_count:
            last_count = current_count
            quiet_since = None
        failed_checkpoints = [
            row for row in terminal_run_checkpoints(session.session_store_path)
            if row.get("id") not in baseline_terminal_ids and row.get("run_outcome") in {"error", "failed"}
        ]
        if failed_checkpoints:
            checkpoint = failed_checkpoints[-1]
            screenshots.append(session.capture(evidence, f"{label}-terminal-error"))
            raise RuntimeError(
                f"CLI persisted run_terminal outcome={checkpoint.get('run_outcome')} "
                f"checkpoint={checkpoint.get('id')} during {label}; screen={screen[-1200:]}"
            )
        if has_request_for_turn and inflight == 0 and not working and approval_tool is None and idle_input_prompt(screen_lines):
            if quiet_since is None:
                quiet_since = max(time.monotonic(), last_complete)
            elif time.monotonic() - quiet_since >= 2.5:
                return {"turn": label, "gatewayRequestsBefore": requests_before, "gatewayRequestsAfter": current_count, "approvals": approvals_for_turn, "screenExcerpt": screen[-700:], "elapsedSeconds": round(time.monotonic() - started, 2)}
        else:
            quiet_since = None
        process_status = session.exit_status()
        if process_status is not None:
            screenshots.append(session.capture(evidence, f"{label}-cli-exit"))
            raise RuntimeError(f"interactive process exited during {label} with status {process_status}; screen={screen[-1200:]}")
    raise TimeoutError(f"turn {label} did not become idle in {timeout}s; screen={session.screen()[-1200:]}")


def start_turn(session: LivePTY, bridge: LiveForwarder, label: str, prompt: str, expected_response_markers: list[str] | None = None) -> None:
    session.turn_terminal_baseline = {str(row.get("id")) for row in terminal_run_checkpoints(session.session_store_path) if row.get("id")}
    # A pasted newline would submit multiple REPL commands; preserve the
    # fixture text while sending one human-entered user turn.
    normalized_prompt = normalize(prompt.splitlines())
    expected_input_markers = sorted(set(re.findall(r"\b(?:LIVE_[A-Z0-9_]+|FOLLOWUP_[A-Z0-9_]+)\b", normalized_prompt)))
    bridge.label_turn(label, expected_response_markers, expected_input_markers)
    session.send_line(normalized_prompt)


def type_into_tui(session: LivePTY, text: str, inter_key_wait: float = 0.005) -> None:
    """Send visible human-style text at a pace the TUI event loop can render."""
    for char in text:
        session.send(char.encode("utf-8"))
        session.pump(inter_key_wait)


def wait_file(path: Path, timeout: float = 30) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if path.is_file():
            return
        time.sleep(0.1)
    raise TimeoutError(f"expected tool-created file did not appear: {path.name}")


def freeze_model_helper(workspace: Path, evidence: Path, helper_name: str, stage: str) -> dict[str, Any] | None:
    """Copy a real model-written/edited helper into evidence and hash both copies."""
    source = workspace / helper_name
    if not source.is_file():
        return None
    destination = evidence / f"model-helper-{stage}-{helper_name}.txt"
    shutil.copy2(source, destination)
    return {
        "workspacePath": helper_name,
        "evidenceFile": destination.name,
        "workspaceSha256": sha256_file(source),
        "evidenceSha256": sha256_file(destination),
        "bytes": source.stat().st_size,
        "byteIdentical": sha256_file(source) == sha256_file(destination),
    }


def query_magpie_audit(gateway: str, session_id: str) -> dict[str, Any]:
    base = gateway.rstrip("/")
    query = urllib.parse.urlencode({"session": session_id})
    result: dict[str, Any] = {"session": session_id, "routeUrl": f"{base}/v1/magpie/route?session=<redacted-session>"}
    with urllib.request.urlopen(f"{base}/v1/magpie/route?{query}", timeout=20) as response:
        body = json.loads(response.read())
    rows = []
    candidates = body if isinstance(body, list) else body.get("rows", body.get("routes", [body]))
    for row in candidates:
        if not isinstance(row, dict):
            continue
        route = row.get("route", row)
        if not isinstance(route, dict):
            continue
        projected = {key: route.get(key) for key in ("id", "time", "ms", "asked", "model", "served", "tier_served", "status", "done", "tries") if key in route}
        for key in ("seq", "session"):
            if key in row:
                projected[key] = row.get(key)
        rows.append(projected)
    result["routes"] = rows
    usage_rows_raw: list[dict[str, Any]] = []
    offset = 0
    total: int | None = None
    while True:
        usage_query = urllib.parse.urlencode({"period": "today", "limit": 500, "offset": offset, "q": session_id})
        with urllib.request.urlopen(f"{base}/v1/magpie/usage/requests?{usage_query}", timeout=20) as response:
            usage = json.loads(response.read())
        if total is None and isinstance(usage.get("total"), int):
            total = usage["total"]
        page = [row for row in usage.get("rows", []) if isinstance(row, dict)]
        usage_rows_raw.extend(page)
        offset += len(page)
        if not page or (total is not None and offset >= total) or (total is None and len(page) < 500):
            break
    usage_rows = []
    for row in usage_rows_raw:
        if row.get("session") != session_id:
            continue
        usage_rows.append({key: row.get(key) for key in ("t", "agent", "provider", "model", "status", "req", "response_id", "route_id", "in", "out", "cache_read", "cache_write", "reasoning", "ms", "session") if key in row})
    result["usage"] = usage_rows
    result["usagePagination"] = {"pageSize": 500, "rowsFetched": len(usage_rows_raw), "total": total, "complete": total is None or len(usage_rows_raw) >= total}
    return result


def wait_for_magpie_route_settlement(gateway: str, session_id: str, timeout: float = 20) -> dict[str, Any]:
    """Wait briefly for Magpie's route audit to finalize after a client closes.

    On a canceled stream, the CLI can exit before the gateway has published its
    terminal 499/usage row. A one-shot audit would preserve only `done=false`,
    losing whether the real upstream request later completed or was canceled.
    This polls only the local loopback audit endpoint and stores no credentials.
    """
    base = gateway.rstrip("/")
    query = urllib.parse.urlencode({"session": session_id})
    deadline = time.monotonic() + timeout
    last_route: dict[str, Any] = {}
    last_error: str | None = None
    while True:
        try:
            with urllib.request.urlopen(f"{base}/v1/magpie/route?{query}", timeout=5) as response:
                body = json.loads(response.read())
            last_route = redact_route_snapshot(body, 0)
            last_error = None
            if last_route.get("done") is True:
                return {"route": last_route, "settled": True, "waitSeconds": round(timeout - max(0, deadline - time.monotonic()), 3)}
        except Exception as error:
            last_error = type(error).__name__
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            result = {"route": last_route, "settled": False, "waitSeconds": timeout}
            if last_error:
                result["lastError"] = last_error
            return result
        time.sleep(min(0.25, remaining))


def wait_for_bridge_quiescence(bridge: LiveForwarder, timeout: float = 20) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    while True:
        with bridge.lock:
            inflight = bridge.inflight
        if inflight == 0:
            return {"settled": True, "inflightAtSnapshot": 0, "waitSeconds": round(timeout - max(0, deadline - time.monotonic()), 3)}
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return {"settled": False, "inflightAtSnapshot": inflight, "waitSeconds": timeout}
        time.sleep(min(0.1, remaining))


def capture_go_source_snapshot(evidence: Path, phase: str = "at-build") -> dict[str, Any]:
    """Persist tracked Go source diffs around the race build."""
    snapshot: dict[str, Any] = {"capturedAt": now()}
    for label, args in (
        ("unstaged", ["git", "diff", "--binary", "--", "internal", "cmd", "go.mod", "go.sum"]),
        ("staged", ["git", "diff", "--cached", "--binary", "--", "internal", "cmd", "go.mod", "go.sum"]),
    ):
        result = run_rtk(args, REPO, timeout=60)
        if result.returncode != 0:
            snapshot[label] = {"captured": False, "error": result.stderr[-1000:]}
            continue
        path = evidence / f"go-source-diff-{label}-{phase}.patch"
        path.write_text(result.stdout, encoding="utf-8")
        snapshot[label] = {
            "captured": True,
            "file": path.name,
            "bytes": len(result.stdout.encode("utf-8")),
            "sha256": sha256(result.stdout.encode("utf-8")),
        }
    status = run_rtk(["git", "status", "--short"], REPO, timeout=60)
    snapshot["gitStatusShort"] = status.stdout.splitlines() if status.returncode == 0 else []
    return snapshot


def live_inference_rows(snapshot: dict[str, Any], label: str) -> list[dict[str, Any]]:
    return [
        row for row in snapshot.get("requests", [])
        if row.get("turn") == label
        and row.get("method") == "POST"
        and "chat/completions" in row.get("path", "")
        and row.get("requestKind", "interactive-user-turn") == "interactive-user-turn"
    ]


def correlate_provider_evidence(snapshot: dict[str, Any], audit: dict[str, Any]) -> dict[str, Any]:
    """Join independent Magpie audit rows to transparent-forwarder requests.

    Magpie supplies per-session route/usage views while the bridge supplies its
    own sequential IDs and per-turn labels. Join by exact request/response IDs
    when exposed; otherwise use the ordered, session-scoped, one-request-per-
    route sequence. The report records which join was used per inference.
    """
    inference = sorted(
        (row for row in snapshot.get("requests", [])
         if row.get("method") == "POST" and "chat/completions" in row.get("path", "")),
        key=lambda row: row.get("sequence", 0),
    )
    usages = sorted(audit.get("usage", []), key=lambda row: str(row.get("t", "")))
    usage_used: set[int] = set()
    joined = []
    session_id = snapshot.get("sessionId")
    usage_ids_available = any(row.get("response_id") or row.get("route_id") for row in usages)
    for ordinal, request in enumerate(inference):
        route = request.get("routeSnapshot") if isinstance(request.get("routeSnapshot"), dict) else None
        route_id_value = (route or {}).get("id")
        route_id = str(route_id_value) if route_id_value is not None else ""
        request_sequence = request.get("sequence")
        route_session_match = bool(route and route.get("session") == session_id)
        route_sequence_match = bool(route and route.get("captureBridgeSequence") == request_sequence)
        route_identity = {
            "status": "matched" if route_id and route_session_match else "missing-or-mismatch",
            "routeId": route_id_value,
            "sessionMatches": route_session_match,
            "captureAssociation": "matched" if route_sequence_match else "missing-or-mismatch",
            "captureBridgeSequence": route.get("captureBridgeSequence") if route else None,
        }

        usage_index = None
        usage_method = None
        usage_identity: dict[str, Any] = {"status": "missing-or-mismatch", "matchedFields": [], "mismatchedFields": []}
        for candidate_index, candidate in enumerate(usages):
            if candidate_index in usage_used:
                continue
            matched_fields: list[str] = []
            mismatched_fields: list[str] = []
            matched_identity_fields: list[str] = []
            candidate_response_id = candidate.get("response_id")
            request_response_id = request.get("responseId")
            if request_response_id and candidate_response_id:
                if str(request_response_id) == str(candidate_response_id):
                    matched_fields.append("response_id")
                    matched_identity_fields.append("response_id")
                else:
                    mismatched_fields.append("response_id")
            candidate_route_id = candidate.get("route_id")
            if route_id and candidate_route_id:
                if str(route_id) == str(candidate_route_id):
                    matched_fields.append("route_id")
                    matched_identity_fields.append("route_id")
                else:
                    mismatched_fields.append("route_id")
            candidate_session = candidate.get("session")
            if session_id and candidate_session:
                if str(session_id) == str(candidate_session):
                    matched_fields.append("session")
                else:
                    mismatched_fields.append("session")
            if matched_identity_fields and not mismatched_fields:
                usage_index = candidate_index
                usage_method = "exact-" + "+".join(matched_fields)
                usage_identity = {"status": "matched", "matchedFields": matched_fields, "mismatchedFields": []}
                break
        if usage_index is None and not usage_ids_available and ordinal < len(usages):
            # Magpie versions without response/route IDs may be joined by the
            # documented timestamp order, restricted to this unique session.
            # If an ID is exposed but conflicts, never hide that mismatch by
            # falling back to whichever usage row happens to be next.
            candidate = usages[ordinal]
            if ordinal not in usage_used and (not session_id or candidate.get("session") == session_id):
                usage_index = ordinal
                usage_method = "session-order-no-response-or-route-ids"
                usage_identity = {"status": "ordered-fallback-identifiers-unavailable", "matchedFields": ["session-order"], "mismatchedFields": []}
        usage = usages[usage_index] if usage_index is not None and usage_index < len(usages) else None
        upstream_usage = request.get("usage") if isinstance(request.get("usage"), dict) else {}
        usage_reconciliation: dict[str, Any] = {"status": "undefined", "reason": "upstream response or Magpie usage lacks comparable input/output values"}
        if usage is not None:
            upstream_input = upstream_usage.get("prompt_tokens", upstream_usage.get("input_tokens"))
            upstream_output = upstream_usage.get("completion_tokens", upstream_usage.get("output_tokens"))
            gateway_input = usage.get("in")
            gateway_output = usage.get("out")
            if upstream_input is not None and upstream_output is not None and gateway_input is not None and gateway_output is not None:
                cached_input = (usage.get("cache_read") or 0) + (usage.get("cache_write") or 0)
                input_matches = int(upstream_input) == int(gateway_input) + int(cached_input)
                output_matches = int(upstream_output) == int(gateway_output)
                usage_reconciliation = {
                    "status": "matched" if input_matches and output_matches else "mismatch",
                    "upstreamInputTokens": upstream_input,
                    "magpieUncachedInputTokens": gateway_input,
                    "magpieCachedInputTokens": cached_input,
                    "upstreamOutputTokens": upstream_output,
                    "magpieOutputTokens": gateway_output,
                    "inputMatches": input_matches,
                    "outputMatches": output_matches,
                }
        if usage_index is not None:
            usage_used.add(usage_index)
        joined.append({
            "sequence": request.get("sequence"),
            "turn": request.get("turn"),
            "requestKind": request.get("requestKind", "interactive-user-turn"),
            "expectedInputMarkers": request.get("expectedInputMarkers", []),
            "inputMarkerMatches": request.get("inputMarkerMatches", []),
            "askedModel": request.get("askedModel"),
            "responseId": request.get("responseId"),
            "requestId": request.get("requestId"),
            "status": request.get("status"),
            "completedAt": request.get("completedAt"),
            "responseBytes": request.get("responseBytes"),
            "responseSha256": request.get("responseSha256"),
            "responseJsonParsed": request.get("responseJsonParsed"),
            "responseMetadataError": request.get("responseMetadataError"),
            "provider": request.get("provider"),
            "servedModel": request.get("servedModel"),
            "usage": request.get("usage"),
            "stream": request.get("stream"),
            "finishReasons": request.get("finishReasons", []),
            "terminalStreamMarker": request.get("terminalStreamMarker"),
            "toolCalls": [{key: call.get(key) for key in ("id", "name", "argumentsSha256", "safeArguments")} for call in request.get("toolCalls", [])],
            "toolReceipts": [{key: receipt.get(key) for key in ("toolCallId", "tool", "bytes", "sha256", "mentionsDenied", "mentionsError", "mentionsPolicyBlocked")} for receipt in request.get("toolReceipts", [])],
            "route": route,
            "routeJoin": "per-request-session-route-snapshot" if route_identity["status"] == "matched" else None,
            "routeIdentity": route_identity,
            "gatewayUsage": usage,
            "gatewayUsageIndex": usage_index,
            "usageJoin": usage_method if usage else None,
            "usageIdentity": usage_identity,
            "usageReconciliation": usage_reconciliation,
        })
    return {"joinPolicy": "route snapshots must match the unique Magpie session and local bridge request sequence; usage rows must match every exposed response/route/session identity; session-order fallback is allowed only if the usage audit exposes no response_id or route_id", "inference": joined}


def turn_provider_checks(rows: list[dict[str, Any]], provider_rows: list[dict[str, Any]], label: str) -> list[dict[str, Any]]:
    paired = [row for row in provider_rows if row.get("turn") == label and row.get("requestKind", "interactive-user-turn") == "interactive-user-turn"]
    matching_count = len(paired) == len(rows) and bool(rows)
    stream_complete = all(
        bool(row.get("finishReasons"))
        and (row.get("stream") is not True or row.get("terminalStreamMarker") is True)
        for row in rows
    )
    return [
        {"name": f"{label}: every real inference completed HTTP 200 with its own response ID", "passed": bool(rows) and all(row.get("status") == 200 and row.get("completedAt") and row.get("responseId") for row in rows)},
        {"name": f"{label}: complete response JSON metadata was parsed without changing provider bytes", "passed": bool(rows) and all(row.get("responseJsonParsed") is True for row in rows)},
        {"name": f"{label}: every upstream response identifies signed-in Codex and the requested served model", "passed": bool(rows) and all(row.get("provider") == "codex" and str(row.get("askedModel", "")).startswith("codex/") and row.get("servedModel") == row.get("askedModel") for row in rows)},
        {"name": f"{label}: each inference completed with a finish reason and streamed responses have a terminal marker", "passed": bool(rows) and stream_complete},
        {"name": f"{label}: each request joins to this session's exact route and identity-matched nonzero usage", "passed": matching_count and all(row.get("routeIdentity", {}).get("status") == "matched" and row.get("usageIdentity", {}).get("status") in {"matched", "ordered-fallback-identifiers-unavailable"} and (row.get("route") or {}).get("done") is True and (row.get("route") or {}).get("status") == 200 and (row.get("gatewayUsage") or {}).get("provider") == "codex" and ((row.get("gatewayUsage") or {}).get("in") or 0) + ((row.get("gatewayUsage") or {}).get("out") or 0) > 0 for row in paired)},
        {"name": f"{label}: per-request upstream and Magpie token usage reconciles or is explicitly undefined", "passed": matching_count and all(row.get("usageReconciliation", {}).get("status") in {"matched", "undefined"} for row in paired)},
    ]


def trace_compactions(trace_dir: Path) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    events: list[dict[str, Any]] = []
    spans: list[dict[str, Any]] = []
    for path in sorted(trace_dir.glob("*.jsonl")):
        for raw_line in path.read_text(encoding="utf-8", errors="replace").splitlines():
            try:
                row = json.loads(raw_line)
            except json.JSONDecodeError:
                continue
            if row.get("name") == "compaction":
                attrs = row.get("attributes", [])
                attrs = {item.get("key"): item.get("value") for item in attrs if isinstance(item, dict)}
                spans.append({"traceFile": path.name, "traceId": row.get("trace_id"), "spanId": row.get("span_id"), "name": row.get("name"), "startTime": row.get("start_time"), "endTime": row.get("end_time"), "attributes": attrs, "events": [item.get("name") for item in row.get("events", []) if isinstance(item, dict)]})
    return events, spans


def logged_compaction_events(log_path: Path) -> list[dict[str, Any]]:
    if not log_path.is_file():
        return []
    whitelist = {"strategy", "items_in", "items_out", "tokens_in", "tokens_out", "max_tokens", "trace_id", "span_id", "parent_span_id"}
    events = []
    for line in log_path.read_text(encoding="utf-8", errors="replace").splitlines():
        if "compaction.unified.done" not in line:
            continue
        normalized_fields = {}
        for key, quoted, plain in re.findall(r'([A-Za-z_]+)=(?:"([^"]*)"|(\S+))', line):
            if key in whitelist:
                normalized_fields[key] = quoted or plain
        events.append({"event": "compaction.unified.done", "logLineSha256": sha256(line.encode("utf-8")), "fields": normalized_fields})
    return events


def durable_compaction_entries(session_dir: Path, memory_canary: str = "", context_first_marker: str = "", context_last_marker: str = "") -> list[dict[str, Any]]:
    evidence: list[dict[str, Any]] = []
    for path in sorted(session_dir.rglob("*.jsonl")):
        rows = []
        seq_by_id: dict[str, int] = {}
        for raw_line in path.read_text(encoding="utf-8", errors="replace").splitlines():
            try:
                row = json.loads(raw_line)
            except json.JSONDecodeError:
                continue
            rows.append(row)
            if row.get("id") is not None and row.get("seq") is not None:
                seq_by_id[str(row["id"])] = int(row["seq"])
        for row in rows:
            if row.get("type") == "compaction":
                summary = row.get("summary") or row.get("content", "")
                boundary_id = row.get("compacted_through_entry_id")
                evidence.append({
                    "file": path.name,
                    "id": row.get("id"),
                    "parentId": row.get("parent_id"),
                    "seq": row.get("seq"),
                    "type": row.get("type"),
                    "summaryBytes": len(str(summary).encode("utf-8")),
                    "summarySha256": sha256(str(summary).encode("utf-8")),
                    "summaryContainsMemoryCanary": bool(memory_canary and memory_canary in str(summary)),
                    "summaryContainsContextFirstMarker": bool(context_first_marker and context_first_marker in str(summary)),
                    "summaryContainsContextLastMarker": bool(context_last_marker and context_last_marker in str(summary)),
                    "compactedThroughEntryId": boundary_id,
                    "compactedThroughEntrySeq": seq_by_id.get(str(boundary_id)) if boundary_id else None,
                    "runId": row.get("run_id"),
                })
    return evidence


def compaction_evidence(trace_dir: Path, cwd: Path, session_store: Path, memory_canary: str, context_first_marker: str = "", context_last_marker: str = "") -> dict[str, Any]:
    _events, spans = trace_compactions(trace_dir)
    log_candidates = [cwd / ".go-cli" / "logs" / "go-cli.log"]
    logs = next((path for path in log_candidates if path.is_file()), log_candidates[0])
    entries = durable_compaction_entries(session_store, memory_canary, context_first_marker, context_last_marker)
    return {
        "traceSpans": spans,
        "unifiedLogEvents": logged_compaction_events(logs),
        "logFile": str(logs) if logs.is_file() else None,
        "durableEntries": entries,
    }


def completed_requests_before(spans: list[dict[str, Any]], requests: list[dict[str, Any]]) -> int:
    summary_starts = []
    for span in spans:
        if span.get("attributes", {}).get("strategy_used") != "summary" or not span.get("startTime"):
            continue
        try:
            summary_starts.append(datetime.fromisoformat(str(span["startTime"]).replace("Z", "+00:00")))
        except ValueError:
            continue
    if not summary_starts:
        return 0
    start = min(summary_starts)
    count = 0
    for request in requests:
        completed = request.get("completedAt")
        if not completed:
            continue
        try:
            timestamp = datetime.fromisoformat(str(completed).replace("Z", "+00:00"))
        except ValueError:
            continue
        if timestamp <= start:
            count += 1
    return count


def workspace_inventory(cwd: Path) -> list[dict[str, Any]]:
    inventory = []
    for path in sorted(item for item in cwd.rglob("*") if item.is_file()):
        relative = path.relative_to(cwd).as_posix()
        if relative == ".git" or relative.startswith(".git/"):
            continue
        inventory.append({"path": relative, "bytes": path.stat().st_size, "sha256": sha256_file(path)})
    return inventory


def parse_timestamp(value: str) -> datetime | None:
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except (AttributeError, ValueError):
        return None


def tool_registration_evidence(session_store: Path, context_first_marker: str = "", context_last_marker: str = "") -> list[dict[str, Any]]:
    records: dict[str, dict[str, Any]] = {}
    for path in sorted(session_store.rglob("*.jsonl")):
        with path.open("r", encoding="utf-8") as stream:
            for line in stream:
                try:
                    row = json.loads(line)
                except json.JSONDecodeError:
                    continue
                tool_call_id = row.get("tool_call_id")
                if not tool_call_id or row.get("type") != "tool":
                    continue
                record = records.setdefault(tool_call_id, {"toolCallId": tool_call_id})
                if row.get("action_state") == "intent":
                    identity = row.get("tool_identity") or {}
                    record.update({
                        "toolName": row.get("tool_name"),
                        "runId": row.get("run_id"),
                        "intentEntryId": row.get("id"),
                        "intentSeq": row.get("seq"),
                        "registrationSource": identity.get("registration_source"),
                        "scopeFingerprint": identity.get("scope_fingerprint"),
                        "effectSet": row.get("effect_set", []),
                    })
                if row.get("action_state") == "execution":
                    record.update({
                        "executionEntryId": row.get("id"),
                        "executionSeq": row.get("seq"),
                        "dispatchState": row.get("dispatch_state"),
                        "resourceState": row.get("resource_state"),
                        "completionProof": row.get("completion_proof"),
                    })
                if row.get("receipt_outcome"):
                    content = str(row.get("content") or "")
                    content_bytes = content.encode("utf-8")
                    record.update({
                        "receiptOutcome": row.get("receipt_outcome"),
                        "receiptTimestamp": row.get("timestamp"),
                        "receiptEntryId": row.get("id"),
                        "receiptSeq": row.get("seq"),
                        "receiptBytes": len(content_bytes),
                        "receiptSha256": sha256(content_bytes),
                        "receiptContainsContextFirstMarker": bool(context_first_marker and context_first_marker in content),
                        "receiptContainsContextLastMarker": bool(context_last_marker and context_last_marker in content),
                    })
    return [records[key] for key in sorted(records)]


def large_context_read_evidence(
    snapshot: dict[str, Any],
    label: str,
    durable_records: list[dict[str, Any]],
    fixture: dict[str, Any],
) -> dict[str, Any]:
    calls = [
        (row, call)
        for row in live_inference_rows(snapshot, label)
        for call in row.get("toolCalls", [])
        if call.get("name") == "read" and call.get("safeArguments", {}).get("pathBasename") == "large-context.txt"
    ]
    durable_by_call = {str(item.get("toolCallId")): item for item in durable_records if item.get("toolCallId")}
    proofs = []
    for call_row, call in calls:
        durable = durable_by_call.get(str(call.get("id")), {})
        proof = {
            "toolCallId": call.get("id"),
            "providerRequestSequence": call_row.get("sequence"),
            "pathBasename": call.get("safeArguments", {}).get("pathBasename"),
            "receiptEntryId": durable.get("receiptEntryId"),
            "receiptSeq": durable.get("receiptSeq"),
            "receiptOutcome": durable.get("receiptOutcome"),
            "receiptBytes": durable.get("receiptBytes", 0),
            "receiptSha256": durable.get("receiptSha256"),
            "receiptBytesMatchFixture": durable.get("receiptBytes") == fixture.get("bytes"),
            "receiptSha256MatchesFixture": durable.get("receiptSha256") == fixture.get("sha256"),
            "receiptContainsFirstMarker": durable.get("receiptContainsContextFirstMarker", False),
            "receiptContainsLastMarker": durable.get("receiptContainsContextLastMarker", False),
            "resourceState": durable.get("resourceState"),
            "completionProof": durable.get("completionProof"),
            "runId": durable.get("runId"),
        }
        proof["passed"] = (
            durable.get("toolName") == "read"
            and durable.get("receiptOutcome") == "success"
            and durable.get("receiptBytes") == fixture.get("bytes")
            and durable.get("receiptSha256") == fixture.get("sha256")
            and durable.get("receiptBytes", 0) >= 200_000
            and durable.get("receiptContainsContextFirstMarker") is True
            and durable.get("receiptContainsContextLastMarker") is True
            and durable.get("resourceState") == "quiescent"
            and bool(durable.get("completionProof"))
        )
        proofs.append(proof)
    return {
        "fixture": fixture,
        "modelRequestedReadCallIds": [call.get("id") for _, call in calls],
        "durableReceipts": proofs,
        "rawReceiptForwardingClaim": "not asserted; the durable original receipt is hashed separately from later compacted provider requests",
    }


def tool_receipt_ids(snapshot: dict[str, Any], label: str) -> tuple[set[str], set[str]]:
    calls = {call.get("id") for row in live_inference_rows(snapshot, label) for call in row.get("toolCalls", []) if call.get("id")}
    receipts = {receipt.get("toolCallId") for row in live_inference_rows(snapshot, label) for receipt in row.get("toolReceipts", []) if receipt.get("toolCallId")}
    return calls, receipts


def required_tool_completions(
    snapshot: dict[str, Any],
    label: str,
    required_names: set[str],
    durable_records: list[dict[str, Any]],
) -> dict[str, Any]:
    """Require an individual later receipt and durable completion per tool kind."""
    rows = live_inference_rows(snapshot, label)
    durable_by_id = {str(item.get("toolCallId")): item for item in durable_records if item.get("toolCallId")}
    evidence: dict[str, Any] = {}
    for required_name in sorted(required_names):
        candidates = [
            (row, call)
            for row in rows
            for call in row.get("toolCalls", [])
            if str(call.get("name", "")).lower() == required_name and call.get("id")
        ]
        proofs = []
        for call_row, call in candidates:
            call_id = str(call["id"])
            call_sequence = int(call_row.get("sequence", 0))
            receipt_rows = [
                row for row in rows
                if int(row.get("sequence", 0)) > call_sequence
                and any(str(receipt.get("toolCallId", "")) == call_id for receipt in row.get("toolReceipts", []))
            ]
            durable = durable_by_id.get(call_id, {})
            durable_success = (
                durable.get("receiptOutcome") == "success"
                and durable.get("resourceState") == "quiescent"
                and bool(durable.get("completionProof"))
            )
            proofs.append({
                "toolCallId": call_id,
                "callSequence": call_sequence,
                "laterReceiptSequences": [int(row.get("sequence", 0)) for row in receipt_rows],
                "durableReceiptOutcome": durable.get("receiptOutcome"),
                "durableResourceState": durable.get("resourceState"),
                "hasCompletionProof": bool(durable.get("completionProof")),
                "passed": bool(receipt_rows) and durable_success,
            })
        passed = any(item["passed"] for item in proofs)
        evidence[required_name] = {"passed": passed, "calls": proofs}
    return {"passed": all(item["passed"] for item in evidence.values()), "byTool": evidence}


def add_turn_provenance(turn: dict[str, Any], snapshot: dict[str, Any], label: str, artifact_path: Path | None = None) -> None:
    rows = live_inference_rows(snapshot, label)
    calls = [
        {key: call.get(key) for key in ("id", "name", "argumentsSha256", "safeArguments")}
        for row in rows for call in row.get("toolCalls", [])
    ]
    receipts = [
        {key: receipt.get(key) for key in ("toolCallId", "tool", "bytes", "sha256", "mentionsDenied", "mentionsError", "mentionsPolicyBlocked")}
        for row in rows for receipt in row.get("toolReceipts", [])
    ]
    receipt_ids = {receipt.get("toolCallId") for receipt in receipts if receipt.get("toolCallId")}
    turn.update({
        "providerRequestSequences": [row.get("sequence") for row in rows],
        "upstreamResponseIds": [row.get("responseId") for row in rows if row.get("responseId")],
        "toolCalls": calls,
        "toolReceipts": receipts,
        "toolCallReceiptMatches": [{"toolCallId": call.get("id"), "matchedReceipt": call.get("id") in receipt_ids} for call in calls if call.get("id")],
    })
    if artifact_path is not None:
        turn["artifact"] = {"path": str(artifact_path), "exists": artifact_path.is_file(), "sha256": sha256_file(artifact_path) if artifact_path.is_file() else None, "bytes": artifact_path.stat().st_size if artifact_path.is_file() else None}


def proof_checks(snapshot: dict[str, Any], audit: dict[str, Any]) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    correlated = correlate_provider_evidence(snapshot, audit)
    inference = correlated["inference"]
    labels = sorted({
        item.get("label") for item in snapshot.get("turnLabels", [])
        if item.get("label") and item.get("label") != "setup" and item.get("providerExpected", True)
    } | {
        row.get("turn") for row in inference
        if row.get("turn") and row.get("turn") != "setup" and row.get("requestKind", "interactive-user-turn") == "interactive-user-turn"
    })
    internal_rows = [row for row in inference if row.get("requestKind") in {"internal-nonstream", "streamed-unmatched"}]
    streamed_unmatched_rows = [row for row in internal_rows if row.get("requestKind") == "streamed-unmatched"]
    complete_internal = all(
        row.get("status") == 200
        and row.get("completedAt")
        and row.get("responseId")
        and row.get("responseJsonParsed") is True
        and bool(row.get("finishReasons"))
        and (row.get("stream") is not True or row.get("terminalStreamMarker") is True)
        and row.get("provider") == "codex"
        and str(row.get("askedModel", "")).startswith("codex/")
        and row.get("servedModel") == row.get("askedModel")
        and row.get("routeIdentity", {}).get("status") == "matched"
        and row.get("routeIdentity", {}).get("captureAssociation") == "matched"
        and row.get("usageIdentity", {}).get("status") in {"matched", "ordered-fallback-identifiers-unavailable"}
        and (row.get("route") or {}).get("done") is True
        and (row.get("route") or {}).get("status") == 200
        and (row.get("gatewayUsage") or {}).get("provider") == "codex"
        and ((row.get("gatewayUsage") or {}).get("in") or 0) + ((row.get("gatewayUsage") or {}).get("out") or 0) > 0
        and row.get("usageReconciliation", {}).get("status") in {"matched", "undefined"}
        for row in internal_rows
    )
    completed_rows = [row for row in inference if row.get("status") == 200 and row.get("completedAt")]
    response_ids = [row.get("responseId") for row in completed_rows]
    route_ids = [(row.get("route") or {}).get("id") for row in completed_rows]
    usage_identities = [
        ((row.get("gatewayUsage") or {}).get("response_id"), (row.get("gatewayUsage") or {}).get("route_id"), (row.get("gatewayUsage") or {}).get("session"))
        for row in inference
        if row.get("gatewayUsage") and ((row.get("gatewayUsage") or {}).get("response_id") or (row.get("gatewayUsage") or {}).get("route_id"))
    ]
    usage_indices = [row.get("gatewayUsageIndex") for row in inference if row.get("gatewayUsageIndex") is not None]
    checks = [{"name": "live PTY produced actual Chat Completions inference requests", "passed": bool(inference)}]
    checks.append({
        "name": "completed inference responses and Magpie routes have unique IDs",
        "passed": bool(completed_rows) and all(response_ids) and len(set(response_ids)) == len(response_ids) and all(route_ids) and len(set(map(str, route_ids))) == len(route_ids),
    })
    checks.append({
        "name": "Magpie usage joins fail closed on exposed response, route, or session ID mismatches",
        "passed": len(usage_identities) == len(set(map(str, usage_identities))) and len(usage_indices) == len(set(usage_indices)) and all(
            row.get("routeIdentity", {}).get("status") == "matched"
            and row.get("routeIdentity", {}).get("captureAssociation") == "matched"
            and row.get("usageIdentity", {}).get("status") in {"matched", "ordered-fallback-identifiers-unavailable"}
            for row in inference
        ),
        "usageIdentityCount": len(usage_identities),
        "usageIndexCount": len(usage_indices),
        "invalidSequences": [
            row.get("sequence") for row in inference
            if row.get("routeIdentity", {}).get("status") != "matched"
            or row.get("routeIdentity", {}).get("captureAssociation") != "matched"
            or row.get("usageIdentity", {}).get("status") not in {"matched", "ordered-fallback-identifiers-unavailable"}
        ],
    })
    checks.append({
        "name": "internal nonstream or unmatched provider requests are separately classified and each completed with route and usage proof",
        "passed": complete_internal,
        "internalRequestCount": len(internal_rows),
        "internalSequences": [row.get("sequence") for row in internal_rows],
    })
    checks.append({
        "name": "no streamed inference was missing the current user-turn input marker",
        "passed": not streamed_unmatched_rows,
        "unmatchedSequences": [row.get("sequence") for row in streamed_unmatched_rows],
    })
    for label in labels:
        local_rows = live_inference_rows(snapshot, label)
        checks.extend(turn_provider_checks(local_rows, inference, label))
    return checks, correlated


def physical_session_ids(session_store: Path) -> list[str]:
    identifiers: set[str] = set()
    for path in (session_store / "sessions").glob("*.jsonl"):
        try:
            with path.open("r", encoding="utf-8") as stream:
                for line in stream:
                    try:
                        row = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    session_id = row.get("session_id")
                    if isinstance(session_id, str) and session_id:
                        identifiers.add(session_id)
        except OSError:
            continue
    return sorted(identifiers)


def physical_session_file_ids(session_store: Path) -> list[str]:
    """Return IDs with a session-store file, including a newly-created empty file.

    `/session new` announces the physical ID and creates its JSONL file before
    the first user turn. The file has no rows until that turn is durably saved,
    so row-based identity checks belong after the fresh-turn assertion.
    """
    identifiers = set()
    for path in (session_store / "sessions").glob("s_*.jsonl"):
        if path.is_file() and re.fullmatch(r"s_[0-9a-f]{32}", path.stem):
            identifiers.add(path.stem)
    return sorted(identifiers)


def wait_for_physical_session_file(session_store: Path, session_id: str, timeout: float = 5.0) -> list[str]:
    deadline = time.monotonic() + timeout
    while True:
        identifiers = physical_session_file_ids(session_store)
        if session_id in identifiers:
            return identifiers
        if time.monotonic() >= deadline:
            return identifiers
        time.sleep(0.05)


def durable_user_turns(session_store: Path, marker: str) -> list[dict[str, Any]]:
    matches = []
    for path in sorted(session_store.rglob("*.jsonl")):
        try:
            with path.open("r", encoding="utf-8") as stream:
                for line in stream:
                    try:
                        row = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    if row.get("type") == "user" and marker in as_text(row.get("content")):
                        matches.append({
                            "entryId": row.get("id"),
                            "sequence": row.get("seq"),
                            "physicalSessionId": row.get("session_id"),
                            "runId": row.get("run_id"),
                        })
        except OSError:
            continue
    return matches


def run_escape_cancel_recovery(
    session: LivePTY,
    bridge: LiveForwarder,
    cwd: Path,
    session_store: Path,
    nonce: str,
    evidence: Path,
    report: dict[str, Any],
    timeout: float,
) -> None:
    """Finish the full run with a real cancel, explicit new session, and no-tool turn."""
    cancel_started = f"cancel_{nonce.lower()}.started"
    cancel_finished = f"cancel_{nonce.lower()}.finished"
    old_session_ids = physical_session_ids(session_store)
    old_physical_session_id = old_session_ids[-1] if old_session_ids else None
    label = "escape-cancel"
    start_turn(
        session, bridge, label,
        f"Use Bash to create {cancel_started} first, sleep 25 seconds, then create {cancel_finished}. Do not shorten or reorder those operations.",
    )
    cancel_approval = drive_approval(session, evidence, "09-cancel-command", "allow", report["screenshots"], timeout)
    report["approvals"].append({"turn": label, **cancel_approval})
    wait_file(cwd / cancel_started, 60)
    session.pump(0.05)
    active_cancel_lines = session.screen_lines()
    if not (cwd / cancel_started).is_file() or (cwd / cancel_finished).exists():
        raise AssertionError("Escape scenario was not observed inside its real start/finish marker interval")
    active_cancel_marker = active_busy_marker(active_cancel_lines)
    active_cancel_screen = normalize(active_cancel_lines)
    report["escapeRunningObservation"] = {
        "startedExists": (cwd / cancel_started).is_file(),
        "finishedExistsBeforeEscape": (cwd / cancel_finished).exists(),
        "startedAt": datetime.fromtimestamp((cwd / cancel_started).stat().st_mtime).astimezone().isoformat(),
        "activeFooterMarker": active_cancel_marker,
        "screenExcerpt": active_cancel_screen[-1000:],
    }
    report["screenshots"].append(session.capture(evidence, "09-cancel-command-running", [active_cancel_marker] if active_cancel_marker else None))
    cancel_started_at = (cwd / cancel_started).stat().st_mtime
    session.send(b"\x1b")
    try:
        notice_kind, interrupted_lines = session.wait_until(
            lambda lines: (
                "unknown-outcome"
                if current_unknown_cancel_warning(lines) and idle_input_prompt(lines)
                else "legacy-known-cancel"
                if current_legacy_cancel_notice(lines) and idle_input_prompt(lines)
                else None
            ),
            "current unknown-outcome warning and idle prompt after Escape",
            10,
        )
    except TimeoutError:
        notice_kind = None
        interrupted_lines = session.screen_lines()
    unknown_warning_observed = current_unknown_cancel_warning(interrupted_lines) and idle_input_prompt(interrupted_lines)
    legacy_notice_observed = current_legacy_cancel_notice(interrupted_lines) and idle_input_prompt(interrupted_lines)
    warning_screenshot_assertions = (
        [UNKNOWN_CANCEL_WARNING]
        if unknown_warning_observed
        else [LEGACY_CANCEL_NOTICE]
        if legacy_notice_observed
        else None
    )
    report["screenshots"].append(session.capture(evidence, "10-escape-sent", warning_screenshot_assertions))

    # Observe through the original operation's full scheduled delay even if
    # the CLI exits. This detects a late filesystem effect independently of UI.
    cancel_deadline = cancel_started_at + 27
    while time.time() < cancel_deadline:
        remaining = max(0.01, cancel_deadline - time.time())
        if session.exit_status() is None:
            session.pump(min(0.25, remaining))
        else:
            time.sleep(min(0.25, remaining))
    cancel_lines = session.screen_lines()
    report["screenshots"].append(session.capture(evidence, "11-escape-cancelled-stable"))
    unknown_warning_observed = unknown_warning_observed or (
        current_unknown_cancel_warning(cancel_lines) and idle_input_prompt(cancel_lines)
    )
    legacy_notice_observed = legacy_notice_observed or (
        current_legacy_cancel_notice(cancel_lines) and idle_input_prompt(cancel_lines)
    )
    if notice_kind is None and unknown_warning_observed:
        notice_kind = "unknown-outcome"
    elif notice_kind is None and legacy_notice_observed:
        notice_kind = "legacy-known-cancel"
    # The standalone legacy marker describes the known-cancel path and cannot
    # prove this test's intentionally non-idempotent Bash has a durable unknown
    # outcome. Keep it visible as separate diagnostic evidence.
    cancel_observed = unknown_warning_observed

    cancel_snapshot = bridge.snapshot()
    cancel_calls = [
        call for row in live_inference_rows(cancel_snapshot, label)
        for call in row.get("toolCalls", [])
        if call.get("name") == "bash" and call.get("id")
    ]
    cancel_call_ids = {str(call["id"]) for call in cancel_calls}
    cancel_durable = [
        item for item in tool_registration_evidence(session_store)
        if str(item.get("toolCallId")) in cancel_call_ids
    ]
    cancel_run_ids = {str(item.get("runId")) for item in cancel_durable if item.get("runId")}
    all_terminals_before_new = terminal_run_checkpoints(session_store)
    cancel_terminals_before_new = [
        row for row in all_terminals_before_new if str(row.get("run_id")) in cancel_run_ids
    ]
    durable_cancel_call_ids = {
        str(item.get("toolCallId")) for item in cancel_durable if item.get("toolCallId")
    }
    unknown_terminal_run_ids = {
        str(row.get("run_id")) for row in cancel_terminals_before_new
        if row.get("run_outcome") == "unknown" and row.get("run_id")
    }
    missing_durable_cancel_call_ids = sorted(cancel_call_ids - durable_cancel_call_ids)
    missing_unknown_terminal_call_ids = sorted(
        call_id for call_id in cancel_call_ids
        if not any(
            str(item.get("toolCallId")) == call_id
            and str(item.get("runId")) in unknown_terminal_run_ids
            for item in cancel_durable
        )
    )
    old_action_snapshot = [
        {key: item.get(key) for key in (
            "toolCallId", "runId", "intentEntryId", "intentSeq", "executionEntryId", "executionSeq",
            "dispatchState", "resourceState", "completionProof", "receiptOutcome", "receiptEntryId",
            "receiptSeq", "receiptBytes", "receiptSha256",
        )}
        for item in cancel_durable
    ]
    old_terminal_snapshot = [
        {key: row.get(key) for key in ("id", "run_id", "session_id", "run_outcome")}
        for row in cancel_terminals_before_new
    ]
    unknown_before_new = (
        bool(cancel_call_ids)
        and not missing_durable_cancel_call_ids
        and not missing_unknown_terminal_call_ids
        and bool(cancel_terminals_before_new)
        and all(row.get("run_outcome") == "unknown" for row in cancel_terminals_before_new)
    )
    quiescent_before_new = (
        bool(cancel_call_ids)
        and not missing_durable_cancel_call_ids
        and all(
            item.get("resourceState") == "quiescent"
            for item in cancel_durable
            if str(item.get("toolCallId")) in cancel_call_ids
        )
    )
    report["escapeCancellation"] = {
        "turn": label,
        "oldPhysicalSessionId": old_physical_session_id,
        "physicalSessionIdsBeforeNewSession": old_session_ids,
        "startedFileExists": (cwd / cancel_started).is_file(),
        "finishedFileAbsentAfterOriginalDelay": not (cwd / cancel_finished).exists(),
        "interruptedNoticeObserved": cancel_observed,
        "unknownOutcomeWarningObserved": unknown_warning_observed,
        "unknownOutcomeWarningKind": notice_kind,
        "legacyStandaloneInterruptedMarkerObserved": legacy_notice_observed,
        "idlePromptObservedWithWarning": unknown_warning_observed or legacy_notice_observed,
        "activeFooterMarker": active_cancel_marker,
        "toolCallIds": sorted(cancel_call_ids),
        "durableCancelToolCallIds": sorted(durable_cancel_call_ids),
        "callIdsMissingDurableAction": missing_durable_cancel_call_ids,
        "callIdsMissingUnknownTerminal": missing_unknown_terminal_call_ids,
        "durableToolActions": cancel_durable,
        "terminalRunsBeforeNewSession": cancel_terminals_before_new,
        "unknownOutcomeBeforeNewSession": unknown_before_new,
        "quiescentBeforeNewSession": quiescent_before_new,
    }
    report["turns"].append({
        "turn": label,
        "cancelMarkerExists": (cwd / cancel_started).exists(),
        "cancelFinishExists": (cwd / cancel_finished).exists(),
        "interruptedNoticeObserved": cancel_observed,
        "unknownOutcomeWarningObserved": unknown_warning_observed,
        "legacyStandaloneInterruptedMarkerObserved": legacy_notice_observed,
        "activeFooterMarker": active_cancel_marker,
        "activeRunScreenExcerpt": active_cancel_screen[-500:],
    })
    report["checks"].extend([
        {"name": "Escape shows the current unknown-outcome warning at an idle prompt before the delayed marker side effect", "passed": unknown_warning_observed and (cwd / cancel_started).is_file() and not (cwd / cancel_finished).exists()},
        {"name": "cancelled Bash remains an explicit unknown outcome after its resources are quiescent and matches the warning", "passed": unknown_before_new and quiescent_before_new and unknown_warning_observed},
    ])
    if session.exit_status() is not None:
        report["escapeCancellation"]["cliExitedBeforeSessionRecovery"] = True
        report["checks"].append({"name": "CLI remained available for explicit fresh-session recovery after cancellation", "passed": False})
        return
    if not unknown_before_new or not quiescent_before_new or not unknown_warning_observed or not old_physical_session_id:
        report["checks"].append({"name": "fresh session was started only after a quiescent unknown cancellation", "passed": False})
        return

    # A normal text prompt is deliberately attempted first. The durable REPL
    # must explain that it sent no new model request until the operator starts
    # an explicit physical session.
    fenced_marker = f"LIVE_FENCED_AFTER_CANCEL_{nonce.upper()}"
    fence_notice = "The interrupted run is unresolved; no new model request was sent."
    fence_sequence_before = max((int(row.get("sequence", 0)) for row in bridge.snapshot().get("requests", [])), default=0)
    bridge.label_turn("fenced-after-cancel", [], expect_provider=False)
    session.send_line(f"Please answer this ordinary text prompt with only {fenced_marker}.")
    try:
        _fenced_value, fenced_lines = session.wait_until(
            lambda lines: True if current_viewport_notice(lines, fence_notice, "The interrupted run is unresolved;") and idle_input_prompt(lines) else None,
            "current unknown-cancellation fence notice and idle prompt for ordinary text input",
            15,
        )
        fence_notice_seen = current_viewport_notice(fenced_lines, fence_notice, "The interrupted run is unresolved;") and idle_input_prompt(fenced_lines)
    except TimeoutError:
        fenced_lines = session.screen_lines()
        fence_notice_seen = current_viewport_notice(fenced_lines, fence_notice, "The interrupted run is unresolved;") and idle_input_prompt(fenced_lines)
    report["screenshots"].append(session.capture(
        evidence,
        "12-cancel-fenced-prompt-warning",
        ["The interrupted run is unresolved", "no new model request was sent"] if fence_notice_seen else None,
    ))
    fenced_snapshot = bridge.snapshot()
    fenced_requests = [
        row for row in fenced_snapshot.get("requests", [])
        if int(row.get("sequence", 0)) > fence_sequence_before
    ]
    fenced_marker_requests = [
        row for row in fenced_requests if fenced_marker in row.get("userMarkers", [])
    ]
    fenced_durable_users = durable_user_turns(session_store, fenced_marker)
    old_state_after_fence = {
        "actions": [
            {key: item.get(key) for key in (
                "toolCallId", "runId", "intentEntryId", "intentSeq", "executionEntryId", "executionSeq",
                "dispatchState", "resourceState", "completionProof", "receiptOutcome", "receiptEntryId",
                "receiptSeq", "receiptBytes", "receiptSha256",
            )}
            for item in tool_registration_evidence(session_store)
            if str(item.get("toolCallId")) in cancel_call_ids
        ],
        "terminals": [
            {key: row.get(key) for key in ("id", "run_id", "session_id", "run_outcome")}
            for row in terminal_run_checkpoints(session_store)
            if str(row.get("run_id")) in cancel_run_ids
        ],
    }
    expected_old_state = {"actions": old_action_snapshot, "terminals": old_terminal_snapshot}
    report["escapeCancellation"]["fencedPrompt"] = {
        "marker": fenced_marker,
        "noticeSeen": fence_notice_seen,
        "durableUserRows": fenced_durable_users,
        "providerRequestSequencesAfterPrompt": [row.get("sequence") for row in fenced_requests],
        "providerRequestsContainingMarker": [row.get("sequence") for row in fenced_marker_requests],
        "oldUnknownOutcomeUnchanged": old_state_after_fence == expected_old_state,
        "screenExcerpt": normalize(fenced_lines)[-700:],
    }
    report["checks"].extend([
        {"name": "a plain post-cancel prompt shows the exact REPL fence warning", "passed": fence_notice_seen},
        {"name": "fenced prompt created no durable user turn and its marker is absent from all provider request metadata", "passed": not fenced_durable_users and not fenced_marker_requests},
        {"name": "fenced prompt did not alter the old unknown receipt or terminal record", "passed": old_state_after_fence == expected_old_state},
    ])
    if not fence_notice_seen or fenced_durable_users or fenced_marker_requests or old_state_after_fence != expected_old_state:
        return

    bridge.label_turn("session-new-recovery", [], expect_provider=False)
    session.send_line("/session new")
    try:
        new_physical_session_id, new_session_lines = session.wait_until(
            lambda lines: match.group(1)
            if (match := re.search(r"Started fresh durable session (s_[0-9a-f]+)", normalize(lines)))
            else None,
            "explicit /session new response and new physical session id",
            30,
        )
    except TimeoutError:
        report["escapeCancellation"]["newSessionCommandScreen"] = session.screen()[-1000:]
        report["checks"].append({"name": "explicit /session new started a different physical session", "passed": False})
        return
    session_ids_after_new = physical_session_ids(session_store)
    session_file_ids_after_new = wait_for_physical_session_file(session_store, new_physical_session_id)
    report["screenshots"].append(session.capture(evidence, "13-session-new-recovery", ["Started fresh durable session", new_physical_session_id]))
    report["escapeCancellation"].update({
        "newPhysicalSessionId": new_physical_session_id,
        "physicalSessionIdsAfterNewSession": session_ids_after_new,
        "physicalSessionFileIdsAfterNewSession": session_file_ids_after_new,
        "newSessionFileAvailableAfterAck": new_physical_session_id in session_file_ids_after_new,
        "physicalSessionChanged": new_physical_session_id != old_physical_session_id,
        "newSessionOutputExcerpt": normalize(new_session_lines)[-700:],
    })
    old_state_after_new = {
        "actions": [
            {key: item.get(key) for key in (
                "toolCallId", "runId", "intentEntryId", "intentSeq", "executionEntryId", "executionSeq",
                "dispatchState", "resourceState", "completionProof", "receiptOutcome", "receiptEntryId",
                "receiptSeq", "receiptBytes", "receiptSha256",
            )}
            for item in tool_registration_evidence(session_store)
            if str(item.get("toolCallId")) in cancel_call_ids
        ],
        "terminals": [
            {key: row.get(key) for key in ("id", "run_id", "session_id", "run_outcome")}
            for row in terminal_run_checkpoints(session_store)
            if str(row.get("run_id")) in cancel_run_ids
        ],
    }
    expected_old_state = {"actions": old_action_snapshot, "terminals": old_terminal_snapshot}
    report["escapeCancellation"]["oldUnknownOutcomeUnchangedByNewSession"] = old_state_after_new == expected_old_state
    report["checks"].extend([
        {"name": "explicit /session new selected a different physical session", "passed": new_physical_session_id != old_physical_session_id and new_physical_session_id in session_file_ids_after_new},
        {"name": "old cancelled tool receipt and unknown terminal outcome remain unchanged after /session new", "passed": old_state_after_new == expected_old_state},
    ])
    if new_physical_session_id == old_physical_session_id or new_physical_session_id not in session_file_ids_after_new:
        return

    label = "after-cancel-fresh-prompt"
    fresh_answer = f"FRESH_{nonce.upper()}"
    start_turn(session, bridge, label, f"Fresh prompt in the new physical session. Reply with exactly {fresh_answer}; do not use tools.", [fresh_answer])
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
    fresh_snapshot = bridge.snapshot()
    fresh_requests = live_inference_rows(fresh_snapshot, label)
    add_turn_provenance(report["turns"][-1], fresh_snapshot, label)
    durable_fresh_user_rows = durable_user_turns(session_store, fresh_answer)
    all_fenced_marker_requests = [
        row for row in fresh_snapshot.get("requests", [])
        if fenced_marker in row.get("userMarkers", [])
    ]
    all_fenced_durable_users = durable_user_turns(session_store, fenced_marker)
    report["escapeCancellation"]["fencedPrompt"]["allSessionProviderRequestsContainingMarker"] = [
        row.get("sequence") for row in all_fenced_marker_requests
    ]
    report["escapeCancellation"]["fencedPrompt"]["durableUserRowsAfterRecovery"] = all_fenced_durable_users
    fresh_terminal_runs = [
        checkpoint
        for user_row in durable_fresh_user_rows
        for checkpoint in terminal_run_checkpoints(session_store)
        if checkpoint.get("run_id") == user_row.get("runId")
    ]
    report["escapeCancellation"]["freshTurn"] = {
        "turn": label,
        "durableUserRows": durable_fresh_user_rows,
        "terminalRuns": fresh_terminal_runs,
        "providerResponseSeen": any(row.get("responseMarkerMatches", {}).get(fresh_answer) for row in fresh_requests),
        "toolCallCount": sum(len(row.get("toolCalls", [])) for row in fresh_requests),
        "workspaceMutationAttemptedAfterCancellation": False,
    }
    old_state_after_fresh = {
        "actions": [
            {key: item.get(key) for key in (
                "toolCallId", "runId", "intentEntryId", "intentSeq", "executionEntryId", "executionSeq",
                "dispatchState", "resourceState", "completionProof", "receiptOutcome", "receiptEntryId",
                "receiptSeq", "receiptBytes", "receiptSha256",
            )}
            for item in tool_registration_evidence(session_store)
            if str(item.get("toolCallId")) in cancel_call_ids
        ],
        "terminals": [
            {key: row.get(key) for key in ("id", "run_id", "session_id", "run_outcome")}
            for row in terminal_run_checkpoints(session_store)
            if str(row.get("run_id")) in cancel_run_ids
        ],
    }
    report["escapeCancellation"]["oldUnknownOutcomeUnchangedAfterFreshTurn"] = old_state_after_fresh == expected_old_state
    fresh_turn_in_new_session = bool(durable_fresh_user_rows) and all(
        row.get("physicalSessionId") == new_physical_session_id for row in durable_fresh_user_rows
    )
    report["checks"].extend([
        {"name": "ordinary no-tool provider turn succeeded in the fresh physical session", "passed": report["escapeCancellation"]["freshTurn"]["providerResponseSeen"] and report["escapeCancellation"]["freshTurn"]["toolCallCount"] == 0 and fresh_turn_in_new_session and any(row.get("run_outcome") == "success" for row in fresh_terminal_runs)},
        {"name": "old cancelled tool receipt and unknown terminal outcome remain unchanged after fresh-session use", "passed": old_state_after_fresh == expected_old_state},
        {"name": "fenced prompt marker never appears in any later provider request or durable user turn", "passed": not all_fenced_marker_requests and not all_fenced_durable_users},
    ])
    report["screenshots"].append(session.capture(evidence, "14-after-cancel-fresh-session", [fresh_answer]))


def run_live(
    gateway: str,
    model: str,
    run_dir: Path,
    evidence: Path,
    max_tokens: int,
    timeout: float,
    smoke_only: bool = False,
    scoped_workspace: bool = False,
    artifact_focus: bool = False,
    model_timeout_total: str = "10m",
    model_timeout_stream_idle: str = "4m",
) -> dict[str, Any]:
    home = run_dir / "home"
    cwd = run_dir / "workspace"
    trace_dir = run_dir / "traces"
    session_store = run_dir / "session-store"
    home.mkdir(parents=True, exist_ok=True)
    cwd.mkdir(parents=True, exist_ok=True)
    evidence.mkdir(parents=True, exist_ok=True)
    trace_dir.mkdir(parents=True, exist_ok=True)
    nonce = uuid.uuid4().hex[:12]
    nonce_upper = nonce.upper()
    memory_fact = f"LIVE_MEMORY_{nonce_upper}=cobalt"
    context_first_marker = f"LIVE_CONTEXT_FIRST_{nonce_upper}=start"
    context_last_marker = f"LIVE_CONTEXT_LAST_{nonce_upper}=end"
    context_repeated_rows = 3800
    session_id = f"go-cli-live-{time.strftime('%Y%m%d')}-{nonce}"
    fixture_path = evidence / "presentation-fixture.json"
    fixture_result = run_rtk(["python3", "scripts/e2e_live_review/generate_fixture.py", "--nonce", nonce, "--out", str(fixture_path)], REPO, timeout=60)
    if fixture_result.returncode:
        raise RuntimeError(f"nonce-specific PPTX fixture generation failed: {fixture_result.stdout[-1200:]}")
    fixture = json.loads(fixture_path.read_text(encoding="utf-8"))
    expected_artifact = str(fixture["artifact_basename"])
    helper_name = "create_live_deck.go"
    alpha_fact = f"LIVE_FACT_ALPHA_{nonce_upper}=blue"
    beta_fact = f"LIVE_FACT_BETA_{nonce_upper}=green"
    workspace_git = run_rtk(["git", "init", "--quiet", str(cwd)], REPO, timeout=60)
    if workspace_git.returncode:
        raise RuntimeError(f"isolated workspace Git boundary setup failed: {workspace_git.stdout[-1200:]}")
    config_dir = home / ".go-cli"
    config_dir.mkdir(parents=True, exist_ok=True)
    config_path = config_dir / "config.yaml"
    config_lines = [
        "tracing:", "  enabled: true", "  exporter: jsonl", "  file_path: " + json.dumps(str(trace_dir)), "  redaction_level: redact",
        "session:", "  store_path: " + json.dumps(str(session_store)),
        "small_model:", "  provider: openai", f"  model: {model}",
        "compaction:", "  strategy: unified", f"  max_tokens: {max_tokens}", "",
        "production:", "  model_timeout:", f"    total: {model_timeout_total}", f"    stream_idle: {model_timeout_stream_idle}", "",
    ]
    allowed_paths = [str(cwd)] if scoped_workspace else []
    if scoped_workspace:
        config_lines.extend(["sandbox:", "  allowed_paths:", "    - " + json.dumps(str(cwd)), ""])
    config_text = "\n".join(config_lines)
    config_path.write_text(config_text, encoding="utf-8")
    allowed_paths_hash = sha256(json.dumps(allowed_paths, ensure_ascii=False, separators=(",", ":")).encode("utf-8"))
    os.environ["LIVE_E2E_TRACE_DIR"] = str(trace_dir)
    # This is test input, not generated output. The client submits the request
    # and all product file writes remain tool actions performed by the model.
    (cwd / "scan-a.txt").write_text(f"review fixture\nLIVE_FACT_ALPHA_{nonce_upper}=blue\n", encoding="utf-8")
    (cwd / "nested").mkdir()
    (cwd / "nested" / "scan-b.txt").write_text(f"review fixture\nLIVE_FACT_BETA_{nonce_upper}=green\n", encoding="utf-8")
    context_middle = f"generated context row {nonce}; steady context data for compaction.\n"
    context_payload = f"{context_first_marker}\n" + context_middle * context_repeated_rows + f"{context_last_marker}\n"
    (cwd / "large-context.txt").write_text(context_payload, encoding="utf-8")
    (evidence / "presentation-fixture.sha256").write_text(sha256_file(fixture_path) + "  presentation-fixture.json\n", encoding="utf-8")
    os.environ["LIVE_E2E_EXPECTED_ARTIFACT"] = expected_artifact
    os.environ["LIVE_E2E_EXPECTED_HELPER"] = helper_name
    bridge = LiveForwarder(gateway, session_id, memory_fact, {"alpha": alpha_fact, "beta": beta_fact})
    bridge_thread = threading.Thread(target=bridge.serve_forever, name="live-magpie-forwarder", daemon=True)
    bridge_thread.start()
    report: dict[str, Any] = {"nonce": nonce, "sessionId": session_id, "gateway": gateway, "model": model, "runMode": "live-artifact-focus" if artifact_focus else "live-full", "selectedScenes": ["real provider memory seed", "model-generated PPTX create and separate-turn edit", "rendered OOXML review", "production context compaction and recall"] if artifact_focus else ["real text and read/search/list tools", "human approval denial and allowed write/edit", "default Python Bash policy rejection and ordinary-turn recovery", "long Bash with queued follow-up", "model-generated PPTX create and separate-turn edit", "production context compaction and recall", "Escape cancellation then explicit /session new and fresh text"], "skippedScenes": ["smoke and approvals", "long Bash with queued follow-up", "Escape cancellation and recovery"] if artifact_focus else [], "maxTokens": max_tokens, "modelTimeout": {"total": model_timeout_total, "streamIdle": model_timeout_stream_idle}, "scopedWorkspace": scoped_workspace, "screenshots": [], "turns": [], "approvals": [], "checks": [], "workspace": str(cwd), "workspaceGitBoundary": {"initialized": True, "root": str(cwd), "purpose": "stop the parent live/workspaces/.gitignore from hiding fixture files from the real grep tool"}, "expectedPptx": expected_artifact, "pptxFixture": {"file": fixture_path.name, "sha256": sha256_file(fixture_path), "artifactName": expected_artifact}, "isolatedConfig": {"file": str(config_path), "sha256": sha256_file(config_path), "traceDirectory": str(trace_dir), "sessionStore": str(session_store), "hostEnvironmentAllowlist": True, "modelTimeout": {"total": model_timeout_total, "streamIdle": model_timeout_stream_idle}, "scopeMode": "workspace-whitelisted diagnostic" if scoped_workspace else "default without sandbox.allowed_paths", "sandboxAllowedPaths": allowed_paths, "sandboxAllowedPathsSha256": allowed_paths_hash}}
    context_file = cwd / "large-context.txt"
    report["contextFixture"] = {
        "path": str(context_file),
        "sha256": sha256_file(context_file),
        "bytes": context_file.stat().st_size,
        "repeatedRows": context_repeated_rows,
        "firstMarker": context_first_marker,
        "lastMarker": context_last_marker,
    }
    session: LivePTY | None = None
    try:
        health = gateway_health(gateway, model)
        report["gatewayHealth"] = health
        node = locate_node()
        binary = run_dir / "bin" / "go-cli-race"
        if not binary.is_file():
            raise RuntimeError("race-instrumented binary missing after build")
        session = LivePTY(binary, home, cwd, bridge, node, model, max_tokens)
        session.session_store_path = session_store
        startup = session.wait_for("Interactive session started", timeout=90)
        report["screenshots"].append(session.capture(evidence, "01-connected", ["Interactive session started"]))
        report["startupExcerpt"] = startup[-600:]
        compaction_before = compaction_evidence(trace_dir, cwd, session_store, memory_fact, context_first_marker, context_last_marker)

        label = "plain-live-text"
        text_answer = f"LIVE_TEXT_OK_{nonce_upper}"
        start_turn(session, bridge, label, f"This is a live provider text check. Reply with exactly {text_answer}. Also remember this early fact for a later question after context compaction: {memory_fact}. Do not repeat the memory fact yet and do not use tools.", [text_answer])
        report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
        add_turn_provenance(report["turns"][-1], bridge.snapshot(), label)
        text_rows = live_inference_rows(bridge.snapshot(), label)
        report["checks"].append({"name": "actual model response contains requested exact text marker", "passed": any(row.get("responseMarkerMatches", {}).get(text_answer) for row in text_rows)})
        if artifact_focus:
            report["screenshots"].append(session.capture(evidence, "02-live-memory-seed"))
        else:
            report["screenshots"].append(session.capture(evidence, "02-live-text-answer"))

        if not artifact_focus:
            # Read/search/list scenario: each read is explicit and the grep query
            # is one literal marker so its meaning is independent of regex support.
            label = "inspect-fixtures"
            start_turn(session, bridge, label, f"Use one tool call at a time in this exact order: (1) list the current workspace with ls, (2) search with grep for the single literal pattern LIVE_FACT_ALPHA_{nonce_upper} under '.', (3) read scan-a.txt, (4) read nested/scan-b.txt. Do not use Bash, combine patterns, or write files. After all four tool results, report exactly LIVE_FACT_ALPHA_{nonce_upper}=blue and LIVE_FACT_BETA_{nonce_upper}=green.", [f"LIVE_FACT_ALPHA_{nonce_upper}=blue", f"LIVE_FACT_BETA_{nonce_upper}=green"])
            report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
            inspect_snapshot = bridge.snapshot()
            add_turn_provenance(report["turns"][-1], inspect_snapshot, label)
            report["toolRegistrationEvidence"] = tool_registration_evidence(session_store)
            tool_names = {call.get("name") for row in bridge.snapshot()["requests"] if row.get("turn") == label for call in row.get("toolCalls", [])}
            report["turns"][-1]["observedToolNames"] = sorted(name for name in tool_names if name)
            inspect_requests = [row for row in bridge.snapshot()["requests"] if row.get("turn") == label]
            inspect_receipts = [receipt for row in inspect_requests for receipt in row.get("toolReceipts", [])]
            read_receipts = [receipt for receipt in inspect_receipts if receipt.get("tool") == "read"]
            all_fixture_receipts = [receipt for receipt in inspect_receipts if receipt.get("fixtureFactsMatched")]
            fixture_facts_from_tools = {fact for receipt in all_fixture_receipts for fact in receipt.get("fixtureFactsMatched", [])}
            report["checks"].extend([
                {"name": "actual upstream assistant response includes exact alpha fixture fact", "passed": any(row.get("responseMarkerMatches", {}).get(f"LIVE_FACT_ALPHA_{nonce_upper}=blue") for row in inspect_requests)},
                {"name": "actual upstream assistant response includes exact beta fixture fact", "passed": any(row.get("responseMarkerMatches", {}).get(f"LIVE_FACT_BETA_{nonce_upper}=green") for row in inspect_requests)},
                {"name": "actual inference selected ls, literal grep, and read tools", "passed": {"ls", "grep", "read"}.issubset(tool_names)},
                {"name": "both real read tool results were delivered back to the model", "passed": len(read_receipts) >= 2 and all(receipt.get("bytes", 0) > 0 for receipt in read_receipts)},
                {"name": "read calls target both nonce fixture files", "passed": {"scan-a.txt", "scan-b.txt"}.issubset({call.get("safeArguments", {}).get("pathBasename") for row in inspect_requests for call in row.get("toolCalls", []) if call.get("name") == "read"})},
                {"name": "real tool receipts contain both exact fixture facts", "passed": {"alpha", "beta"}.issubset(fixture_facts_from_tools)},
            ])
            if not scoped_workspace:
                inspection_approvals = report["turns"][-1].get("approvals", [])
                approval_shots = {name for approval in inspection_approvals for name in approval.get("pageScreenshots", [])}
                screenshot_evidence = [shot for shot in report["screenshots"] if shot.get("file") in approval_shots]
                grep_approval = next((approval for approval in inspection_approvals if approval.get("decision") == "allow"), None)
                alpha_pattern = f"LIVE_FACT_ALPHA_{nonce_upper}"
                approvals_visible = (
                    bool(screenshot_evidence)
                    and all((evidence / name).is_file() for name in approval_shots)
                    and any("[approval] grep" in shot.get("screenExcerpt", "") and alpha_pattern in shot.get("screenExcerpt", "") for shot in screenshot_evidence)
                )
                inspected_call_ids = {call.get("id") for row in inspect_requests for call in row.get("toolCalls", []) if call.get("id")}
                registrations = [record for record in report["toolRegistrationEvidence"] if record.get("toolCallId") in inspected_call_ids and record.get("toolName") in {"ls", "grep", "read"}]
                grep_call_ids = {call.get("id") for row in inspect_requests for call in row.get("toolCalls", []) if call.get("name") == "grep" and call.get("id")}
                grep_registrations = [record for record in registrations if record.get("toolName") == "grep" and record.get("toolCallId") in grep_call_ids]
                canonical_registrations = [record for record in registrations if record.get("toolName") in {"ls", "read"}]
                grep_zero_scope = bool(grep_registrations) and all(not record.get("scopeFingerprint") or set(record["scopeFingerprint"]) <= {"0"} for record in grep_registrations)
                grep_unknown_effect = bool(grep_registrations) and all(record.get("effectSet") == ["unknown"] for record in grep_registrations)
                canonical_scope_fingerprints = {record.get("scopeFingerprint") for record in canonical_registrations}
                canonical_read_scope = (
                    bool(canonical_registrations)
                    and len(canonical_scope_fingerprints) == 1
                    and all(record.get("registrationSource") == "builtin_tool" and record.get("effectSet") == ["workspace_read"] for record in canonical_registrations)
                )
                grep_receipt_ids = {
                    receipt.get("toolCallId") for receipt in inspect_receipts
                    if receipt.get("tool") == "grep" and receipt.get("bytes", 0) > 0 and "alpha" in receipt.get("fixtureFactsMatched", [])
                }
                grep_receipt_records = [record for record in grep_registrations if record.get("toolCallId") in grep_receipt_ids and record.get("receiptOutcome") == "success"]
                approval_time = parse_timestamp(grep_approval.get("decidedAt")) if grep_approval else None
                grep_receipt_after_approval = bool(approval_time) and any(
                    (receipt_time := parse_timestamp(record.get("receiptTimestamp"))) is not None and receipt_time >= approval_time
                    for record in grep_receipt_records
                )
                report["checks"].extend([
                    {"name": "default unscoped grep retains zero scope and unknown effect while ls/read retain canonical workspace scope", "passed": grep_zero_scope and grep_unknown_effect and all(record.get("resourceState") == "quiescent" and bool(record.get("completionProof")) for record in grep_registrations) and canonical_read_scope},
                    {"name": "default unscoped grep approval screenshot precedes a successful exact-fact grep receipt and read receipts", "passed": approvals_visible and grep_receipt_after_approval and bool(grep_receipt_records) and all(receipt.get("bytes", 0) > 0 for receipt in read_receipts)},
                ])
            report["screenshots"].append(session.capture(evidence, "02-live-tools-answer"))

            # Human-style denial is observed in the real approval modal. The file
            # must be absent both before and after a deny receipt reaches the model.
            denied_path = f"blocked_{nonce.lower()}.txt"
            denied_absent_before = not (cwd / denied_path).exists()
            label = "deny-write"
            start_turn(session, bridge, label, f"Use the write tool to create {denied_path} containing exactly BLOCKED_{nonce}. This is a deliberate approval test; request the tool action normally.")
            denial = drive_approval(session, evidence, "03-deny", "deny", report["screenshots"], timeout)
            report["approvals"].append({"turn": label, **denial})
            report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
            add_turn_provenance(report["turns"][-1], bridge.snapshot(), label)
            denial_exists = (cwd / denied_path).exists()
            deny_requests = [row for row in bridge.snapshot()["requests"] if row.get("turn") == label]
            denied_receipts = [receipt for row in deny_requests for receipt in row.get("toolReceipts", []) if receipt.get("mentionsDenied")]
            report["checks"].extend([
                {"name": "denied write target was absent before and remains absent after human denial", "passed": denied_absent_before and denial["decision"] == "deny" and not denial_exists},
                {"name": "upstream request after denial carries a denied tool receipt", "passed": bool(denied_receipts)},
            ])

            # This is distinct from the human approval denial above: request
            # one exact Bash command that the default interpreter blacklist
            # must reject before process start. If the normal UI asks for
            # approval, complete its full review and approve once; the command
            # policy must still prevent the process from spawning.
            policy_marker_name = f"python_policy_{nonce.lower()}.txt"
            policy_marker_path = cwd / policy_marker_name
            policy_marker_value = f"POLICY_MUST_NOT_RUN_{nonce_upper}"
            policy_command = f"python3 -c 'from pathlib import Path; Path({json.dumps(str(policy_marker_path))}).write_text(\"{policy_marker_value}\")'"
            os.environ["LIVE_E2E_EXPECTED_POLICY_COMMAND"] = policy_command
            policy_marker_absent_before = not policy_marker_path.exists()
            label = "default-policy-reject-python"
            policy_response_marker = f"LIVE_POLICY_REJECTION_{nonce_upper}"
            policy_prompt = (
                "Use exactly one Bash tool call with this command, copied exactly as written, and no other tools:\n"
                f"```sh\n{policy_command}\n```\n"
                "The single attempt is required to test the default command policy. If the normal approval UI appears, "
                "review and approve this exact action once. Do not switch to another executable or interpreter, retry, "
                "or use any alternative. After the tool returns, "
                f"reply with exact marker {policy_response_marker} and one short sentence explaining whether the command "
                "was blocked and whether it wrote the marker file."
            )
            start_turn(session, bridge, label, policy_prompt, [policy_response_marker])
            report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
            policy_snapshot = bridge.snapshot()
            add_turn_provenance(report["turns"][-1], policy_snapshot, label)
            policy_rows = live_inference_rows(policy_snapshot, label)
            policy_all_calls = [
                call for row in policy_rows for call in row.get("toolCalls", [])
            ]
            policy_calls = [
                (row, call)
                for row in policy_rows
                for call in row.get("toolCalls", [])
                if call.get("name") == "bash"
            ]
            policy_call_ids = {str(call.get("id")) for _, call in policy_calls if call.get("id")}
            policy_later_receipts = [
                (row, receipt)
                for row in policy_rows
                for receipt in row.get("toolReceipts", [])
                if str(receipt.get("toolCallId", "")) in policy_call_ids
                and any(int(row.get("sequence", 0)) > int(call_row.get("sequence", 0)) for call_row, call in policy_calls if str(call.get("id")) == str(receipt.get("toolCallId", "")))
            ]
            policy_durable = tool_registration_evidence(session_store)
            policy_durable_by_id = {str(item.get("toolCallId")): item for item in policy_durable if item.get("toolCallId")}
            policy_proofs = []
            for call_row, call in policy_calls:
                call_id = str(call.get("id", ""))
                durable = policy_durable_by_id.get(call_id, {})
                receipts = [receipt for row, receipt in policy_later_receipts if str(receipt.get("toolCallId", "")) == call_id]
                proof = {
                    "toolCallId": call_id,
                    "providerRequestSequence": call_row.get("sequence"),
                    "commandSha256": call.get("argumentsSha256"),
                    "commandMatchesExactExpected": call.get("safeArguments", {}).get("policyCommandMatchesExact") is True,
                    "laterReceiptCount": len(receipts),
                    "laterReceiptIsPolicyBlock": any(receipt.get("mentionsPolicyBlocked") is True and receipt.get("mentionsError") is True for receipt in receipts),
                    "durableReceiptOutcome": durable.get("receiptOutcome"),
                    "resourceState": durable.get("resourceState"),
                    "completionProof": durable.get("completionProof"),
                    "dispatchState": durable.get("dispatchState"),
                    "runId": durable.get("runId"),
                }
                proof["passed"] = (
                    proof["commandMatchesExactExpected"]
                    and proof["laterReceiptCount"] > 0
                    and proof["laterReceiptIsPolicyBlock"]
                    and proof["durableReceiptOutcome"] == "error"
                    and proof["resourceState"] == "quiescent"
                    and bool(proof["completionProof"])
                    and proof["dispatchState"] == "dispatched"
                )
                policy_proofs.append(proof)
            policy_terminal_outcomes = {
                row.get("run_outcome")
                for row in terminal_run_checkpoints(session_store)
                if row.get("run_id") in {proof.get("runId") for proof in policy_proofs}
            }
            policy_later_receipt_sequences = [int(row.get("sequence", 0)) for row, _ in policy_later_receipts]
            policy_latest_receipt_sequence = max(policy_later_receipt_sequences, default=0)
            # The request row that submits the durable error receipt can also
            # contain the model's answer to that receipt, so accept the same
            # sequence as well as any later follow-up inference.
            policy_response_rows_at_or_after_receipt = [
                row for row in policy_rows
                if row.get("requestKind") == "interactive-user-turn"
                and int(row.get("sequence", 0)) >= policy_latest_receipt_sequence
            ]
            policy_response = any(row.get("responseMarkerMatches", {}).get(policy_response_marker) for row in policy_response_rows_at_or_after_receipt)
            policy_explanation = any(row.get("responseExplainsPolicyRejection") for row in policy_response_rows_at_or_after_receipt)
            policy_marker_absent_after = not policy_marker_path.exists()
            policy_approvals = [item for item in report["approvals"] if item.get("turn") == label]
            policy_approval_count = len(policy_approvals)
            policy_approval_safe = all(
                item.get("decision") == "allow"
                and item.get("toolName") == "bash"
                and item.get("detailRowsFullyCovered") is True
                and bool(item.get("pageScreenshots"))
                for item in policy_approvals
            )
            report["policyRejection"] = {
                "turn": label,
                "markerFile": policy_marker_name,
                "markerValueSha256": sha256(policy_marker_value.encode("utf-8")),
                "exactCommandSha256": sha256(policy_command.encode("utf-8")),
                "attemptedBashCallCount": len(policy_calls),
                "allToolCallCount": len(policy_all_calls),
                "durableActionProofs": policy_proofs,
                "terminalRunOutcomes": sorted(policy_terminal_outcomes),
                "approvalCount": policy_approval_count,
                "approvalEvidence": [
                    {key: item.get(key) for key in ("decision", "decidedAt", "toolName", "toolCallIds", "reviewedRows", "detailRows", "detailRowsFullyCovered", "pageScreenshots", "decisionBasis")}
                    for item in policy_approvals
                ],
                "approvalSafe": policy_approval_safe,
                "markerAbsentBefore": policy_marker_absent_before,
                "markerAbsentAfter": policy_marker_absent_after,
                "responseMarkerSeen": policy_response,
                "responseExplainsPolicyRejection": policy_explanation,
                "responseRequestSequencesAtOrAfterErrorReceipt": [row.get("sequence") for row in policy_response_rows_at_or_after_receipt],
            }
            report["checks"].extend([
                {"name": "default policy rejected the sole exact requested Python Bash command with a later matching error receipt and durable quiescent no-process proof", "passed": len(policy_proofs) == 1 and len(policy_calls) == 1 and len(policy_all_calls) == 1 and policy_proofs[0]["passed"]},
                {"name": "any mandatory Bash modal was fully reviewed and approved once; no policy override was applied", "passed": policy_approval_safe},
                {"name": "policy-rejected Python command left its nonce marker absent", "passed": policy_marker_absent_before and policy_marker_absent_after},
                {"name": "model explained the policy rejection after receiving the error receipt", "passed": policy_response and policy_explanation and policy_terminal_outcomes == {"success"}},
            ])

            label = "after-policy-rejection-text"
            policy_recovery_marker = f"LIVE_POLICY_RECOVERY_{nonce_upper}"
            start_turn(session, bridge, label, f"The previous command was rejected. For this independent ordinary turn, do not use any tool. Reply with exactly {policy_recovery_marker}.", [policy_recovery_marker])
            report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
            policy_recovery_snapshot = bridge.snapshot()
            add_turn_provenance(report["turns"][-1], policy_recovery_snapshot, label)
            recovery_rows = live_inference_rows(policy_recovery_snapshot, label)
            recovery_calls = [call for row in recovery_rows for call in row.get("toolCalls", [])]
            policy_marker_absent_after_recovery = not policy_marker_path.exists()
            report["policyRejection"]["recoveryTurn"] = {
                "turn": label,
                "providerResponseSeen": any(row.get("responseMarkerMatches", {}).get(policy_recovery_marker) for row in recovery_rows),
                "toolCallCount": len(recovery_calls),
                "markerStillAbsent": policy_marker_absent_after_recovery,
            }
            report["checks"].extend([
                {"name": "an ordinary no-tool provider turn succeeded after the known policy rejection", "passed": report["policyRejection"]["recoveryTurn"]["providerResponseSeen"] and not recovery_calls},
                {"name": "policy marker remains absent after subsequent ordinary turn", "passed": policy_marker_absent_after_recovery},
            ])

            allowed_path = f"approved_{nonce.lower()}.txt"
            label = "allow-write"
            start_turn(session, bridge, label, f"Now use the write tool to create {allowed_path} containing exactly ALLOWED_{nonce}. Request the approval as usual.")
            allow_info = drive_approval(session, evidence, "04-allow", "allow", report["screenshots"], timeout)
            report["approvals"].append({"turn": label, **allow_info})
            wait_file(cwd / allowed_path, 60)
            report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
            allow_snapshot = bridge.snapshot()
            allow_durable = tool_registration_evidence(session_store)
            allow_completion = required_tool_completions(allow_snapshot, label, {"write"}, allow_durable)
            report["turns"][-1]["requiredToolCompletionEvidence"] = allow_completion
            report["checks"].append({"name": "human-style write approval produced a file and its matching durable completion proof", "passed": (cwd / allowed_path).read_text(encoding="utf-8") == f"ALLOWED_{nonce}" and allow_completion["passed"]})
            add_turn_provenance(report["turns"][-1], allow_snapshot, label)

            label = "edit-written-file"
            edit_value = f"EDITED_{nonce}"
            start_turn(session, bridge, label, f"Use the edit tool to replace the exact text ALLOWED_{nonce} with {edit_value} in {allowed_path}. Preserve the rest of the file and report the final contents.", [edit_value])
            edit_approval = drive_approval(session, evidence, "04-edit", "allow", report["screenshots"], timeout)
            report["approvals"].append({"turn": label, **edit_approval})
            report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
            edit_snapshot = bridge.snapshot()
            edit_requests = live_inference_rows(edit_snapshot, label)
            edit_durable = tool_registration_evidence(session_store)
            edit_completion = required_tool_completions(edit_snapshot, label, {"edit"}, edit_durable)
            report["turns"][-1]["requiredToolCompletionEvidence"] = edit_completion
            add_turn_provenance(report["turns"][-1], edit_snapshot, label)
            report["checks"].extend([
                {"name": "human-style edit approval applied the exact replacement", "passed": (cwd / allowed_path).read_text(encoding="utf-8") == edit_value and edit_approval.get("decision") == "allow"},
                {"name": "the model's Edit call has a later receipt and durable quiescent completion proof", "passed": edit_completion["passed"]},
                {"name": "actual upstream assistant response reports the edited content", "passed": any(row.get("responseMarkerMatches", {}).get(edit_value) for row in edit_requests)},
            ])

            if smoke_only:
                report["status"] = "live-smoke-only"
                report["smokeOnly"] = True
                report["screenshots"].append(session.capture(evidence, "06-live-smoke-complete"))
                return report

            # True long-running Bash with a real start/done sentinel. Submit a user
            # follow-up while the Bash process is still in the tool executor; verify
            # it is delivered with the completed tool receipt on the next LLM call.
            # Streaming Bash currently has a fixed 30-second default limit, so use
            # a 20-second real command to leave room for teardown and receipt flush.
            long_bash_seconds = 20
            started_name = f"long_{nonce.lower()}.started"
            finished_name = f"long_{nonce.lower()}.finished"
            followup = f"FOLLOWUP_{nonce_upper}_QUEUE_ACK: after the long command finishes, report the contents of {finished_name}."
            label = "long-bash-followup"
            start_turn(session, bridge, label, f"Use Bash for one sequential command: create {started_name} with the word started, sleep {long_bash_seconds} seconds, then create {finished_name} with the word finished. Do not claim completion before the second file exists.")
            first_approval = drive_approval(session, evidence, "05-long-bash", "allow", report["screenshots"], timeout)
            report["approvals"].append({"turn": label, **first_approval})
            wait_file(cwd / started_name, 60)
            try:
                running_marker, running_lines = session.wait_until(
                    lambda lines: active_busy_marker(lines)
                    if (cwd / started_name).is_file() and not (cwd / finished_name).exists() and active_busy_marker(lines)
                    else None,
                    "visible active TUI progress while real Bash start/finish markers prove the command is running",
                    5,
                )
            except TimeoutError:
                # Preserve the actual viewport even if the status indicator is
                # missing; the required spinner check below remains a failure.
                running_lines = session.screen_lines()
                running_marker = active_busy_marker(running_lines)
            if not (cwd / started_name).is_file() or (cwd / finished_name).exists():
                raise AssertionError("long Bash start/finish markers do not prove an in-flight command")
            report["longBashRunningObservation"] = {
                "requestedSleepSeconds": long_bash_seconds,
                "startedExists": (cwd / started_name).is_file(),
                "finishedExistsBeforeFollowUp": (cwd / finished_name).exists(),
                "startedAt": datetime.fromtimestamp((cwd / started_name).stat().st_mtime).astimezone().isoformat(),
                "activeFooterMarker": running_marker,
                "screenExcerpt": normalize(running_lines)[-1000:],
            }
            report["screenshots"].append(session.capture(evidence, "06-long-bash-running", [running_marker] if running_marker else None))
            session.send(b"f")
            _followup_screen, followup_lines = session.wait_until(active_followup_input, "live follow-up input footer and caret", 10)
            followup_entered_while_running = (cwd / started_name).is_file() and not (cwd / finished_name).exists()
            report["screenshots"].append(session.capture(evidence, "06-followup-input-ready", ["followup>", "█"]))
            followup_marker = f"FOLLOWUP_{nonce_upper}_QUEUE_ACK"
            type_into_tui(session, followup)
            typed_screen, typed_lines = session.wait_until(
                lambda lines: normalize(lines)
                if followup_marker in normalize(lines) and active_followup_input(lines)
                else None,
                "complete follow-up marker visible in the still-active TUI input field",
                10,
            )
            report["screenshots"].append(session.capture(evidence, "06-followup-input-filled", ["followup>", followup_marker]))
            session.send(b"\r")
            queued_screen, queued_lines = session.wait_until(
                lambda lines: normalize(lines)
                if not active_followup_input(lines)
                and (cwd / started_name).is_file()
                and not (cwd / finished_name).exists()
                else None,
                "follow-up input mode exits while the approved Bash command remains in flight",
                5,
            )
            followup_submitted_while_running = (cwd / started_name).is_file() and not (cwd / finished_name).exists()
            queued_busy_marker = active_busy_marker(queued_lines)
            report["screenshots"].append(session.capture(evidence, "07-followup-submitted", [queued_busy_marker] if queued_busy_marker else None))
            report["longTaskRunningExcerpt"] = normalize(running_lines)[-500:]
            report["followupInputReadyExcerpt"] = normalize(followup_lines)[-500:]
            report["followupInputFilledExcerpt"] = normalize(typed_lines)[-500:]
            report["followupQueuedExcerpt"] = queued_screen[-500:]
            report["longBashRunningObservation"].update({
                "followupModeEnteredBeforeFinish": followup_entered_while_running,
                "followUpSubmittedBeforeFinish": followup_submitted_while_running,
                "followupInputModeExitedAfterEnter": not active_followup_input(queued_lines),
                "finishedExistsImmediatelyAfterSubmit": (cwd / finished_name).exists(),
                "activeFooterMarkerAfterSubmit": queued_busy_marker,
            })
            wait_file(cwd / finished_name, 90)
            report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
            long_snapshot = bridge.snapshot()
            long_durable = tool_registration_evidence(session_store)
            long_completion = required_tool_completions(long_snapshot, label, {"bash"}, long_durable)
            report["turns"][-1]["requiredToolCompletionEvidence"] = long_completion
            add_turn_provenance(report["turns"][-1], long_snapshot, label)
            # request_metadata records matching markers as boolean labels, not raw
            # text. The exact nonce token is also checked separately below.
            actual_requests = bridge.snapshot()["requests"]
            followup_marker = f"FOLLOWUP_{nonce_upper}_QUEUE_ACK"
            followup_seen = any(followup_marker in row.get("userMarkers", []) for row in actual_requests if row.get("turn") == label)
            long_calls = {call.get("id") for row in actual_requests if row.get("turn") == label for call in row.get("toolCalls", []) if call.get("name") == "bash" and call.get("id")}
            matching_receipt = any(
                followup_marker in row.get("userMarkers", [])
                and any(receipt.get("toolCallId") in long_calls for receipt in row.get("toolReceipts", []))
                for row in actual_requests if row.get("turn") == label
            )
            report["checks"].extend([
                {"name": "real sequential Bash created both start and finish markers", "passed": (cwd / started_name).read_text().strip() == "started" and (cwd / finished_name).read_text().strip() == "finished"},
                {"name": "follow-up mode was entered and submitted while the start marker existed and finish marker was absent", "passed": report["longBashRunningObservation"]["startedExists"] and not report["longBashRunningObservation"]["finishedExistsBeforeFollowUp"] and followup_entered_while_running and followup_submitted_while_running and report["longBashRunningObservation"]["followupInputModeExitedAfterEnter"] and not report["longBashRunningObservation"]["finishedExistsImmediatelyAfterSubmit"]},
                {"name": "live TUI displayed a busy footer while the long Bash command was running", "passed": running_marker is not None or queued_busy_marker is not None},
                {"name": "user follow-up marker reached a later Magpie inference request", "passed": followup_seen},
                {"name": "Bash tool receipt reached a later inference request", "passed": matching_receipt},
                {"name": "long Bash call has an individual later receipt and durable quiescent completion proof", "passed": long_completion["passed"]},
            ])
            report["screenshots"].append(session.capture(evidence, "08-long-bash-followup-complete"))

            # Escape cancellation and explicit recovery are deferred until after
            # PPTX creation/edit and context compaction. The cancelled workspace
            # remains quarantined, so no artifact writes may follow that scene.

        report["nonce"] = nonce
        report["presentationGenerationRuntime"] = "LLM-authored Go standard-library OOXML program executed through the real CLI Bash tool; independent verification and rendering are separate"
        artifact = cwd / expected_artifact
        initial_inventory = workspace_inventory(cwd)
        report["workspaceInventoryBeforePptx"] = initial_inventory
        artifact_absent_before = not artifact.exists()
        report["checks"].append({"name": "nonce-bound external PPTX fixture exists and target artifact is absent before turn 1", "passed": fixture_path.is_file() and artifact_absent_before})

        label = "pptx-create"
        create_prompt = fixture["first_turn_prompt"] + f"\nUse the Write tool to create `{helper_name}`, a concise Go program using only the standard library. Then use the Bash tool to run exactly `go run {helper_name} {expected_artifact}`. The program must use `os.Args[1]` as the target path and create a valid editable OOXML `.pptx` there, with four slides, distinct exact fixture titles, all three exact synthetic facts, and editable text shapes (no raster-only content). The `[Content_Types].xml` override for `/ppt/presentation.xml` MUST use exactly `application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml` (including `.main`); verify that exact MIME string in the completed archive. Include `const oldStatus = \"Release status: INITIAL\"` and `const releaseStatus = \"Release status: INITIAL\" // target: {expected_artifact}`. If the deck does not exist, create it. If it already exists, update only the slide-4 status text from `oldStatus` to `releaseStatus`, preserving every other ZIP package entry and all slide shapes. Verify the completed ZIP and required slide text using Go standard-library code. Do not invoke Python, Python 3, Node, network access, or package installation."
        start_turn(session, bridge, label, create_prompt, [fixture["title"], fixture["initial_status"]])
        report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout, capture_progress="13-pptx-generation-working"))
        create_snapshot = bridge.snapshot()
        add_turn_provenance(report["turns"][-1], create_snapshot, label, artifact)
        create_helper_snapshot = freeze_model_helper(cwd, evidence, helper_name, "after-create")
        report["turns"][-1]["modelToolSource"] = create_helper_snapshot
        create_calls, create_receipts = tool_receipt_ids(create_snapshot, label)
        create_durable = tool_registration_evidence(session_store)
        create_completion = required_tool_completions(create_snapshot, label, {"write", "bash"}, create_durable)
        report["turns"][-1]["requiredToolCompletionEvidence"] = create_completion
        create_turn_calls = [call for row in live_inference_rows(create_snapshot, label) for call in row.get("toolCalls", [])]
        create_tool_names = {call.get("name") for call in create_turn_calls}
        create_artifact_call = any(call.get("safeArguments", {}).get("expectedArtifactMentioned") for call in create_turn_calls)
        create_write_targets_helper = any(
            call.get("name") == "write"
            and call.get("safeArguments", {}).get("relativePath") == helper_name
            and call.get("safeArguments", {}).get("expectedArtifactMentioned")
            for call in create_turn_calls
        )
        create_bash_invokes_helper = any(
            call.get("name") == "bash"
            and call.get("safeArguments", {}).get("expectedHelperExecuted")
            and call.get("safeArguments", {}).get("expectedArtifactCommandArgument")
            for call in create_turn_calls
        )
        wait_file(artifact, 60)
        shutil.copy2(artifact, evidence / "presentation-before.pptx")
        before_hash = sha256_file(evidence / "presentation-before.pptx")
        report["turns"][-1]["artifactSnapshot"] = {"file": "presentation-before.pptx", "sha256": before_hash, "bytes": artifact.stat().st_size, "capturedBeforeSecondUserTurn": True}
        report["turns"][-1]["observedToolNames"] = sorted(name for name in create_tool_names if name)
        report["checks"].extend([
            {"name": "turn 1 Write and Bash calls each have a later receipt and durable quiescent completion proof", "passed": create_completion["passed"]},
            {"name": "turn 1 tool-generated PPTX exists at the fixture path with immutable pre-edit snapshot", "passed": artifact_absent_before and artifact.is_file() and before_hash == sha256_file(artifact)},
            {"name": "turn 1 LLM-issued Write targets the helper that references the exact PPTX and source snapshot is frozen", "passed": create_write_targets_helper and create_helper_snapshot is not None and create_helper_snapshot.get("byteIdentical")},
            {"name": "turn 1 LLM-issued Bash runs the same helper with the exact PPTX path", "passed": create_bash_invokes_helper},
            {"name": "turn 1 LLM-issued tool arguments reference the target PPTX output path", "passed": create_artifact_call},
        ])
        report["screenshots"].append(session.capture(evidence, "14-pptx-created", [expected_artifact]))

        label = "pptx-edit-existing"
        edit_prompt = fixture["second_turn_prompt"] + f"\nIn this separate user turn, use the Edit tool on the same existing helper `{helper_name}` from turn 1. Replace only this line `const releaseStatus = \"Release status: INITIAL\" // target: {expected_artifact}` with `const releaseStatus = \"Release status: REVISED\" // target: {expected_artifact}`; keep `oldStatus`, the target filename comment, and all other helper bytes unchanged. The existing program must open the already-created `{expected_artifact}` and patch only the slide-4 status text in that OOXML archive, preserving every other package entry and slide shape. Then use Bash to run exactly `go run {helper_name} {expected_artifact}`. Do not recreate the presentation or invoke Python, Python 3, Node, network access, or package installation. Verify the exact old/new status and report that all nonce/fact text remains."
        start_turn(session, bridge, label, edit_prompt, [fixture["revised_status"]])
        report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout, capture_progress="15-pptx-edit-working"))
        edit_snapshot = bridge.snapshot()
        add_turn_provenance(report["turns"][-1], edit_snapshot, label, artifact)
        edit_helper_snapshot = freeze_model_helper(cwd, evidence, helper_name, "after-edit")
        report["turns"][-1]["modelToolSource"] = edit_helper_snapshot
        edit_calls, edit_receipts = tool_receipt_ids(edit_snapshot, label)
        edit_durable = tool_registration_evidence(session_store)
        deck_edit_completion = required_tool_completions(edit_snapshot, label, {"edit", "bash"}, edit_durable)
        report["turns"][-1]["requiredToolCompletionEvidence"] = deck_edit_completion
        edit_turn_calls = [call for row in live_inference_rows(edit_snapshot, label) for call in row.get("toolCalls", [])]
        edit_tool_names = {call.get("name") for call in edit_turn_calls}
        edit_targets_same_helper_and_artifact = any(
            call.get("name") == "edit"
            and call.get("safeArguments", {}).get("pathBasename") == helper_name
            and call.get("safeArguments", {}).get("expectedArtifactMentioned")
            for call in edit_turn_calls
        )
        edit_bash_invokes_same_helper_and_artifact = any(
            call.get("name") == "bash"
            and call.get("safeArguments", {}).get("expectedHelperExecuted")
            and call.get("safeArguments", {}).get("expectedArtifactCommandArgument")
            for call in edit_turn_calls
        )
        edit_helper_hash_changed = bool(
            create_helper_snapshot and edit_helper_snapshot
            and create_helper_snapshot.get("workspaceSha256") != edit_helper_snapshot.get("workspaceSha256")
            and edit_helper_snapshot.get("byteIdentical")
        )
        report["turns"][-1]["observedToolNames"] = sorted(name for name in edit_tool_names if name)
        report["checks"].extend([
            {"name": "turn 2 Edit and Bash calls each have a later receipt and durable quiescent completion proof", "passed": deck_edit_completion["passed"]},
            {"name": "turn 2 Edit targets the same helper and its arguments identify the existing PPTX", "passed": edit_targets_same_helper_and_artifact},
            {"name": "turn 2 Bash executes that same helper with the existing PPTX path", "passed": edit_bash_invokes_same_helper_and_artifact},
            {"name": "the real helper source changed between separately submitted create and edit turns", "passed": edit_helper_hash_changed},
            {"name": "PPTX bytes changed after the separately submitted turn 2", "passed": artifact.is_file() and sha256_file(artifact) != before_hash},
            {"name": "turn 2 response includes the revised status marker", "passed": any(row.get("responseMarkerMatches", {}).get(fixture["revised_status"]) for row in live_inference_rows(edit_snapshot, label))},
        ])
        after_snapshot_path = evidence / "presentation-after.pptx"
        shutil.copy2(artifact, after_snapshot_path)
        after_hash = sha256_file(after_snapshot_path)
        workspace_after_hash = sha256_file(artifact)
        report["turns"][-1]["artifactSnapshot"] = {
            "file": after_snapshot_path.name,
            "sha256": after_hash,
            "bytes": after_snapshot_path.stat().st_size,
            "workspaceArtifactSha256": workspace_after_hash,
            "copiedAfterSecondUserTurn": True,
        }
        report["checks"].append({"name": "frozen after-deck evidence is a byte-identical copy of Dot's workspace artifact", "passed": after_hash == workspace_after_hash})
        report["screenshots"].append(session.capture(evidence, "16-pptx-edited", [expected_artifact]))

        verifier_path = evidence / "pptx-review.json"
        verifier_args = [
            "python3", "scripts/e2e_live_review/verify_pptx.py",
            "--fixture", str(fixture_path),
            "--before", str(evidence / "presentation-before.pptx"),
            "--after", str(after_snapshot_path),
            "--out", str(verifier_path),
            "--render-dir", str(evidence / "pptx-rendered"),
            "--soffice", "/Users/yinyuepeng/.cache/codex-runtimes/codex-primary-runtime/dependencies/bin/override/soffice",
            "--pdftoppm", "/Users/yinyuepeng/.cache/codex-runtimes/codex-primary-runtime/dependencies/bin/override/pdftoppm",
        ]
        verifier_result = run_rtk(verifier_args, REPO, timeout=300)
        report["pptxReviewCommandExit"] = verifier_result.returncode
        report["pptxReviewInputs"] = {
            "before": {"file": "presentation-before.pptx", "sha256": sha256_file(evidence / "presentation-before.pptx")},
            "after": {"file": after_snapshot_path.name, "sha256": after_hash},
            "workspaceAfter": {"path": str(artifact), "sha256": workspace_after_hash},
        }
        if verifier_path.is_file():
            pptx_review = json.loads(verifier_path.read_text(encoding="utf-8"))
            report["pptxReview"] = pptx_review
            report["checks"].append({"name": "independent OOXML verifier confirms both deck versions and the one-field edit", "passed": verifier_result.returncode == 0 and pptx_review.get("status") == "pass"})
            rendered_pages = (pptx_review.get("render") or {}).get("pages", [])
            report["pptxHumanVisualReview"] = {"status": "pending", "pages": rendered_pages, "reviewTemplate": "../review/visual-review-template.md"}
        else:
            report["checks"].append({"name": "independent OOXML verifier confirms both deck versions and the one-field edit", "passed": False})
            report["pptxReviewOutput"] = verifier_result.stdout[-3000:]

        label = "large-context-read"
        start_turn(session, bridge, label, f"Use the read tool exactly once to read `large-context.txt`. This generated QA fixture begins with {context_first_marker}, then has {context_repeated_rows} repeated rows, and ends with {context_last_marker}. Verify both boundary markers and count the repeated rows. After the tool returns, include the exact text `{context_first_marker}`, `{context_last_marker}`, and `repeated_rows={context_repeated_rows}` in your answer. Do not use Bash and do not repeat or ask about the separate early memory fact.", [context_first_marker, context_last_marker, f"repeated_rows={context_repeated_rows}"])
        report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout, capture_progress="17-large-context-working"))
        context_snapshot = bridge.snapshot()
        add_turn_provenance(report["turns"][-1], context_snapshot, label)
        durable_context_reads = tool_registration_evidence(session_store, context_first_marker, context_last_marker)
        report["largeContextReadEvidence"] = large_context_read_evidence(context_snapshot, label, durable_context_reads, report["contextFixture"])
        context_read_proofs = report["largeContextReadEvidence"]["durableReceipts"]
        report["checks"].append({
            "name": "context pressure is tied to a model-requested read and its original durable receipt is >=200KB, hash-bound, boundary-marked, quiescent, and successful",
            "passed": len(context_read_proofs) == 1 and context_read_proofs[0]["passed"],
        })
        large_context_answer_rows = live_inference_rows(context_snapshot, label)
        report["checks"].append({
            "name": "actual assistant response verifies the first/last context markers and exact repeated-row count",
            "passed": all(any(row.get("responseMarkerMatches", {}).get(marker) for row in large_context_answer_rows) for marker in (context_first_marker, context_last_marker, f"repeated_rows={context_repeated_rows}")),
        })
        report["screenshots"].append(session.capture(evidence, "18-large-context-read-complete"))

        pressure_steps = []
        for step in range(1, 7):
            label = f"context-pressure-{step}"
            marker = f"LIVE_CONTEXT_PRESSURE_{step}_{nonce_upper}"
            pressure_prefix = f"This is synthetic QA context chunk {step}. Keep the early remembered fact available for one later recall question. Reply with exactly {marker} and do not use tools. "
            pressure_row = f"generated QA row for run {nonce}; steady audit data. "
            target_pressure_bytes = 30_000
            prefix_bytes = len(pressure_prefix.encode("utf-8"))
            row_bytes = len(pressure_row.encode("utf-8"))
            pressure_repeat_count = max(0, (target_pressure_bytes - prefix_bytes + row_bytes - 1) // row_bytes)
            pressure_text = pressure_prefix + (pressure_row * pressure_repeat_count)
            start_turn(session, bridge, label, pressure_text, [marker])
            turn = wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout)
            pressure_snapshot = bridge.snapshot()
            add_turn_provenance(turn, pressure_snapshot, label)
            rows = live_inference_rows(pressure_snapshot, label)
            marker_seen = any(row.get("responseMarkerMatches", {}).get(marker) for row in rows)
            turn["pressureInputBytes"] = len(pressure_text.encode("utf-8"))
            turn["pressureInputSha256"] = sha256(pressure_text.encode("utf-8"))
            turn["pressureGeneratedRows"] = pressure_repeat_count
            pressure_steps.append({"turn": label, "inputBytes": turn["pressureInputBytes"], "targetBytes": target_pressure_bytes, "generatedRows": pressure_repeat_count, "inputSha256": turn["pressureInputSha256"], "responseMarker": marker, "responseMarkerSeen": marker_seen})
            report["turns"].append(turn)
            report["checks"].append({"name": f"real context-pressure user turn {step} received an actual provider response", "passed": marker_seen})
        report["contextPressureTurns"] = pressure_steps
        report["screenshots"].append(session.capture(evidence, "19-context-compaction-boundary"))

        label = "post-compaction-recall"
        latest_prompt_marker = f"LIVE_LATEST_PROMPT_{nonce_upper}"
        recall_prompt = f"Question marker {latest_prompt_marker}: what exact key=value fact did I ask you to remember near the beginning of this live conversation? Reply with only that key=value text. Do not use tools."
        start_turn(session, bridge, label, recall_prompt, [memory_fact])
        report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout, capture_progress="20-recall-working"))
        recall_snapshot = bridge.snapshot()
        add_turn_provenance(report["turns"][-1], recall_snapshot, label)
        recall_rows = live_inference_rows(recall_snapshot, label)
        compaction_after = compaction_evidence(trace_dir, cwd, session_store, memory_fact, context_first_marker, context_last_marker)
        compaction_spans = compaction_after["traceSpans"][len(compaction_before["traceSpans"]):]
        compaction_logs = compaction_after["unifiedLogEvents"][len(compaction_before["unifiedLogEvents"]):]
        compaction_entries = compaction_after["durableEntries"][len(compaction_before["durableEntries"]):]
        summary_span = any(
            span.get("attributes", {}).get("strategy_used") == "summary"
            and span.get("attributes", {}).get("trigger_reason")
            and span.get("attributes", {}).get("current_tokens")
            and span.get("attributes", {}).get("max_tokens")
            and "trying_summary" in span.get("events", [])
            for span in compaction_spans
        )
        canary_durable = any(entry.get("summaryContainsMemoryCanary") for entry in compaction_entries)
        context_receipt_seqs = [
            int(item["receiptSeq"])
            for item in context_read_proofs
            if item.get("passed") and item.get("receiptSeq") is not None
        ]
        context_receipt_seq = max(context_receipt_seqs, default=None)
        source_boundaries_cover_read = [
            entry for entry in compaction_entries
            if context_receipt_seq is not None
            and entry.get("compactedThroughEntrySeq") is not None
            and int(entry["compactedThroughEntrySeq"]) >= context_receipt_seq
        ]
        summary_covers_large_context_read = bool(source_boundaries_cover_read)
        report["largeContextReadEvidence"]["summarySourceBoundaries"] = [
            {"summaryEntryId": entry.get("id"), "summarySeq": entry.get("seq"), "compactedThroughEntryId": entry.get("compactedThroughEntryId"), "compactedThroughEntrySeq": entry.get("compactedThroughEntrySeq"), "coversReadReceipt": True}
            for entry in source_boundaries_cover_read
        ]
        recall_inference_has_memory = any(row.get("memoryCanaryPresent") for row in recall_rows)
        prior_provider_requests = completed_requests_before(compaction_spans, bridge.snapshot().get("requests", []))
        report["contextCompaction"] = {
            "before": {"traceSpanCount": len(compaction_before["traceSpans"]), "unifiedLogCount": len(compaction_before["unifiedLogEvents"]), "durableEntryCount": len(compaction_before["durableEntries"])},
            "after": {"traceSpanCount": len(compaction_after["traceSpans"]), "unifiedLogCount": len(compaction_after["unifiedLogEvents"]), "durableEntryCount": len(compaction_after["durableEntries"])},
            "newTraceSpans": compaction_spans,
            "newUnifiedLogEvents": compaction_logs,
            "newDurableEntries": compaction_entries,
            "largeContextReadReceiptSequence": context_receipt_seq,
            "summarySourceBoundaryCoversLargeContextReadReceipt": summary_covers_large_context_read,
            "completedProviderRequestsBeforeSummary": prior_provider_requests,
            "postCompactionProviderRequestContainsMemoryCanary": recall_inference_has_memory,
            "postCompactionProviderRequestContainsLatestPrompt": any(latest_prompt_marker in row.get("userMarkers", []) for row in recall_rows),
            "recalledMemoryFact": any(row.get("responseMarkerMatches", {}).get(memory_fact) for row in recall_rows),
        }
        report["checks"].extend([
            {"name": "production UnifiedCompactor span records a summary compaction trigger and token budget", "passed": summary_span},
            {"name": "summary compaction follows at least four completed real provider requests", "passed": summary_span and prior_provider_requests >= 4},
            {"name": "production compaction.unified.done log event was emitted", "passed": bool(compaction_logs)},
            {"name": "durable compaction entry retains the early canary", "passed": canary_durable},
            {"name": "durable summary source boundary includes the specific successful large-context read receipt", "passed": summary_covers_large_context_read},
            {"name": "actual post-compaction provider request contains the retained early canary", "passed": recall_inference_has_memory},
            {"name": "actual post-compaction provider request contains the latest user prompt", "passed": report["contextCompaction"]["postCompactionProviderRequestContainsLatestPrompt"]},
            {"name": "actual provider answer recalls the early canary exactly", "passed": report["contextCompaction"]["recalledMemoryFact"]},
        ])
        report["screenshots"].append(session.capture(evidence, "21-post-compaction-recall", [memory_fact]))

        if not artifact_focus:
            run_escape_cancel_recovery(session, bridge, cwd, session_store, nonce, evidence, report, timeout)

        report["workspaceInventoryAfterPptx"] = workspace_inventory(cwd)
        report["status"] = "pending-final-provider-proof"
        report["screenshots"].append(session.capture(evidence, "22-live-complete"))
    except Exception:
        report["status"] = "failed"
        report["failedTurn"] = bridge.current_label
        report["failure"] = traceback.format_exc()
    finally:
        if session is not None:
            transcript = session.save_transcript(evidence / "pty-session.ansi")
            report["ptyTranscript"] = transcript
            report["ptyInputDeliveries"] = list(session.input_deliveries)
            report["raceDetectorClean"] = b"WARNING: DATA RACE" not in session.raw_bytes
            report["cliExitStatus"] = session.close()
            report["checks"].append({"name": "interactive CLI exited successfully", "passed": report["cliExitStatus"] == 0})
            report["checks"].append({"name": "Go race detector reported no DATA RACE in the real PTY session", "passed": report["raceDetectorClean"]})
        report["bridgeSettlement"] = wait_for_bridge_quiescence(bridge)
        report["bridge"] = bridge.snapshot()
        durable_tool_records = tool_registration_evidence(session_store, context_first_marker, context_last_marker)
        report["toolRegistrationEvidence"] = durable_tool_records
        if report.get("failedTurn") == "large-context-read" and "largeContextReadEvidence" not in report:
            report["largeContextReadEvidence"] = large_context_read_evidence(
                report["bridge"], "large-context-read", durable_tool_records, report["contextFixture"]
            )
        if "largeContextReadEvidence" in report and "contextCompaction" not in report:
            try:
                final_compaction = compaction_evidence(
                    trace_dir, cwd, session_store, memory_fact, context_first_marker, context_last_marker
                )
                baseline = compaction_before if "compaction_before" in locals() else {
                    "traceSpans": [], "unifiedLogEvents": [], "durableEntries": []
                }
                new_entries = final_compaction["durableEntries"][len(baseline["durableEntries"]):]
                read_proofs = report["largeContextReadEvidence"].get("durableReceipts", [])
                receipt_sequences = [
                    int(item["receiptSeq"])
                    for item in read_proofs
                    if item.get("passed") and item.get("receiptSeq") is not None
                ]
                read_receipt_seq = max(receipt_sequences, default=None)
                covering_entries = [
                    entry for entry in new_entries
                    if read_receipt_seq is not None
                    and entry.get("compactedThroughEntrySeq") is not None
                    and int(entry["compactedThroughEntrySeq"]) >= read_receipt_seq
                ]
                report["largeContextReadEvidence"]["summarySourceBoundaries"] = [
                    {
                        "summaryEntryId": entry.get("id"),
                        "summarySeq": entry.get("seq"),
                        "compactedThroughEntryId": entry.get("compactedThroughEntryId"),
                        "compactedThroughEntrySeq": entry.get("compactedThroughEntrySeq"),
                        "coversReadReceipt": True,
                    }
                    for entry in covering_entries
                ]
                report["contextCompactionPartial"] = {
                    "newTraceSpans": final_compaction["traceSpans"][len(baseline["traceSpans"]):],
                    "newUnifiedLogEvents": final_compaction["unifiedLogEvents"][len(baseline["unifiedLogEvents"]):],
                    "newDurableEntries": new_entries,
                    "largeContextReadReceiptSequence": read_receipt_seq,
                    "summarySourceBoundaryCoversLargeContextReadReceipt": bool(covering_entries),
                    "scope": "partial failure evidence; no compaction pass inferred",
                }
            except Exception as error:
                report["contextCompactionEvidenceError"] = f"{type(error).__name__}: {error}"
        try:
            report["magpieAudit"] = query_magpie_audit(gateway, session_id)
            if any(row.get("method") == "POST" and "chat/completions" in row.get("path", "") for row in report["bridge"].get("requests", [])):
                report["magpieAudit"]["lastRouteSettlement"] = wait_for_magpie_route_settlement(gateway, session_id)
                # Re-read after the route settles so canceled or slow requests
                # retain the final route status and usage row in the report.
                report["magpieAudit"] = query_magpie_audit(gateway, session_id) | {
                    "lastRouteSettlement": report["magpieAudit"]["lastRouteSettlement"]
                }
        except Exception as error:
            report["magpieAuditError"] = f"{type(error).__name__}: {error}"
        try:
            request_checks, provider_evidence = proof_checks(report["bridge"], report.get("magpieAudit", {}))
            report["checks"].extend(request_checks)
            report["providerEvidence"] = provider_evidence
        except Exception as error:
            report["proofError"] = f"{type(error).__name__}: {error}"
        bridge.shutdown()
        bridge.server_close()
        bridge_thread.join(timeout=5)
        report["source"] = {
            "evidenceClass": "true live LLM end-to-end; real CLI binary, PTY, local forwarding bridge and signed-in Magpie Codex inference",
            "provider": "Codex through Magpie; verified using per-session route/usage audit and upstream headers",
            "model": model,
            "scopeMode": "workspace-whitelisted diagnostic" if scoped_workspace else "default without sandbox.allowed_paths",
            "sandboxAllowedPaths": allowed_paths,
            "sandboxAllowedPathsSha256": allowed_paths_hash,
            "isolatedConfigSha256": sha256_file(config_path),
            "cliCredential": "non-secret local sentinel; never logged",
            "authorizationHeadersCaptured": False,
            "requestBodiesCaptured": False,
            "responseBodiesCaptured": False,
            "screenshotSource": "headless Chromium screenshot of xterm.js fed from real PTY bytes, not native Terminal",
            "ptyInputMethod": "xterm bracketed-paste framing followed by a distinct Enter byte; evidence records hashes and byte counts only",
            "isolatedHome": str(home),
            "isolatedWorkspace": str(cwd),
        }
    if report.get("status") not in {"failed", "live-smoke-only"} and not smoke_only:
        if all(item.get("passed") for item in report["checks"]):
            report["status"] = "live-artifact-focus" if artifact_focus else "passed"
        else:
            report["status"] = "failed"
    return report


def write_reports(evidence: Path, report: dict[str, Any]) -> None:
    evidence.mkdir(parents=True, exist_ok=True)
    (evidence / "run-report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    lines = [
        "# Live Magpie terminal E2E evidence",
        "",
        f"- Status: **{report.get('status', 'failed')}**",
        f"- Model: `{report.get('model')}`",
        f"- Run mode: `{report.get('runMode', 'live-full')}`",
        f"- Magpie session: `{report.get('sessionId')}`",
        f"- Run nonce: `{report.get('nonce')}`",
        f"- Race detector clean: `{report.get('raceDetectorClean')}`",
        "- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.",
        "- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.",
        "- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.",
        "",
        "## Checks",
        "",
    ]
    for check in report.get("checks", []):
        lines.append(f"- {'PASS' if check.get('passed') else 'FAIL'}: {check.get('name')}")
    lines.extend(["", "## Screenshots", ""])
    for screenshot in report.get("screenshots", []):
        terminal = screenshot.get("terminal", {})
        lines.append(f"- [`{screenshot['file']}`]({screenshot['file']}) — {terminal.get('widthPx')}×{terminal.get('heightPx')} px, xterm {terminal.get('cols')}×{terminal.get('rows')}; {screenshot.get('source')}.")
    lines.extend(["", "## Provider proof", "", "Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.", ""])
    if report.get("runMode") == "live-artifact-focus":
        lines.extend(["## Scope", "", "Selected scenes:", ""])
        lines.extend(f"- {scene}" for scene in report.get("selectedScenes", []))
        lines.extend(["", "Skipped scenes:", ""])
        lines.extend(f"- {scene}" for scene in report.get("skippedScenes", []))
        lines.extend(["", "Full-suite acceptance is not claimed for this focused run.", ""])
    if report.get("note"):
        lines.extend(["## Remaining cases", "", report["note"], ""])
    if report.get("failure"):
        lines.extend(["## Failure", "", "```text", report["failure"], "```", ""])
    (evidence / "evidence.md").write_text("\n".join(lines), encoding="utf-8")


def ensure_workspace_module_boundary() -> dict[str, Any]:
    """Keep ignored model-authored .go fixtures outside the product module."""
    path = WORKSPACE_ROOT / "go.mod"
    created = False
    if not path.exists():
        path.write_text("module dot-live-qa-workspaces\n\ngo 1.24.0\n", encoding="utf-8")
        created = True
    return {"path": str(path), "sha256": sha256_file(path), "createdByThisRun": created}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--gateway", default=os.environ.get("MAGPIE_URL", DEFAULT_GATEWAY))
    parser.add_argument("--model", default=MODEL)
    parser.add_argument("--max-tokens", type=int, default=128000, help="local go-cli compaction threshold; leaves room for the >=200KB read before real pressure turns trigger summary")
    parser.add_argument("--timeout", type=float, default=900, help="per-turn timeout seconds")
    parser.add_argument("--model-timeout-total", default="10m", help="isolated production.model_timeout.total; defaults to the approved long-call profile")
    parser.add_argument("--model-timeout-stream-idle", default="4m", help="isolated production.model_timeout.stream_idle; defaults to the approved long-call profile")
    parser.add_argument("--run-dir", type=Path)
    parser.add_argument("--evidence-dir", type=Path)
    parser.add_argument("--reuse-binary", type=Path, help="reuse a previously recorded race binary without rebuilding; focused diagnostics only")
    parser.add_argument("--smoke-only", action="store_true", help="run the real text/read/search/list/deny/allow prefix only")
    parser.add_argument("--scoped-workspace", action="store_true", help="diagnostic only: add the isolated workspace to sandbox.allowed_paths")
    parser.add_argument("--artifact-focus", action="store_true", help="run real memory seed, model-generated PPTX create/edit, and context compaction/recall; intentionally skip the separately tested smoke, long-task, and Escape scenarios")
    args = parser.parse_args()
    node = locate_node()
    ensure_renderer_dependencies(node)
    WORKSPACE_ROOT.mkdir(parents=True, exist_ok=True)
    EVIDENCE_ROOT.mkdir(parents=True, exist_ok=True)
    workspace_module = ensure_workspace_module_boundary()
    run_id = time.strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:8]
    run_dir = args.run_dir.expanduser().resolve() if args.run_dir else WORKSPACE_ROOT / run_id
    evidence = args.evidence_dir.expanduser().resolve() if args.evidence_dir else EVIDENCE_ROOT / run_id
    run_dir.mkdir(parents=True, exist_ok=True)
    evidence.mkdir(parents=True, exist_ok=True)
    report: dict[str, Any] = {"runId": run_id, "startedAt": now(), "revision": None, "raceBuild": None, "failure": None, "workspaceGoModuleBoundary": workspace_module, "requestedModelTimeout": {"total": args.model_timeout_total, "streamIdle": args.model_timeout_stream_idle}}
    try:
        rev = run_rtk(["git", "rev-parse", "HEAD"], REPO)
        report["revision"] = rev.stdout.strip() if rev.returncode == 0 else "unknown"
        if args.reuse_binary:
            report["currentGoSourceSnapshot"] = capture_go_source_snapshot(evidence, "current-source-at-reuse")
            binary, binary_hash, origin = reuse_binary(args.reuse_binary, run_dir)
            report["raceBuild"] = {
                "mode": "reuse-pinned-race-binary-no-build",
                "binary": str(binary),
                "sha256": binary_hash,
                "origin": origin,
                "currentCheckoutRevisionIsBinarySource": False,
            }
        else:
            report["goSourceSnapshot"] = capture_go_source_snapshot(evidence)
            binary, binary_hash = build_binary(run_dir)
            report["raceBuild"] = {"binary": str(binary), "sha256": binary_hash, "flags": "rtk proxy go build -race"}
            source_after_build = capture_go_source_snapshot(evidence, "after-build")
            report["goSourceSnapshotAfterBuild"] = source_after_build
            report["goSourceUnchangedDuringBuild"] = all(
                report["goSourceSnapshot"].get(stage, {}).get("sha256") == source_after_build.get(stage, {}).get("sha256")
                for stage in ("unstaged", "staged")
            )
            if not report["goSourceUnchangedDuringBuild"]:
                raise RuntimeError("tracked Go source diff changed during the race binary build window")
        case_report = run_live(
            args.gateway,
            args.model,
            run_dir,
            evidence,
            args.max_tokens,
            args.timeout,
            args.smoke_only,
            args.scoped_workspace,
            args.artifact_focus,
            args.model_timeout_total,
            args.model_timeout_stream_idle,
        )
        report.update(case_report)
    except Exception:
        report["failure"] = traceback.format_exc()
        report["status"] = "failed"
    report["finishedAt"] = now()
    write_reports(evidence, report)
    print(f"Evidence: {evidence}")
    print(f"Workspace: {run_dir}")
    failed = [item["name"] for item in report.get("checks", []) if not item.get("passed")]
    if report.get("failure") or failed:
        print(f"LIVE E2E FAILED: {report.get('failure') or ', '.join(failed)}", file=sys.stderr)
        return 1
    if report.get("raceDetectorClean") is False:
        print("LIVE E2E FAILED: Go race detector warning found in PTY transcript", file=sys.stderr)
        return 1
    if report.get("runMode") == "live-artifact-focus":
        print(f"LIVE ARTIFACT FOCUS PASSED: {len(report.get('checks', []))} checks; {len(report.get('screenshots', []))} PTY screenshots; full-suite status remains unclaimed")
    else:
        print(f"LIVE E2E PASSED: {len(report.get('checks', []))} checks; {len(report.get('screenshots', []))} PTY screenshots")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
