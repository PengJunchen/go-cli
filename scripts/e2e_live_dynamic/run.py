#!/usr/bin/env python3
"""Browser-accessible real PTYs for operator-driven, live Magpie QA.

No browser automation and no provider fixtures: the old transparent forwarder
is reused, while process ownership and HOME-preserving isolation live here.
"""

from __future__ import annotations

import argparse
import base64
import fcntl
import hashlib
import http.client
import importlib.util
import json
import os
import pty
import secrets
import select
import signal
import socket
import stat
import struct
import subprocess
import sys
import termios
import threading
import time
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
MAX_OUTPUT = 128 * 1024 * 1024
KEYS = {"enter": b"\r", "escape": b"\x1b", "page-down": b"\x1b[6~",
        "page-up": b"\x1b[5~", "up": b"\x1b[A", "down": b"\x1b[B",
        "ctrl-c": b"\x03", "ctrl-d": b"\x04", "y": b"y", "n": b"n"}


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def file_digest(path: Path) -> str:
    result = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            result.update(chunk)
    return result.hexdigest()


def write_json(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("w", encoding="utf-8") as target:
        os.chmod(temporary, 0o600)
        json.dump(value, target, indent=2, ensure_ascii=False)
        target.write("\n")
    temporary.replace(path)


def inside(path: Path, root: Path) -> Path:
    result = path.resolve()
    if result != root and root not in result.parents:
        raise ValueError("all workspace, fixture and storage paths must be inside the task root")
    return result


def metadata(path: Path) -> dict:
    result = {"path": str(path), "exists": path.exists() or path.is_symlink()}
    if result["exists"]:
        details = path.lstat()
        result.update(bytes=details.st_size, mode=oct(stat.S_IMODE(details.st_mode)),
                      symlink=stat.S_ISLNK(details.st_mode))
        if stat.S_ISREG(details.st_mode):
            result["sha256"] = file_digest(path)
    return result


def authority(root: Path) -> tuple[str, Path]:
    workspace_id = "workspace-v1-" + digest(b"go-cli/workspace-quarantine/v1\0" + str(root).encode())
    home = Path(os.environ["HOME"])
    if sys.platform == "darwin":
        directory = home / "Library/Application Support"
    else:
        directory = Path(os.environ.get("XDG_CONFIG_HOME", str(home / ".config")))
    return workspace_id, directory / "go-cli/workspace-quarantine"


def load_legacy():
    spec = importlib.util.spec_from_file_location("go_cli_live_bridge", HERE.parent / "e2e_live/run.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def own_trust_entry(workspace: Path) -> dict:
    # Project only this task's key; never emit the shared store or other entries.
    path = Path(os.environ["HOME"]) / ".config/go-cli/trust.json"
    if not path.exists():
        return {"present": False}
    if path.stat().st_size > 16 * 1024 * 1024:
        raise ValueError("trust store exceeds the inspection budget")
    entry = json.loads(path.read_text()).get(str(workspace))
    if not isinstance(entry, dict):
        return {"present": False}
    return {"present": True, "revoked": entry.get("revoked", False),
            "manifest_version": entry.get("manifest_version", 0),
            "trust_generation": entry.get("trust_generation"),
            "manifest_digest": entry.get("manifest_digest"),
            "entry_sha256": digest(json.dumps(entry, sort_keys=True).encode())}


class Session:
    def __init__(self, app, role: str, action: str, cols: int, rows: int):
        self.app, self.role, self.action = app, role, action
        self.id = f"{role}-{action}-{secrets.token_hex(4)}"
        self.workspace = app.scenarios[role]["workspace"]
        self.output_path = app.root / "artifacts" / f"{self.id}.pty"
        self.events_path = app.root / "artifacts" / f"{self.id}.input-metadata.json"
        self.lock, self.events = threading.RLock(), []
        self.bytes, self.sha256, self.truncated = 0, hashlib.sha256(), False
        self.closed = False
        self.cols, self.rows, self.bridge = cols, rows, None
        env = app.child_env()
        args = [str(app.binary)]
        if action == "interactive":
            app.check_storage(role)
            entry = own_trust_entry(self.workspace)
            if not entry.get("present") or entry.get("revoked") or not entry.get("manifest_version"):
                raise ValueError("grant this workspace with the real trust terminal before interactive start")
            session_id = f"go-cli-live-dynamic-{self.id}"
            self.bridge = app.legacy.LiveForwarder(app.gateway, session_id)
            self.bridge.owned_upstreams = set()
            self.bridge.RequestHandlerClass = app.bridge_handler
            self.bridge_thread = threading.Thread(target=self.bridge.serve_forever, daemon=True)
            self.bridge_thread.start()
            env.update(GO_CLI_PROVIDER_BASE_URL=f"http://127.0.0.1:{self.bridge.server_port}/v1",
                       GO_CLI_TRACING_FILE_PATH=str(app.root / "artifacts" / f"{self.id}.traces"))
            approval = app.scenarios[role].get("approval", role if role in ("ask", "auto", "deny") else "ask")
            if approval not in ("ask", "auto", "deny"):
                raise ValueError("approval must be ask, auto or deny")
            args += ["interactive", "--no-onboarding", "--provider", "openai", "--model", app.model,
                     "--thinking", "none", "--approve", approval]
            args += app.scenarios[role].get("args", [])
        elif action == "trust-grant":
            args += ["trust", "grant"]
        elif action == "trust-revoke":
            args += ["trust", "revoke", str(self.workspace)]
        else:
            raise ValueError("unknown start action")
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        spec_path = app.root / "artifacts" / f"{self.id}.exec.json"
        write_json(spec_path, {"args": args, "cwd": str(self.workspace)})
        try:
            self.proc = subprocess.Popen([sys.executable, str(HERE / "run.py"), "--pty-child", str(spec_path)],
                                         stdin=slave, stdout=slave, stderr=slave, env=env,
                                         start_new_session=True, close_fds=True)
        except Exception:
            os.close(self.master)
            if self.bridge:
                self.bridge.shutdown()
                self.bridge.server_close()
            raise
        finally:
            os.close(slave)
        self.reader = threading.Thread(target=self.read_output, daemon=True)
        self.reader.start()

    def read_output(self):
        with self.output_path.open("wb", buffering=0) as target:
            os.chmod(self.output_path, 0o600)
            while True:
                try:
                    chunk = os.read(self.master, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                with self.lock:
                    self.sha256.update(chunk)
                    self.bytes += len(chunk)
                    allowed = max(0, MAX_OUTPUT - target.tell())
                    target.write(chunk[:allowed])
                    self.truncated |= len(chunk) > allowed
        self.proc.wait()
        self.app.save_manifest()

    def send(self, data: bytes, kind: str):
        if self.proc.poll() is not None:
            raise ValueError("PTY process has exited")
        with self.lock:
            self.events.append({"at": time.time(), "kind": kind, "bytes": len(data), "sha256": digest(data)})
            write_json(self.events_path, self.events)
        offset = 0
        while offset < len(data):
            offset += os.write(self.master, data[offset:])

    def resize(self, cols: int, rows: int):
        self.cols, self.rows = cols, rows
        fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        if self.proc.poll() is None:
            os.killpg(self.proc.pid, signal.SIGWINCH)

    def state(self):
        with self.lock:
            return {"id": self.id, "role": self.role, "action": self.action, "pid": self.proc.pid,
                    "exit": self.proc.poll(), "cols": self.cols, "rows": self.rows,
                    "bytes": self.bytes, "sha256": self.sha256.hexdigest(), "captureTruncated": self.truncated,
                    "magpieSession": self.bridge.session_id if self.bridge else None,
                    "inflight": self.bridge.inflight if self.bridge else 0}

    def audit(self):
        if not self.bridge:
            return {"providerExpected": False}
        snapshot = self.bridge.snapshot()
        audit = self.app.legacy.query_magpie_audit(self.app.gateway, self.bridge.session_id)
        evidence = self.app.legacy.correlate_provider_evidence(snapshot, audit)
        result = {"snapshot": snapshot, "audit": audit, "evidence": evidence}
        write_json(self.app.root / "artifacts" / f"{self.id}.provider-evidence.json", result)
        return result

    def close(self):
        with self.lock:
            if self.closed:
                return
            self.closed = True
        # Only signal the process group created by this Popen(start_new_session).
        for signum, wait in ((signal.SIGTERM, 2), (signal.SIGKILL, 2)):
            try:
                os.killpg(self.proc.pid, signum)
            except ProcessLookupError:
                break
            try:
                self.proc.wait(timeout=wait)
            except subprocess.TimeoutExpired:
                continue
            # A child may outlive the leader; the next iteration fences its group.
            if signum == signal.SIGKILL:
                break
            time.sleep(0.1)
        self.reader.join(timeout=2)
        if self.bridge:
            with self.bridge.lock:
                for upstream in list(self.bridge.owned_upstreams):
                    try:
                        upstream.shutdown(socket.SHUT_RDWR)
                    except OSError:
                        pass
            self.bridge.shutdown()
            self.bridge.server_close()
            self.bridge_thread.join(timeout=2)
            write_json(self.app.root / "artifacts" / f"{self.id}.bridge.json", self.bridge.snapshot())
        try:
            os.close(self.master)
        except OSError:
            pass


class App:
    def __init__(self, args):
        self.root = args.root.resolve()
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        for name in ("artifacts", "workspaces", "testtmp"):
            (self.root / name).mkdir(exist_ok=True, mode=0o700)
        self.binary = inside(args.binary, self.root)
        self.assets, self.ready_file = args.assets.resolve(), inside(args.ready_file, self.root)
        self.gateway, self.model = args.gateway.rstrip("/"), args.model
        parsed = urllib.parse.urlsplit(self.gateway)
        if parsed.scheme != "http" or parsed.hostname != "127.0.0.1" or parsed.port != 3425 or parsed.path:
            raise ValueError("gateway must be the existing http://127.0.0.1:3425")
        urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler({})))
        self.legacy = load_legacy()
        legacy = self.legacy

        class GuardedBridge(legacy.LiveForwarderHandler):
            def do_GET(self):
                self.send_error(405)

            def do_POST(self):
                expected = "Bearer " + legacy.SENTINEL_API_KEY
                if not secrets.compare_digest(self.headers.get("Authorization", ""), expected):
                    self.send_error(403)
                    return
                if self.path != "/v1/chat/completions":
                    self.send_error(404)
                    return
                super().do_POST()

            def _forward(self, body):
                # Same legacy capture contract, but retain the upstream socket
                # so downstream cancellation does not wait for another SSE line.
                bridge = self.server
                sequence, _ = bridge.begin(self.command, self.path, body)
                headers = {"X-Magpie-Session": bridge.session_id, "X-Magpie-Agent": "go-cli-live-e2e"}
                for name in ("Authorization", "Content-Type", "Accept", "OpenAI-Organization", "OpenAI-Project"):
                    if self.headers.get(name):
                        headers[name] = self.headers[name]
                connection = http.client.HTTPConnection("127.0.0.1", 3425, timeout=10)
                finished, canceled = threading.Event(), threading.Event()
                upstream_socket, monitor = None, None
                status, response_headers = 502, {}

                def watch_disconnect():
                    while not finished.wait(0.05):
                        try:
                            readable, _, _ = select.select([self.connection], [], [], 0)
                            if readable and self.connection.recv(1, socket.MSG_PEEK | socket.MSG_DONTWAIT) == b"":
                                canceled.set()
                                upstream_socket.shutdown(socket.SHUT_RDWR)
                                return
                        except (OSError, ValueError):
                            return

                try:
                    connection.connect()
                    upstream_socket = connection.sock
                    upstream_socket.settimeout(900)
                    with bridge.lock:
                        bridge.owned_upstreams.add(upstream_socket)
                    monitor = threading.Thread(target=watch_disconnect, daemon=True)
                    monitor.start()
                    connection.request(self.command, self.path, body=body, headers=headers)
                    response = connection.getresponse()
                    status, response_headers = response.status, response.headers
                    content_type = response.headers.get("Content-Type", "application/octet-stream")
                    self.send_response(status)
                    self.send_header("Content-Type", content_type)
                    self.send_header("Connection", "close")
                    for name in ("Cache-Control", "X-Magpie-Provider", "X-Magpie-Model", "X-Request-Id", "Request-Id", "X-Magpie-Request-Id"):
                        value = response.headers.get(name)
                        if value:
                            self.send_header(name, value)
                    self.end_headers()
                    while True:
                        chunk = response.readline(131072) if "text/event-stream" in content_type else response.read(65536)
                        if not chunk:
                            break
                        bridge.response_chunk(sequence, chunk)
                        self.wfile.write(chunk)
                        self.wfile.flush()
                    response.close()
                except (OSError, http.client.HTTPException):
                    with bridge.lock:
                        bridge.events.append({"sequence": sequence, "kind": "client_stream_closed" if canceled.is_set() else "transport_closed"})
                    # No fabricated provider JSON or SSE is written on failure.
                    self.close_connection = True
                finally:
                    finished.set()
                    connection.close()
                    if upstream_socket:
                        with bridge.lock:
                            bridge.owned_upstreams.discard(upstream_socket)
                    if monitor:
                        monitor.join(timeout=0.2)
                    bridge.finish(sequence, status, response_headers)
                    self.close_connection = True

        self.bridge_handler = GuardedBridge
        self.nonce, self.sessions, self.lock = secrets.token_urlsafe(32), {}, threading.RLock()
        raw = json.loads(args.scenario_config.read_text())
        self.scenarios = {}
        for role, scenario in raw["roles"].items():
            if role not in ("ask", "auto", "deny", "dynamic"):
                raise ValueError("unsupported scenario role")
            scenario = dict(scenario)
            scenario["workspace"] = inside(Path(scenario["workspace"]), self.root)
            scenario["workspace"].mkdir(parents=True, exist_ok=True, mode=0o700)
            if not isinstance(scenario.get("args", []), list) or not all(isinstance(x, str) for x in scenario.get("args", [])):
                raise ValueError("scenario args must be a string array")
            self.scenarios[role] = scenario
        if args.prepare_workspaces:
            self.prepare()
        self.original_authorities = {}
        for role, scenario in self.scenarios.items():
            workspace_id, directory = authority(scenario["workspace"])
            originals = list(directory.glob(workspace_id + ".*"))
            self.original_authorities[role] = [str(path) for path in originals]
            if originals or own_trust_entry(scenario["workspace"]).get("present"):
                raise ValueError("fresh harness workspaces must have no preexisting authority or trust key")
        self.save_manifest()

    def prepare(self):
        for role, scenario in self.scenarios.items():
            path = scenario["workspace"] / ".go-cli.json"
            if path.exists():
                config = json.loads(path.read_text())
            elif scenario.get("fixture_config"):
                fixture = inside(Path(scenario["fixture_config"]), self.root)
                config = json.loads(fixture.read_text())
            else:
                config = {}
            storage = self.root / "artifacts" / "stores" / role
            storage.mkdir(parents=True, exist_ok=True, mode=0o700)
            for section, key, value in (("session", "store_path", storage / "sessions"),
                                        ("history", "path", storage / "history.jsonl"),
                                        ("memory", "store_path", storage / "memories.jsonl")):
                config.setdefault(section, {})[key] = str(value)
            audit = config.setdefault("production", {}).setdefault("audit", {})
            if audit.get("enabled") is None:
                audit["enabled"] = True
            audit["path"] = str(storage / "tool-audit.jsonl")
            write_json(path, config)

    def check_storage(self, role):
        workspace = self.scenarios[role]["workspace"]
        if (workspace / ".go-cli.yaml").exists() or (workspace / ".go-cli.yml").exists():
            raise ValueError("a YAML project config would shadow the controlled JSON storage config")
        config = json.loads((workspace / ".go-cli.json").read_text())
        for section, key in (("session", "store_path"), ("history", "path"), ("memory", "store_path")):
            value = config.get(section, {}).get(key)
            if not value or not Path(value).is_absolute():
                raise ValueError(f"explicit {section}.{key} is required")
            inside(Path(value), self.root)
        session = Path(config["session"]["store_path"])
        if session.exists() and not session.is_dir():
            raise ValueError("session.store_path must be a directory")
        audit = config.get("production", {}).get("audit", {})
        # The production loader enables auditing by default. A missing path
        # would therefore write to the real user's default audit store.
        if audit.get("enabled") is not False:
            value = audit.get("path")
            if not value or not Path(value).is_absolute():
                raise ValueError("explicit production.audit.path is required when audit is enabled")
            inside(Path(value), self.root)

    def child_env(self):
        # Preserve HOME, never inherit ambient provider credentials/proxy agents.
        env = {key: os.environ[key] for key in ("HOME", "PATH", "LANG", "LC_ALL", "XDG_CONFIG_HOME") if key in os.environ}
        env.update(TERM="xterm-256color", COLORTERM="truecolor", TMPDIR=str(self.root / "testtmp"),
                   GO_CLI_PROVIDER_NAME="openai", GO_CLI_PROVIDER_API_KEY=self.legacy.SENTINEL_API_KEY,
                   GO_CLI_PROVIDER_MODEL=self.model, GO_CLI_MODEL_NAME=self.model,
                   GO_CLI_TRACING_ENABLED="true", GO_CLI_TRACING_EXPORTER="jsonl")
        return env

    def start(self, role, action, cols, rows):
        with self.lock:
            if role not in self.scenarios or not self.scenarios[role].get("enabled", True):
                raise ValueError("scenario is disabled")
            ready = json.loads(self.ready_file.read_text())
            if not ready.get("allow_start") or ready.get("binary_sha256") != file_digest(self.binary):
                raise ValueError("production binary is not released by the ready gate")
            if not os.access(self.binary, os.X_OK):
                raise ValueError("binary is not executable")
            session = Session(self, role, action, cols, rows)
            self.sessions[session.id] = session
            self.save_manifest()
            return session.state()

    def save_manifest(self):
        with self.lock:
            workspaces = []
            for role, scenario in self.scenarios.items():
                workspace = scenario["workspace"]
                workspace_id, directory = authority(workspace)
                exact = [directory / (workspace_id + suffix) for suffix in (".jsonl", ".lock")]
                sidecars = sorted(directory.glob(workspace_id + ".jsonl.*"))
                workspaces.append({"role": role, "canonicalWorkspace": str(workspace), "workspaceId": workspace_id,
                                   "authority": [metadata(path) for path in exact + sidecars],
                                   "authorityPreexisted": self.original_authorities.get(role, []),
                                   "trustStore": str(Path(os.environ["HOME"]) / ".config/go-cli/trust.json"),
                                   "ownedTrustKey": str(workspace), "trust": own_trust_entry(workspace)})
            write_json(self.root / "artifacts" / "owned-resources.json", {
                "homeUnchanged": True, "root": str(self.root), "binary": str(self.binary),
                "workspaces": workspaces, "sessions": [session.state() for session in self.sessions.values()],
                "cleanup": "Stop all owned PTYs and bridges first. Revoke only listed trust keys via CLI; atomically remove only their tombstones under the production store lock. Remove only listed authority files belonging to these IDs. Never delete the shared trust file or directory."})

    def close(self):
        for session in list(self.sessions.values()):
            session.close()
        self.save_manifest()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, _format, *_args):
        return

    def reply(self, status, body, content_type="application/json"):
        data = json.dumps(body).encode() if content_type == "application/json" else body
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("X-Frame-Options", "DENY")
        self.end_headers()
        self.wfile.write(data)

    def authorized(self):
        return secrets.compare_digest(self.headers.get("X-Live-Control", ""), self.server.app.nonce)

    def do_GET(self):
        app = self.server.app
        url = urllib.parse.urlsplit(self.path)
        if self.headers.get("Host") != f"127.0.0.1:{self.server.server_port}":
            self.reply(403, {"error": "loopback Host required"})
            return
        if url.path == "/":
            page = (HERE / "index.html").read_text().replace("__CONTROL_NONCE__", app.nonce)
            self.reply(200, page.encode(), "text/html; charset=utf-8")
            return
        assets = {"/assets/xterm.js": "@xterm/xterm/lib/xterm.js", "/assets/xterm.css": "@xterm/xterm/css/xterm.css",
                  "/assets/addon-fit.js": "@xterm/addon-fit/lib/addon-fit.js"}
        if url.path in assets:
            path = app.assets / assets[url.path]
            self.reply(200, path.read_bytes(), "text/css" if path.suffix == ".css" else "text/javascript")
            return
        if not self.authorized():
            self.reply(403, {"error": "control nonce required"})
            return
        try:
            if url.path == "/api/status":
                self.reply(200, {"roles": [{"name": role, "enabled": scenario.get("enabled", True),
                                          "workspace": str(scenario["workspace"])} for role, scenario in app.scenarios.items()],
                                 "sessions": [session.state() for session in app.sessions.values()],
                                 "model": app.model, "ready": app.ready_file.exists()})
            elif url.path == "/api/output":
                query = urllib.parse.parse_qs(url.query)
                session = app.sessions[query["id"][0]]
                offset = max(0, int(query.get("offset", ["0"])[0]))
                chunk = b""
                if session.output_path.exists():
                    with session.output_path.open("rb") as source:
                        source.seek(offset)
                        chunk = source.read(131072)
                self.reply(200, {"offset": offset + len(chunk), "data": base64.b64encode(chunk).decode(), "state": session.state()})
            else:
                self.reply(404, {"error": "unknown endpoint"})
        except (ValueError, KeyError, OSError):
            self.reply(400, {"error": "invalid output/status request"})

    def do_POST(self):
        app = self.server.app
        origin = f"http://127.0.0.1:{self.server.server_port}"
        if not self.authorized() or self.headers.get("Origin") != origin or self.headers.get("Host") != origin[7:]:
            self.reply(403, {"error": "same-origin control nonce required"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length < 0 or length > 1048576:
                raise ValueError("control payload exceeds 1 MiB")
            body = json.loads(self.rfile.read(length))
            path = urllib.parse.urlsplit(self.path).path
            if path == "/api/start":
                result = app.start(body["role"], body["action"], max(40, min(300, int(body.get("cols", 100)))),
                                   max(10, min(100, int(body.get("rows", 30)))))
            else:
                session = app.sessions[body["id"]]
                if path == "/api/input":
                    text = body["text"]
                    if not isinstance(text, str) or "\x1b" in text or "\x00" in text:
                        raise ValueError("use labeled key buttons for terminal control characters")
                    data = text.encode()
                    if session.action == "interactive" and body.get("paste", True):
                        data = b"\x1b[200~" + data + b"\x1b[201~"
                    if body.get("enter", False):
                        data += b"\r"
                    session.send(data, "text+enter" if body.get("enter") else "text")
                    result = {"sentBytes": len(data)}
                elif path == "/api/key":
                    session.send(KEYS[body["key"]], "key:" + body["key"])
                    result = {"sent": True}
                elif path == "/api/terminal-data":
                    data = base64.b64decode(body["data"], validate=True)
                    if len(data) > 65536:
                        raise ValueError("terminal input exceeds 64 KiB")
                    session.send(data, "xterm-input")
                    result = {"sentBytes": len(data)}
                elif path == "/api/resize":
                    cols, rows = max(40, min(300, int(body["cols"]))), max(10, min(100, int(body["rows"])))
                    for owned in app.sessions.values():
                        if owned.proc.poll() is None:
                            owned.resize(cols, rows)
                    result = {"cols": cols, "rows": rows}
                elif path == "/api/label":
                    if session.bridge:
                        session.bridge.label_turn(str(body["label"])[:120], expect_provider=body.get("expect_provider", True))
                    result = {"labeled": True}
                elif path == "/api/audit":
                    result = session.audit()
                elif path == "/api/close":
                    session.close()
                    app.save_manifest()
                    result = session.state()
                else:
                    raise ValueError("unknown control endpoint")
            self.reply(200, result)
        except Exception as error:
            # Local validation messages contain no environment or upstream body.
            safe = str(error) if isinstance(error, (ValueError, FileNotFoundError)) else type(error).__name__
            self.reply(400, {"error": safe})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--scenario-config", type=Path, required=True)
    parser.add_argument("--ready-file", type=Path, required=True)
    parser.add_argument("--gateway", default="http://127.0.0.1:3425")
    parser.add_argument("--model", default="codex/gpt-6.1-sol")
    parser.add_argument("--port", type=int, default=0)
    parser.add_argument("--prepare-workspaces", action="store_true")
    args = parser.parse_args()
    app = App(args)
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    server.daemon_threads, server.app = True, app
    write_json(app.root / "artifacts/control.json", {"url": f"http://127.0.0.1:{server.server_port}/", "nonce": app.nonce,
                                                    "pid": os.getpid(), "readyFile": str(app.ready_file)})
    print(json.dumps({"url": f"http://127.0.0.1:{server.server_port}/", "controlFile": str(app.root / "artifacts/control.json"),
                      "startsReleased": False}), flush=True)
    def stop(_signum, _frame):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        server.serve_forever()
    finally:
        app.close()
        server.server_close()


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--pty-child":
        spec = json.loads(Path(sys.argv[2]).read_text())
        fcntl.ioctl(0, termios.TIOCSCTTY, 0)
        os.chdir(spec["cwd"])
        os.execve(spec["args"][0], spec["args"], dict(os.environ))
    main()
