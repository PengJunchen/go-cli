#!/usr/bin/env python3
"""Measure real CLI startup/tool loops against a local scripted provider.

No paid model calls. No install/build is performed. Run after the full functional
verification stage. Output keeps every failure and raw stdout/stderr per arm.
"""
import argparse
import datetime
import getpass
import hashlib
import http.server
import json
import math
import os
import platform
import shutil
import signal
import statistics
import subprocess
import sys
import threading
import time
from pathlib import Path

MARKER = "BENCH_OK"
OLD = "answer = 41\n"
NEW = "answer = 42\n"
TOOLS_PROMPT = "Read answer.txt, edit answer = 41 to answer = 42, read answer.txt again, then return BENCH_OK."
READY_PROMPT = "Return BENCH_OK."


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")


class Fixture(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        state = self.server.state
        started = time.perf_counter()
        tool_map = {t["function"]["name"]: t["function"] for t in body.get("tools", [])}
        state["requests"].append({
            "received_ms": (started - state["started"]) * 1000,
            "schema_bytes": len(json.dumps(body.get("tools", []), ensure_ascii=False).encode()),
            "tool_names": sorted(tool_map), "messages": len(body.get("messages", [])),
            "tool_receipt_ids": [m.get("tool_call_id") for m in body.get("messages", []) if m.get("role") == "tool"],
            "latest_tool_receipt": next((str(m.get("content", "")) for m in reversed(body.get("messages", [])) if m.get("role") == "tool"), None),
        })
        # Derive the next task step from receipts, never advance on HTTP retries.
        turn = sum(m.get("role") == "tool" for m in body.get("messages", []))
        tool = None
        arguments = None
        try:
            if state["scenario"] == "tools" and turn < 3:
                candidates = ("edit", "edit_file") if turn == 1 else ("read", "read_file")
                tool = next((name for name in candidates if name in tool_map), None)
                if tool is None:
                    raise ValueError("required tool absent: " + repr(candidates))
                props = tool_map[tool]["parameters"].get("properties", {})
                path_key = next((key for key in ("path", "file_path", "filePath") if key in props), "path")
                arguments = {path_key: str(state["workspace"] / "answer.txt")}
                if turn == 1:
                    old_key = next((key for key in ("oldText", "old_string", "oldString") if key in props), "oldText")
                    new_key = next((key for key in ("newText", "new_string", "newString") if key in props), "newText")
                    arguments.update({old_key: OLD, new_key: NEW})
            elif state["scenario"] == "tools" and turn != 3:
                raise ValueError("unexpected extra request after fixed tool sequence")
        except ValueError as exc:
            state["protocol_errors"].append(str(exc))
            self.send_error(422, str(exc))
            return
        message = {"role": "assistant", "content": MARKER if tool is None else None}
        finish = "stop"
        if tool is not None:
            finish = "tool_calls"
            message["tool_calls"] = [{"id": "fixture-" + str(turn), "type": "function",
                "function": {"name": tool, "arguments": json.dumps(arguments)}}]
        self.send_response(200)
        if body.get("stream"):
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            delta = dict(message)
            if tool is not None:
                delta["tool_calls"][0]["index"] = 0
            chunk = {"id": "fixture", "object": "chat.completion.chunk", "created": 1,
                     "model": "fixture", "choices": [{"index": 0, "delta": delta, "finish_reason": None}]}
            self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
            chunk["choices"] = [{"index": 0, "delta": {}, "finish_reason": finish}]
            self.wfile.write(("data: " + json.dumps(chunk) + "\n\ndata: [DONE]\n\n").encode())
        else:
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            result = {"id": "fixture", "object": "chat.completion", "created": 1,
                      "model": "fixture", "choices": [{"index": 0, "message": message, "finish_reason": finish}],
                      "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}
            self.wfile.write(json.dumps(result).encode())
        self.wfile.flush()


def command_version(argv):
    try:
        result = subprocess.run(argv, capture_output=True, text=True, timeout=10)
        return {"argv": argv, "exit_code": result.returncode, "stdout": result.stdout.strip(), "stderr": result.stderr.strip()}
    except (OSError, subprocess.TimeoutExpired) as exc:
        return {"argv": argv, "error": str(exc)}



class ProcessTreeSampler:
    """50 ms PID/PPID RSS snapshots; short peaks between samples remain unobserved."""
    def __init__(self, pid, interval=.05):
        self.pid = pid
        self.interval = interval
        self.peak_bytes = None
        self.samples = 0
        self.observed_pids = set()
        self.errors = []
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)

    def _run(self):
        while not self.stop.is_set():
            try:
                result = subprocess.run(["ps", "-axo", "pid=,ppid=,rss="],
                                        capture_output=True, text=True, timeout=2)
                if result.returncode != 0:
                    raise RuntimeError("ps exited " + str(result.returncode))
                processes = {}
                for line in result.stdout.splitlines():
                    pid, ppid, rss_kib = map(int, line.split())
                    processes[pid] = (ppid, rss_kib * 1024)
                # Remember previously seen descendants so reparenting during
                # this invocation does not drop a still-running worker.
                members = ({self.pid} | self.observed_pids) & processes.keys()
                changed = True
                while changed:
                    children = {pid for pid, (ppid, _) in processes.items() if ppid in members}
                    changed = not children.issubset(members)
                    members.update(children)
                if members:
                    self.observed_pids.update(members)
                    total = sum(processes[pid][1] for pid in members)
                    self.peak_bytes = max(self.peak_bytes or 0, total)
                    self.samples += 1
            except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired) as exc:
                self.errors.append(str(exc))
            self.stop.wait(self.interval)

    def start(self):
        self.thread.start()

    def finish(self):
        self.stop.set()
        self.thread.join(timeout=3)
        return {"peak_bytes": self.peak_bytes, "samples": self.samples,
                "observed_pids": sorted(self.observed_pids), "errors": self.errors,
                "interval_ms": self.interval * 1000,
                "scope": "sum RSS of sampled PID/PPID descendants; shared pages may be counted more than once; peaks between samples are missed"}



def cleanup_owned_group(pid, observed_pids):
    """Inspect after wait4; never kill any group other than this invocation's."""
    report = {"residual_group_pids": [], "observed_pids_still_alive": [], "cleanup_error": None}
    try:
        snapshot = subprocess.run(["ps", "-axo", "pid=,pgid=,stat="], capture_output=True, text=True, timeout=2)
        if snapshot.returncode != 0:
            raise RuntimeError("process cleanup ps exited " + str(snapshot.returncode))
        for line in snapshot.stdout.splitlines():
            current, group, status = line.split()
            if status.startswith("Z"):
                continue
            if int(group) == pid:
                report["residual_group_pids"].append(int(current))
            if int(current) in observed_pids:
                report["observed_pids_still_alive"].append(int(current))
        if report["residual_group_pids"]:
            try:
                os.killpg(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
    except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired) as exc:
        report["cleanup_error"] = str(exc)
    return report


def run_arm(arm, scenario, root, index, server, timeout, warmup):
    target = root / ("warmup" if warmup else "measured") / (str(index) + "-" + scenario + "-" + arm["name"])
    workspace = target / "workspace"
    workspace.mkdir(parents=True)
    # An empty conventional project directory shadows personal Skill discovery.
    # No trust grant or instructions are fabricated.
    (workspace / ".go-cli" / "skills").mkdir(parents=True)
    (workspace / "answer.txt").write_text(OLD)
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("GO_CLI_", "PI_", "OPENCODE_")) and key not in ("NODE_OPTIONS",)}
    # HOME is intentionally unchanged. Go personal config is preflighted;
    # PI has its supported dedicated resource-dir override.
    env.update({"NO_COLOR": "1", "PI_SKIP_VERSION_CHECK": "1"})
    endpoint = "http://127.0.0.1:" + str(server.server_port) + "/v1"
    prompt = TOOLS_PROMPT if scenario == "tools" else READY_PROMPT
    if arm["kind"] == "go":
        env.update({"GO_CLI_PROVIDER_NAME": "openai", "GO_CLI_PROVIDER_API_KEY": "fixture",
                    "GO_CLI_PROVIDER_BASE_URL": endpoint, "GO_CLI_MODEL_NAME": "fixture",
                    "GO_CLI_TRACING_ENABLED": "false", "GO_CLI_GIT_ENABLED": "false",
                    "GO_CLI_EXTENSIONS_ENABLED": "false", "GO_CLI_APPROVAL_MODE": "auto"})
        command = arm["argv"] + ["prompt", "--provider", "openai", "--model", "fixture", "--approve", "auto", "--output", "text", prompt]
    elif arm["kind"] == "opencode":
        env.update({"OPENCODE_CONFIG_DIR": str(target / "opencode-config"),
                    "OPENCODE_TEST_HOME": str(target / "opencode-home"),
                    "XDG_CONFIG_HOME": str(target / "xdg-config"),
                    "XDG_DATA_HOME": str(target / "xdg-data"), "XDG_CACHE_HOME": str(target / "xdg-cache"),
                    "XDG_STATE_HOME": str(target / "xdg-state")})
        configuration = {"update": "disable", "share": "disabled", "snapshots": False,
            "providers": {"fixture": {"name": "fixture", "package": "@opencode/ai/providers/openai-compatible",
                "settings": {"baseURL": endpoint, "apiKey": "fixture"},
                "models": {"fixture": {"name": "fixture", "capabilities": {"tools": True, "input": ["text"], "output": ["text"]},
                    "limit": {"context": 128000, "output": 2048}}}}}}
        env["OPENCODE_CONFIG_CONTENT"] = json.dumps(configuration)
        write_json(target / "opencode-fixture-config.json", configuration)
        command = arm["argv"] + ["run", "--standalone", "--auto", "--title", "fixture", "--model", "fixture/fixture", prompt]
    else:
        agent_dir = target / "pi-agent"
        env["PI_CODING_AGENT_DIR"] = str(agent_dir)
        model = {"id": "fixture", "name": "fixture", "reasoning": False, "input": ["text"],
                 "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
                 "contextWindow": 128000, "maxTokens": 2048}
        write_json(agent_dir / "models.json", {"providers": {"fixture": {
            "baseUrl": endpoint, "apiKey": "fixture", "api": "openai-completions", "models": [model]}}})
        command = arm["argv"] + ["--provider", "fixture", "--model", "fixture", "--print", "--no-session", "--no-extensions", "--no-skills", "--no-mcp", "--offline", prompt]
    state = {"scenario": scenario, "workspace": workspace, "requests": [], "protocol_errors": []}
    server.state = state
    with (target / "stdout.txt").open("wb") as stdout, (target / "stderr.txt").open("wb") as stderr:
        started = time.perf_counter()
        state["started"] = started
        proc = subprocess.Popen(command, cwd=workspace, env=env, stdin=subprocess.DEVNULL,
                                stdout=stdout, stderr=stderr, start_new_session=True)
        sampler = ProcessTreeSampler(proc.pid)
        sampler.start()
        timed_out = threading.Event()
        def terminate():
            timed_out.set()
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        timer = threading.Timer(timeout, terminate)
        timer.start()
        try:
            _, status, usage = os.wait4(proc.pid, 0)
            proc.returncode = os.waitstatus_to_exitcode(status)
            ended = time.perf_counter()
        finally:
            timer.cancel()
            cleanup_started = time.perf_counter()
            tree_rss = sampler.finish()
            process_cleanup = cleanup_owned_group(proc.pid, tree_rss["observed_pids"])
            process_cleanup["post_wait_verification_ms"] = (time.perf_counter() - cleanup_started) * 1000
            process_cleanup["grace_ms"] = 0
        wall_ms = (ended - started) * 1000
    peak_rss = usage.ru_maxrss if sys.platform == "darwin" else usage.ru_maxrss * 1024
    content_ok = scenario != "tools" or (workspace / "answer.txt").read_text() == NEW
    output_ok = MARKER in (target / "stdout.txt").read_text(errors="replace")
    expected_requests = 4 if scenario == "tools" else 1
    receipts_ok = scenario != "tools" or (len(state["requests"]) == 4 and "answer = 41" in str(state["requests"][1]["latest_tool_receipt"]) and "answer = 42" in str(state["requests"][3]["latest_tool_receipt"]))
    processes_exited = not (process_cleanup["residual_group_pids"] or process_cleanup["observed_pids_still_alive"] or process_cleanup["cleanup_error"])
    passed = proc.returncode == 0 and processes_exited and not timed_out.is_set() and content_ok and output_ok and receipts_ok and len(state["requests"]) == expected_requests and not state["protocol_errors"]
    observation = {"arm": arm["name"], "scenario": scenario, "iteration": index, "warmup": warmup,
        "argv": command, "exit_code": proc.returncode, "timed_out": timed_out.is_set(), "passed": passed,
        "process_cleanup": process_cleanup,
        "wall_ms": wall_ms, "wall_scope": "Popen through primary-process wait4; post-exit verification/cleanup is separate", "cpu_ms": (usage.ru_utime + usage.ru_stime) * 1000, "peak_rss_bytes": peak_rss,
        "peak_rss_scope": "wait4 child-reported; not a summed process-tree RSS",
        "tree_peak_rss_bytes": tree_rss["peak_bytes"], "tree_rss": tree_rss,
        "first_request_ms": state["requests"][0]["received_ms"] if state["requests"] else None,
        "requests": state["requests"], "protocol_errors": state["protocol_errors"], "artifacts": str(target)}
    write_json(target / "observation.json", observation)
    return observation


def percentile(values, fraction):
    return sorted(values)[max(0, math.ceil(len(values) * fraction) - 1)]



def host_info():
    result = {"logical_cpus": os.cpu_count(), "cpu_model": platform.processor() or None}
    try:
        result["memory_bytes"] = os.sysconf("SC_PAGE_SIZE") * os.sysconf("SC_PHYS_PAGES")
    except (OSError, ValueError):
        result["memory_bytes"] = None
    if sys.platform == "darwin":
        cpu = command_version(["sysctl", "-n", "machdep.cpu.brand_string"])
        if cpu.get("exit_code") == 0:
            result["cpu_model"] = cpu["stdout"]
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go-cli", action="append", default=[], metavar="LABEL=PATH")
    parser.add_argument("--opencode-cli", type=Path, help="Optional official v2 binary; runs a private --standalone server")
    parser.add_argument("--pi-command", help="JSON argv array, e.g. '[\"/usr/bin/node\",\"/tmp/pi/dist/bundle/cli.js\"]'")
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--purpose", choices=["pilot", "measurement"], default="measurement")
    parser.add_argument("--runs", type=int, default=30)
    parser.add_argument("--warmups", type=int, default=5)
    parser.add_argument("--timeout", type=float, default=30)
    parser.add_argument("--scenario", choices=["ready", "tools", "all"], default="all")
    args = parser.parse_args()
    if not hasattr(os, "wait4"):
        parser.error("Unix wait4 is required for per-process resource accounting")
    if args.runs < 1 or args.warmups < 0 or args.timeout <= 0:
        parser.error("invalid run count or timeout")
    # Do not rewrite user config or HOME to make a benchmark appear clean.
    personal_sources = [Path(".go-cli") / ("config." + ext) for ext in ("json", "yaml", "yml")]
    personal_sources += [Path(".config/go-cli") / name for name in
                         ("skills", "mcp.json", "hooks.json", "hooks.yaml", "hooks.yml", "auth.json", "models.json", "commands")]
    existing = [str(Path.home() / relative) for relative in personal_sources
                if os.path.lexists(Path.home() / relative)]
    if existing:
        parser.error("Go personal config/resources would contaminate results; run in a clean container/account: " + ", ".join(existing))
    memory_file = Path.home() / ".go-cli/memories.jsonl"
    memory_bytes = memory_file.stat().st_size if memory_file.exists() else None
    if memory_file.is_symlink() or (memory_file.exists() and not memory_file.is_file()) or memory_bytes not in (None, 0):
        parser.error("Go personal memory must be absent or an empty regular file; do not read personal memory for a benchmark")
    opencode_system_sources = []
    if args.opencode_cli and sys.platform == "darwin":
        opencode_system_sources = [Path("/Library/Application Support/opencode"),
            Path("/Library/Managed Preferences/ai.opencode.managed.plist"),
            Path("/Library/Managed Preferences") / getpass.getuser() / "ai.opencode.managed.plist"]
        if any(os.path.lexists(path) for path in opencode_system_sources):
            parser.error("OpenCode system/managed config would contaminate results; use a clean host")
    arms = []
    for value in args.go_cli:
        name, binary = value.split("=", 1)
        if not name.replace("-", "").replace("_", "").isalnum():
            parser.error("arm labels must be alphanumeric with - or _")
        binary = Path(binary).resolve(strict=True)
        arms.append({"name": name, "kind": "go", "argv": [str(binary)], "binary_bytes": binary.stat().st_size,
                     "sha256": hashlib.sha256(binary.read_bytes()).hexdigest()})
    if args.pi_command:
        command = json.loads(args.pi_command)
        if not isinstance(command, list) or not command or not all(isinstance(x, str) for x in command):
            parser.error("--pi-command must contain a nonempty JSON string argv array")
        command[0] = str(Path(shutil.which(command[0]) or command[0]).resolve(strict=True))
        arms.append({"name": "pi", "kind": "pi", "argv": command, "binary_bytes": None})
    if args.opencode_cli:
        binary = args.opencode_cli.resolve(strict=True)
        arms.append({"name": "opencode-v2", "kind": "opencode", "argv": [str(binary)],
                     "binary_bytes": binary.stat().st_size, "sha256": hashlib.sha256(binary.read_bytes()).hexdigest()})
    if not arms or len({arm["name"] for arm in arms}) != len(arms):
        parser.error("provide at least one arm; labels must be unique")
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    scenarios = ["ready", "tools"] if args.scenario == "all" else [args.scenario]
    protocol = {"format": "go-cli-runtime-benchmark/v1", "platform": platform.platform(),
                "started_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                "host": host_info(), "harness_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                "machine": platform.machine(), "python": sys.version, "runs": args.runs, "purpose": args.purpose,
                "warmups": args.warmups, "scenarios": scenarios, "arms": arms,
                "order": "AB/BA alternates by iteration", "model": "local scripted fixture; no quality/real-model claim",
                "prompts": {"ready": READY_PROMPT, "tools": TOOLS_PROMPT},
                "cache": "fresh workspace/resource dir; OS cache not flushed",
                "memory": "PID/PPID sum RSS sampled every 50ms; wait4 parent rusage separately reported; ps sampler adds overhead",
                "resource_isolation": "HOME unchanged; Go preflights known personal config/Skills/MCP/hooks/auth/models/commands by metadata only, accepts absent or empty memory, and shadows conventional Skill dir; PI_CODING_AGENT_DIR; OpenCode XDG roots/CONFIG_DIR plus official OPENCODE_TEST_HOME isolate compatibility Skills",
                "schema_bytes_method": "UTF-8 length of json.dumps(tools, ensure_ascii=False), default separators; re-encoded subset, not original wire body or tokenizer tokens",
                "opencode_system_preflight_absent": [str(path) for path in opencode_system_sources],
                "go_personal_preflight": {"paths_absent": [str(path) for path in personal_sources], "memory_bytes": memory_bytes, "read_personal_content": False},
                "go_version": command_version(["go", "version"]),
                "versions": [command_version(arm["argv"] + ["--version"]) for arm in arms]}
    write_json(root / "protocol.json", protocol)
    server = http.server.HTTPServer(("127.0.0.1", 0), Fixture)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    observations = []
    try:
        with (root / "observations.jsonl").open("w") as records:
            for warmup, count in ((True, args.warmups), (False, args.runs)):
                for index in range(count):
                    for scenario in scenarios:
                        for arm in (arms if index % 2 == 0 else list(reversed(arms))):
                            row = run_arm(arm, scenario, root, index, server, args.timeout, warmup)
                            observations.append(row)
                            records.write(json.dumps(row) + "\n")
                            records.flush()
                            print(json.dumps({key: row[key] for key in ("arm", "scenario", "iteration", "warmup", "passed", "wall_ms")}), flush=True)
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=2)
    summary = []
    for arm in arms:
        for scenario in scenarios:
            rows = [row for row in observations if not row["warmup"] and row["arm"] == arm["name"] and row["scenario"] == scenario]
            warmup_rows = [row for row in observations if row["warmup"] and row["arm"] == arm["name"] and row["scenario"] == scenario]
            warmups_passed = len(warmup_rows) == args.warmups and all(row["passed"] for row in warmup_rows)
            all_passed = len(rows) == args.runs and all(row["passed"] for row in rows) and warmups_passed
            result = {"arm": arm["name"], "scenario": scenario, "planned": args.runs,
                      "observed": len(rows), "passed": sum(row["passed"] for row in rows),
                      "warmups_planned": args.warmups, "warmups_observed": len(warmup_rows),
                      "warmups_passed": sum(row["passed"] for row in warmup_rows),
                      "cohort_passed": all_passed,
                      "headline_metrics_available": all_passed and args.purpose == "measurement" and args.runs >= 30 and args.warmups >= 5}
            if result["headline_metrics_available"]:
                for key in ("wall_ms", "cpu_ms", "peak_rss_bytes", "tree_peak_rss_bytes", "first_request_ms"):
                    values = [row[key] for row in rows]
                    if any(value is None for value in values):
                        result[key] = {"available": False}
                        continue
                    result[key] = {"median": statistics.median(values), "p95": percentile(values, .95)}
            summary.append(result)
    write_json(root / "summary.json", summary)
    return 0 if all(item["cohort_passed"] for item in summary) else 1


if __name__ == "__main__":
    raise SystemExit(main())
