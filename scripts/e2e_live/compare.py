#!/usr/bin/env python3
"""Paired live-provider experiment. Screenshots render unmodified real PTY bytes."""
from __future__ import annotations

import argparse
import html
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import statistics
import subprocess
import threading
import time
import uuid


spec = importlib.util.spec_from_file_location("live", Path(__file__).with_name("run.py"))
live = importlib.util.module_from_spec(spec)
spec.loader.exec_module(live)


def prompt_hash(prompt):
    return live.sha256(prompt.encode("utf-8"))


def clone_resume_store(source, destination):
    inventory = [{"path": str(p.relative_to(source)), "sha256": live.sha256_file(p)} for p in sorted(source.rglob("*")) if p.is_file()]
    shutil.copytree(source, destination, dirs_exist_ok=True)
    copied = [{"path": str(p.relative_to(destination)), "sha256": live.sha256_file(p)} for p in sorted(destination.rglob("*")) if p.is_file()]
    if inventory != copied:
        raise ValueError("resume store clone differs from the original bytes")
    return inventory


def actual_answer(rows):
    return "\n\n".join(row.get("assistantText", "") for row in rows if row.get("stream") is True and row.get("assistantText"))


def new_receipts(rows):
    ids = {call.get("id") for row in rows for call in row.get("toolCalls", [])}
    return [receipt for row in rows for receipt in row.get("toolReceipts", []) if receipt.get("toolCallId") in ids]


def bound_task_review(review, arm, row):
    result = review.get("arms", {}).get(arm, {}).get(row["id"], {})
    if not result or result.get("actualAnswerSha256") != prompt_hash(row.get("actualAnswer", "")):
        return {"taskStatus": "INCONCLUSIVE", "reason": "No independent review bound to this exact actual-answer SHA-256."}
    return result


def summarize(rows):
    not_run = len([r for r in rows if r.get("status") == "not_run"])
    attempted = len(rows) - not_run
    completed = [row for row in rows if row.get("status") == "completed"]
    streak = longest = 0
    for row in rows:
        streak = streak + 1 if row.get("status") == "completed" else 0
        longest = max(longest, streak)
    times = [row["elapsedSeconds"] for row in completed]
    return {"attempted": attempted, "completed": len(completed), "failed": attempted - len(completed), "notRun": not_run, "longestContinuousSuccess": longest,
            "completedSecondsMedian": statistics.median(times) if times else None,
            "completedSecondsP95": sorted(times)[max(0, int(len(times) * .95 + .999999) - 1)] if times else None}


def paired_speed(develop, main):
    baseline = {row["id"]: row for row in main}
    deltas = [row["elapsedSeconds"] - baseline[row["id"]]["elapsedSeconds"] for row in develop
              if row.get("status") == "completed" and row["id"] in baseline and baseline[row["id"]].get("status") == "completed"]
    pure = [(row, baseline[row["id"]]) for row in develop if row.get("status") == "completed" and row["id"] in baseline and baseline[row["id"]].get("status") == "completed"
            and all(r.get("kind") == "short-text" and not r.get("approvals") and not r.get("toolCalls") and len(r.get("requestSequences", [])) == 1 for r in (row, baseline[row["id"]]))]
    pure_deltas = [a["elapsedSeconds"] - b["elapsedSeconds"] for a, b in pure]
    return {"successfulPairs": len(deltas), "developMinusMainMedianSeconds": statistics.median(deltas) if deltas else None,
            "pureShortTextSuccessfulPairs": len(pure), "pureShortTextDevelopMinusMainMedianSeconds": statistics.median(pure_deltas) if pure_deltas else None,
            "headlineSpeedMetric": "pureShortTextDevelopMinusMainMedianSeconds; excludes tools, approvals, retries and nonstream calls",
            "interpretation": "Only both-completed pairs; failures never count as speed wins. Provider cache/load and different generated answers remain confounders."}


class TimedForwarder(live.LiveForwarder):
    def begin(self, method, path, body):
        index, label = super().begin(method, path, body)
        with self.lock:
            record = self.requests[index - 1]
            record["startedMonotonic"] = time.monotonic()
            record["assistantText"] = ""
            record["firstByteSeconds"] = None
            record["firstContentSeconds"] = None
            record["latestUserSha256"] = None
            try:
                payload = json.loads(body)
                messages = payload.get("messages", [])
                catalog = payload.get("tools", [])
                record["advertisedToolNames"] = [t.get("function", {}).get("name") for t in catalog if isinstance(t, dict)]
                record["advertisedToolsSchemaSha256"] = prompt_hash(json.dumps(catalog, sort_keys=True, ensure_ascii=False, separators=(",", ":")))
                record["advertisedToolCount"] = len(catalog)
                users = [m for m in messages if m.get("role") == "user"]
                if users:
                    latest = live.as_text(users[-1].get("content"))
                    record["latestUserSha256"] = prompt_hash(latest)
                    markers = re.findall(r"\bTURN_GZ_T\d+\b", latest)
                    record["userMarkers"] = markers
                    kind, matches = live.classify_request_kind(record.get("stream"), self.current_expected_input_markers, markers)
                    record["requestKind"], record["inputMarkerMatches"] = kind, matches
            except (ValueError, AttributeError):
                pass
        return index, label

    def response_chunk(self, index, payload):
        with self.lock:
            row = self.requests[index - 1]
            if payload and row["firstByteSeconds"] is None:
                row["firstByteSeconds"] = time.monotonic() - row["startedMonotonic"]
        super().response_chunk(index, payload)

    def merge_response(self, index, value):
        super().merge_response(index, value)
        with self.lock:
            row = self.requests[index - 1]
            for choice in value.get("choices") or []:
                text = live.as_text((choice.get("delta") or choice.get("message") or {}).get("content"))
                if text:
                    if row["firstContentSeconds"] is None:
                        row["firstContentSeconds"] = time.monotonic() - row["startedMonotonic"]
                    row["assistantText"] += text

    def finish(self, index, status, headers):
        with self.lock:
            self.requests[index - 1]["upstreamElapsedSeconds"] = time.monotonic() - self.requests[index - 1]["startedMonotonic"]
        super().finish(index, status, headers)


class Arm:
    def __init__(self, name, repo, output, args):
        self.name, self.repo, self.root = name, repo.resolve(), output / name
        self.home, self.workspace, self.store, self.trace = [self.root / key for key in ("home", "workspace", "sessions", "traces")]
        resume_id = None
        resume_inventory = None
        resume_authority_inventory = None
        if args.resume_from:
            if args.only != name:
                raise ValueError("resume requires an explicit --only arm")
            self.workspace = args.resume_from / "workspace"
            resume_inventory = clone_resume_store(args.resume_from / "sessions", self.store)
            authority_relative = Path("Library/Application Support/go-cli/workspace-quarantine")
            authority_origin = args.resume_from / "home" / authority_relative
            if authority_origin.exists():
                resume_authority_inventory = clone_resume_store(authority_origin, self.home / authority_relative)
                # The durable authority validates its private parent directories too.
                for relative in (Path("Library"), Path("Library/Application Support"), Path("Library/Application Support/go-cli")):
                    shutil.copystat(args.resume_from / "home" / relative, self.home / relative)
            identities = live.physical_session_ids(self.store)
            if len(identities) != 1:
                raise ValueError(f"resume requires one exact prior physical session, found {len(identities)}")
            resume_id = identities[0]
        self.evidence = self.root / "evidence"
        for path in (self.home, self.workspace, self.store, self.trace, self.evidence):
            path.mkdir(parents=True, exist_ok=True)
        subprocess.run(["git", "init", "--quiet", str(self.workspace)], check=True)
        self.binary = self.root / "go-cli-race"
        with (self.root / "build.log").open("w") as log:
            subprocess.run(["go", "build", "-race", "-o", str(self.binary), "./cmd/cli"], cwd=self.repo, stdout=log, stderr=subprocess.STDOUT, check=True)
        revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repo, text=True).strip()
        self.metadata = {"revision": revision, "binarySha256": live.sha256_file(self.binary), "worktree": str(self.repo), "workspace": str(self.workspace)}
        self.metadata["resumedPhysicalSessionId"] = resume_id
        self.metadata["resumeFrom"] = str(args.resume_from) if args.resume_from else None
        self.metadata["resumeOriginalStoreInventory"] = resume_inventory
        self.metadata["resumeOriginalAuthorityInventory"] = resume_authority_inventory
        self.metadata["dirtyDiffSha256"] = live.sha256(subprocess.check_output(["git", "diff", "HEAD"], cwd=self.repo))
        self.metadata["sourceFiles"] = [{"path": str(p.relative_to(self.repo)), "sha256": live.sha256_file(p)} for p in sorted(self.repo.rglob("*.go")) if "/.git/" not in str(p)]
        config = {"tracing": {"enabled": True, "exporter": "jsonl", "file_path": str(self.trace), "redaction_level": "redact"},
                  "session": {"store_path": str(self.store)}, "small_model": {"provider": "openai", "model": args.model},
                  "compaction": {"strategy": "unified", "max_tokens": args.max_tokens},
                  "production": {"model_timeout": {"total": args.model_timeout_total, "stream_idle": args.model_timeout_idle}}}
        config_path = self.home / ".go-cli/config.yaml"
        config_path.parent.mkdir(parents=True)
        config_text = "\n".join([
            "tracing:", "  enabled: true", "  exporter: jsonl", "  file_path: " + json.dumps(str(self.trace)), "  redaction_level: redact",
            "session:", "  store_path: " + json.dumps(str(self.store)),
            "small_model:", "  provider: openai", "  model: " + args.model,
            "compaction:", "  strategy: unified", "  max_tokens: " + str(args.max_tokens),
            "production:", "  model_timeout:", "    total: " + args.model_timeout_total, "    stream_idle: " + args.model_timeout_idle, "",
        ])
        config_path.write_text(config_text, encoding="utf-8")
        self.metadata["config"] = config
        self.metadata["configSha256"] = live.sha256_file(config_path)
        self.metadata["normalizedConfigSha256"] = prompt_hash(config_text.replace(str(self.root), "<ARM_ROOT>"))
        if args.fixtures:
            if args.fixtures.is_file():
                shutil.copy2(args.fixtures, self.workspace / args.fixtures.name)
            else:
                shutil.copytree(args.fixtures, self.workspace, dirs_exist_ok=True)
        self.metadata["inputFixtureInventory"] = live.workspace_inventory(self.workspace)
        self.bridge = TimedForwarder(args.gateway, "guangzhou-" + name + "-" + uuid.uuid4().hex[:16])
        self.thread = threading.Thread(target=self.bridge.serve_forever, daemon=True)
        self.thread.start()
        os.environ["LIVE_E2E_TRACE_DIR"] = str(self.trace)
        self.session = live.LivePTY(self.binary, self.home, self.workspace, self.bridge, live.locate_node(), args.model, args.max_tokens, resume_session_id=resume_id)
        self.session.session_store_path = self.store
        self.rows = []
        self.active = True
        try:
            self.session.wait_for("Interactive session started", 90)
            self.session.wait_until(live.idle_input_prompt, "idle input prompt", 30)
            self.session.capture(self.evidence, "startup")
        except Exception as error:
            self.metadata["startupFailure"] = f"{type(error).__name__}: {error}"
            self.session.capture(self.evidence, "startup-failed")
            self.save(final=True)
            self.session.close()
            self.bridge.shutdown()
            self.bridge.server_close()
            raise

    def save(self, final=False):
        (self.root / "bridge.json").write_text(json.dumps(self.bridge.snapshot(), ensure_ascii=False, indent=2), encoding="utf-8")
        (self.root / "metadata.json").write_text(json.dumps(self.metadata, ensure_ascii=False, indent=2), encoding="utf-8")
        (self.root / "ledger.json").write_text(json.dumps(self.rows, ensure_ascii=False, indent=2), encoding="utf-8")
        if final:
            self.session.save_transcript(self.root / "pty.raw")

    def turn(self, turn, order, timeout):
        label = turn["id"]
        prompt = turn["prompt"]
        # Actual bracketed paste preserves all bytes, including double spaces/newlines.
        self.session.turn_terminal_baseline = {r.get("id") for r in live.terminal_run_checkpoints(self.store)}
        marker = "TURN_GZ_" + label
        self.bridge.label_turn(label, [marker], [marker])
        start = time.monotonic()
        raw_offset = len(self.session.raw_bytes)
        row = {"id": label, "phase": turn.get("phase"), "kind": turn.get("kind"), "prompt": prompt, "promptSha256": prompt_hash(prompt), "pairOrder": order,
               "startedAt": live.now(), "status": "pending", "approvals": [], "screenshots": [], "uiFirstContentSeconds": None,
               "uiFirstIdleSeconds": None, "approvalDurationSeconds": 0, "controllerActions": [], "actualAnswer": "", "elapsedSeconds": 0, "providerFirstContentSeconds": None}
        self.rows.append(row)
        self.session.send_line(prompt)
        self.save()
        quiet = None
        last_progress = start
        try:
            while time.monotonic() - start < timeout:
                self.session.pump(.10)
                lines = self.session.screen_lines()
                requests = [r for r in self.bridge.snapshot()["requests"] if r["turn"] == label]
                actual = actual_answer(requests)
                if time.monotonic() - last_progress >= 20:
                    row["actualAnswer"] = actual
                    row["elapsedSeconds"] = round(time.monotonic() - start, 3)
                    self.save()
                    print(json.dumps({"arm": self.name, "id": label, "status": "in_progress", "seconds": row["elapsedSeconds"], "requests": len(requests), "approvals": len(row["approvals"])}, ensure_ascii=False), flush=True)
                    last_progress = time.monotonic()
                if live.active_busy_footer(lines) and not any(s["file"] == label + "-busy.png" for s in row["screenshots"]):
                    row["screenshots"].append(self.session.capture(self.evidence, label + "-busy"))
                if actual and row["uiFirstContentSeconds"] is None:
                    fragment = next((r["assistantText"][:min(16, len(r["assistantText"]))] for r in requests if r.get("stream") and r.get("assistantText")), "")
                    if fragment and fragment in "\n".join(lines):
                        row["uiFirstContentSeconds"] = time.monotonic() - start
                        row["screenshots"].append(self.session.capture(self.evidence, label + "-content"))
                approval_tool = live.active_approval_tool(lines)
                pending = live.unresolved_tool_call_ids(self.bridge, label, approval_tool) if approval_tool else set()
                if pending - self.session.decided_tool_call_ids:
                    decision = "deny" if turn.get("approval") == "deny" else "allow"
                    approval_start = time.monotonic()
                    result = live.drive_approval(self.session, self.evidence, f"{label}-{len(row['approvals'])+1}", decision, row["screenshots"], timeout=min(30, timeout), screen=lines)
                    result["durationSeconds"] = time.monotonic() - approval_start
                    row["approvalDurationSeconds"] += result["durationSeconds"]
                    row["approvals"].append(result)
                    quiet = None
                    continue
                terminals = [r for r in live.terminal_run_checkpoints(self.store) if r.get("id") not in self.session.turn_terminal_baseline]
                failures = [r for r in terminals if r.get("run_outcome") in ("failed", "error")]
                if failures:
                    row["status"] = "terminal_error"
                    row["terminalCheckpoints"] = terminals
                    self.passive_finish(row, 15)
                    break
                if self.session.exit_status() is not None:
                    row["status"] = "process_exit"
                    break
                with self.bridge.lock:
                    inflight = self.bridge.inflight
                if requests and actual and live.idle_input_prompt(lines) and not live.active_busy_footer(lines) and not approval_tool and row["uiFirstIdleSeconds"] is None:
                    row["uiFirstIdleSeconds"] = time.monotonic() - start
                if requests and inflight == 0 and live.idle_input_prompt(lines) and not live.active_busy_footer(lines) and not approval_tool:
                    if quiet is None:
                        quiet = time.monotonic()
                    elif time.monotonic() - quiet >= 1:
                        success = any(r.get("run_outcome") in ("success", "completed") for r in terminals)
                        streams = [r for r in requests if r.get("stream")]
                        transport_complete = bool(streams) and all(r.get("status") == 200 and (r.get("terminalStreamMarker") or r.get("finishReasons")) for r in streams)
                        row["status"] = "completed" if actual and success and transport_complete else "idle_without_completed_response"
                        row["terminalCheckpoints"] = terminals
                        break
                else:
                    quiet = None
            else:
                row["status"] = "harness_timeout"
        except Exception as error:
            row["status"] = "harness_error"
            row["error"] = f"{type(error).__name__}: {error}"
        row["elapsedSeconds"] = round(time.monotonic() - start, 3)
        row["completedAt"] = live.now()
        requests = [r for r in self.bridge.snapshot()["requests"] if r["turn"] == label]
        row["actualAnswer"] = actual_answer(requests)
        streams = [r for r in requests if r.get("stream")]
        row["requestSequences"] = [r["sequence"] for r in requests]
        row["providerFirstContentSeconds"] = min((r["startedMonotonic"] - start + r["firstContentSeconds"] for r in requests if r.get("firstContentSeconds") is not None), default=None)
        row["providerPerRequestMinContentSeconds"] = min((r["firstContentSeconds"] for r in requests if r.get("firstContentSeconds") is not None), default=None)
        row["providerEvidenceComplete"] = bool(streams) and all(r.get("responseId") and r.get("askedModel") == self.session.model and r.get("servedModel") and r.get("usage") for r in streams)
        row["exactPromptSeenUpstream"] = any(r.get("latestUserSha256") == row["promptSha256"] for r in requests)
        row["toolCalls"] = [c for r in requests for c in r.get("toolCalls", [])]
        row["toolReceipts"] = new_receipts(requests)
        row["workspaceFiles"] = live.workspace_inventory(self.workspace)
        row["artifactSnapshots"] = []
        if label in {"T033", "T049", "T073", "T089", "T101"}:
            for basename, suffix in [("guangzhou-trip.pptx", "presentation.pptx"), ("create_guangzhou_deck.go", "helper.go")]:
                source = self.workspace / basename
                if source.is_file():
                    target = self.evidence / (label + "-" + suffix)
                    shutil.copy2(source, target)
                    row["artifactSnapshots"].append({"file": target.name, "sourceBasename": basename, "bytes": target.stat().st_size,
                                                     "sha256": live.sha256_file(target), "origin": "copied unchanged from real CLI tool-created workspace output",
                                                     "requestSequences": row["requestSequences"], "toolCallIds": [c.get("id") for c in row["toolCalls"]]})
        row["physicalSessionIds"] = live.physical_session_ids(self.store)
        row["transportContentChecks"] = {"markerReturned": marker in row["actualAnswer"], "exactPromptSeenUpstream": row["exactPromptSeenUpstream"], "providerMetadataPresent": row["providerEvidenceComplete"]}
        row["acceptanceStatus"] = "pending-independent-review"
        row["screenshots"].append(self.session.capture(self.evidence, label + "-final"))
        raw = bytes(self.session.raw_bytes[raw_offset:])
        (self.evidence / (label + ".raw")).write_bytes(raw)
        row["rawSha256"] = live.sha256(raw)
        row["rawByteOffset"] = raw_offset
        row["rawByteLength"] = len(raw)
        (self.evidence / (label + "-answer.txt")).write_text(row["actualAnswer"], encoding="utf-8")
        self.save()
        print(json.dumps({"arm": self.name, "id": label, "status": row["status"], "seconds": row["elapsedSeconds"], "providerTTFT": row["providerFirstContentSeconds"], "approvals": len(row["approvals"])}, ensure_ascii=False), flush=True)
        return row

    def passive_finish(self, row, timeout):
        started = time.monotonic()
        started_at = live.now()
        while time.monotonic() - started < timeout:
            self.session.pump(.10)
            lines = self.session.screen_lines()
            exit_status = self.session.exit_status()
            if exit_status is not None:
                row["processExitStatus"] = exit_status
                result = "process_exit"
                break
            if live.idle_input_prompt(lines) and not live.active_busy_footer(lines):
                result = "natural_idle"
                break
        else:
            result = "passive_wait_timeout"
        row["controllerActions"].append({"action": "passive_wait_no_input", "startedAt": started_at, "durationSeconds": time.monotonic() - started, "result": result})
        return result == "natural_idle"

    def recover_idle(self):
        # A failure stays in the same persistent conversation; no invented success.
        if self.session.exit_status() is not None:
            return False
        if live.idle_input_prompt(self.session.screen_lines()):
            return True
        row = self.rows[-1]
        if self.passive_finish(row, 15):
            self.save()
            return True
        if self.session.exit_status() is not None:
            self.save()
            return False
        try:
            row["controllerActions"].append({"action": "escape_key", "keyHex": "1b", "sentAt": live.now(), "basis": "no natural idle after bounded passive wait"})
            self.session.send(b"\x1b")
            self.session.wait_until(live.idle_input_prompt, "idle after failed attempt", 15)
            self.save()
            return True
        except (TimeoutError, OSError):
            self.save()
            return False

    def close(self):
        self.metadata["sourceFilesAfter"] = [{"path": str(p.relative_to(self.repo)), "sha256": live.sha256_file(p)} for p in sorted(self.repo.rglob("*.go")) if "/.git/" not in str(p)]
        self.metadata["sourceUnchangedDuringRun"] = self.metadata["sourceFilesAfter"] == self.metadata["sourceFiles"]
        audit = live.query_magpie_audit(self.bridge.gateway, self.bridge.session_id)
        (self.root / "magpie-audit.json").write_text(json.dumps(audit, ensure_ascii=False, indent=2), encoding="utf-8")
        self.metadata["providerCorrelation"] = live.correlate_provider_evidence(self.bridge.snapshot(), audit)
        self.save(final=True)
        self.session.close()
        self.bridge.shutdown()
        self.bridge.server_close()

    def skip(self, turn, reason):
        self.rows.append({"id": turn["id"], "phase": turn.get("phase"), "prompt": turn["prompt"], "promptSha256": prompt_hash(turn["prompt"]),
                          "status": "not_run", "reason": reason, "elapsedSeconds": None, "actualAnswer": "", "screenshots": [], "approvals": [],
                          "providerFirstContentSeconds": None, "uiFirstContentSeconds": None})
        self.save()


def write_index(output, arms, manifest):
    review_path = output / "independent-task-review.json"
    review = json.loads(review_path.read_text(encoding="utf-8")) if review_path.exists() else {}
    summaries = {arm.name: summarize(arm.rows) for arm in arms}
    comparison = paired_speed(arms[0].rows, arms[1].rows) if len(arms) == 2 else {}
    manifest["summaries"], manifest["pairedSpeed"] = summaries, comparison
    (output / "manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
    summary = "# 广州持续对话真实测试\n\n本报告统计真实模型和真实CLI PTY，不把失败尝试称作完成持续对话。截图是xterm.js对真实PTY原始字节的无修饰渲染，非原生应用窗口截图。\n\n"
    for name, result in summaries.items():
        summary += f"- {name}：尝试 {result['attempted']} 轮，完成 {result['completed']} 轮，失败 {result['failed']} 轮，未执行 {result['notRun']} 轮；最长连续成功 {result['longestContinuousSuccess']} 轮；成功轮耗时中位数 {result['completedSecondsMedian']} 秒。\n"
    summary += "\n仅两边均完成的轮进入速度对比。" + json.dumps(comparison, ensure_ascii=False) + "\n\n[逐轮输入、回答、截图与审批证据](evidence-index.html)\n"
    (output / "summary.md").write_text(summary, encoding="utf-8")
    cards = []
    ids = list(dict.fromkeys(r["id"] for arm in arms for r in arm.rows))
    for label in ids:
        cells = []
        fail = False
        for arm in arms:
            row = next((r for r in arm.rows if r["id"] == label), None)
            if not row:
                cells.append("<section>尚未执行</section>")
                continue
            task_review = bound_task_review(review, arm.name, row)
            fail |= row["status"] not in ("completed", "not_run") or task_review["taskStatus"] == "FAIL"
            links = "".join(f'<a href="{arm.name}/evidence/{html.escape(s["file"])}"><img loading="lazy" src="{arm.name}/evidence/{html.escape(s["file"])}"></a>' for s in row["screenshots"])
            result_label = "未执行：原会话此前断裂" if row["status"] == "not_run" else ("对话完成" if row["status"] == "completed" else "FAIL：对话未完成")
            task_caption = f'独立任务审核 {task_review["taskStatus"]} · 内容 {task_review.get("semanticContentStatus", "未审")} · 格式 {task_review.get("formatStatus", "未审")}'
            cells.append(f'<section><h3>{arm.name} Git {arm.metadata["revision"][:8]} · {result_label}</h3><p>{html.escape(task_caption)}</p><p>{html.escape(task_review.get("reason", ""))}</p><p>{row["status"]} · {row["elapsedSeconds"]}s / 从提交到模型首文字 {row["providerFirstContentSeconds"]}s / UI首次文字 {row["uiFirstContentSeconds"]}s / 审批 {len(row["approvals"])}</p><pre>{html.escape(row["actualAnswer"])}</pre><details><summary>审批与逐轮原始数据</summary><pre>{html.escape(json.dumps(row, ensure_ascii=False, indent=2))}</pre></details>{links}</section>')
        prompt = next(r["prompt"] for arm in arms for r in arm.rows if r["id"] == label)
        cards.append(f'<article data-id="{label}" data-failed="{str(fail).lower()}"><h2>{label}</h2><pre>{html.escape(prompt)}</pre><div class="pair">{"".join(cells)}</div></article>')
    headline = " · ".join(f'{a.name} 对话完成 {summaries[a.name]["completed"]}/{manifest["plannedUserRoundsPerArm"]}' for a in arms)
    task_counts = " · ".join(f'{a.name} 严格任务 PASS {sum(bound_task_review(review, a.name, r)["taskStatus"] == "PASS" for r in a.rows)}/{len(a.rows)}' for a in arms) if review else "任务验收等待独立审核"
    page = '<!doctype html><meta charset="utf-8"><title>广州真实对话评测证据</title><style>body{font:16px system-ui;background:#eee;margin:24px}nav{position:sticky;top:0;background:white;padding:12px}.pair{display:grid;grid-template-columns:repeat(' + str(len(arms)) + ',minmax(0,1fr));gap:16px}article{background:white;padding:16px;margin:18px 0}img{width:100%;margin:8px 0}pre{white-space:pre-wrap;overflow-wrap:anywhere}section{min-width:0}details{max-height:500px;overflow:auto}</style><h1>广州持续对话：逐轮证据</h1><h2>' + headline + '</h2><p>' + task_counts + '</p><p>原始真实PTY通过xterm.js渲染；完整实际模型回答列在截图旁边。busy/content图为实际Bubbletea TUI，final图为待输入readline；错误截图可能仍在收尾，应看status/raw证据。截图Branch:main指会话分支；Git工作树身份由每列标题明确标注。对话完成、内容正确、格式合规、真实PPT/审批是独立判定，只有答案hash匹配才应用审核。</p><p>模型首文字/UI首次文字/首次idle从本轮提交开始计时；bridge逐request首文字从该request开始。elapsed额外包含约1秒idle稳定等待与审批时间，不等于纯生成时间。</p><nav><input id="q" placeholder="轮号 T001"><label><input id="f" type="checkbox">只看对话或任务失败</label> <a href="manifest.json">Manifest</a> <a href="independent-task-review.json">独立任务审核</a></nav>' + ''.join(cards) + '<script>function filter(){document.querySelectorAll("article").forEach(e=>e.hidden=!(e.dataset.id.toLowerCase().includes(q.value.toLowerCase())&&(!f.checked||e.dataset.failed==="true")))}q.oninput=f.onchange=filter</script>'
    (output / "evidence-index.html").write_text(page, encoding="utf-8")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--develop", required=True, type=Path)
    parser.add_argument("--baseline", required=True, type=Path)
    parser.add_argument("--prompts", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--fixtures", type=Path)
    parser.add_argument("--gateway", default=live.DEFAULT_GATEWAY)
    parser.add_argument("--model", default=live.MODEL)
    parser.add_argument("--max-tokens", type=int, default=128000)
    parser.add_argument("--model-timeout-total", default="30s")
    parser.add_argument("--model-timeout-idle", default="10s")
    parser.add_argument("--turn-timeout", type=float, default=150)
    parser.add_argument("--limit", type=int)
    parser.add_argument("--only", choices=["develop", "main"])
    parser.add_argument("--resume-from", type=Path, help="explicitly reuse this prior arm's workspace and durable store; new process/config/evidence phase")
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    harness_files = []
    for source in (Path(__file__), Path(live.__file__), live.RENDERER):
        target = args.output / (source.stem + "-at-start" + source.suffix)
        shutil.copy2(source, target)
        harness_files.append({"file": target.name, "sha256": live.sha256_file(target)})
    live.ensure_renderer_dependencies(live.locate_node())
    health = live.gateway_health(args.gateway, args.model)
    payload = json.loads(args.prompts.read_text(encoding="utf-8"))
    turns = payload if isinstance(payload, list) else payload["turns"]
    if args.limit:
        turns = turns[:args.limit]
    assert len({t["id"] for t in turns}) == len(turns), "duplicate turn IDs"
    frozen = json.dumps(turns, ensure_ascii=False, indent=2)
    frozen_bytes = args.prompts.read_bytes() if not args.limit else frozen.encode("utf-8")
    (args.output / "prompts.json").write_bytes(frozen_bytes)
    manifest = {"startedAt": live.now(), "model": args.model, "gatewayHealth": health, "promptsSha256": live.sha256(frozen_bytes),
                "sourcePromptsFileSha256": live.sha256_file(args.prompts), "harnessFiles": harness_files,
                "execution": "single-arm persistent session" if args.only else "paired singleflight AB/BA, one persistent isolated session per arm", "plannedUserRoundsPerArm": len(turns),
                "modelTimeout": {"total": args.model_timeout_total, "streamIdle": args.model_timeout_idle}, "screenshotsSource": "real CLI PTY bytes rendered by headless Chromium/xterm.js"}
    arms = []
    try:
        if args.only != "main":
            arms.append(Arm("develop", args.develop, args.output, args))
        if args.only != "develop":
            arms.append(Arm("main", args.baseline, args.output, args))
        for index, turn in enumerate(turns):
            ordered = arms if index % 2 == 0 else list(reversed(arms))
            for arm in ordered:
                if not arm.active:
                    arm.skip(turn, "The original process/session could not recover after a prior failed attempt. No silent restart.")
                    continue
                row = arm.turn(turn, "/".join(a.name for a in ordered), args.turn_timeout)
                write_index(args.output, arms, manifest)
                if row["status"] != "completed" and not arm.recover_idle():
                    arm.active = False
                    manifest.setdefault("brokenArms", {})[arm.name] = f"could not recover idle after {turn['id']}"
                # Drain late upstream completion before allowing the other arm to call the model.
                settlement = live.wait_for_bridge_quiescence(arm.bridge, timeout=40)
                if arm.bridge.inflight:
                    manifest["stoppedReason"] = f"{arm.name} upstream still inflight; singleflight integrity requires stopping"
                    return 2
            write_index(args.output, arms, manifest)
        manifest["finishedAt"] = live.now()
        return 0
    finally:
        for arm in arms:
            try:
                arm.close()
            except Exception as error:
                manifest.setdefault("cleanupErrors", {})[arm.name] = f"{type(error).__name__}: {error}"
                try:
                    arm.session.close()
                except Exception:
                    pass
                arm.bridge.shutdown()
                arm.bridge.server_close()
        write_index(args.output, arms, manifest)


if __name__ == "__main__":
    raise SystemExit(main())
