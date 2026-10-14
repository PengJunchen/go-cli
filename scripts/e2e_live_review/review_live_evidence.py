#!/usr/bin/env python3
"""Independently cross-check redacted live-run evidence against durable data.

The runner report is an index, not ground truth. This verifier rejoins each
completed local response to Magpie usage by response ID, checks route/session
identity and token accounting, and cross-checks durable compaction pointers
against the actual session JSONL. It never reads CLI configuration or request
bodies and never prints authorization material.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
from typing import Any


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def read_jsonl(path: Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    if not path.is_file():
        return rows
    with path.open("r", encoding="utf-8") as source:
        for line in source:
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(row, dict):
                rows.append(row)
    return rows


def add_check(checks: list[dict[str, Any]], name: str, passed: bool, **evidence: Any) -> None:
    checks.append({"name": name, "passed": bool(passed), **evidence})


def int_value(value: Any) -> int | None:
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def review_provider(report: dict[str, Any], checks: list[dict[str, Any]]) -> dict[str, Any]:
    provider = report.get("providerEvidence", {})
    audit = report.get("magpieAudit", {})
    inference = provider.get("inference", [])
    usage = audit.get("usage", [])
    usage_by_response: dict[str, dict[str, Any]] = {}
    duplicate_usage_responses: list[str] = []
    for row in usage:
        response_id = row.get("response_id")
        if not isinstance(response_id, str) or not response_id:
            duplicate_usage_responses.append("<missing>")
        elif response_id in usage_by_response:
            duplicate_usage_responses.append(response_id)
        else:
            usage_by_response[response_id] = row

    seen_responses: set[str] = set()
    seen_routes: set[str] = set()
    seen_sequences: set[int] = set()
    joined_rows: list[dict[str, Any]] = []
    problems: list[dict[str, Any]] = []
    interactive = 0
    internal_nonstream = 0

    for row in inference:
        seq = int_value(row.get("sequence"))
        response_id = row.get("responseId")
        usage_row = usage_by_response.get(response_id) if isinstance(response_id, str) else None
        route = row.get("routeIdentity") or {}
        usage_identity = row.get("usageIdentity") or {}
        reconciliation = row.get("usageReconciliation") or {}
        route_id = route.get("routeId")
        checks_for_row = {
            "response_id_present": isinstance(response_id, str) and bool(response_id),
            "response_id_unique": isinstance(response_id, str) and response_id not in seen_responses,
            "sequence_unique": seq is not None and seq not in seen_sequences,
            "usage_row_joined_by_response_id": usage_row is not None,
            "route_snapshot_matched": (
                row.get("routeJoin") == "per-request-session-route-snapshot"
                and route.get("status") == "matched"
                and route.get("sessionMatches") is True
            ),
            "usage_identity_matched": (
                row.get("usageJoin") == "exact-response_id+route_id+session"
                and usage_identity.get("status") == "matched"
                and not usage_identity.get("mismatchedFields")
            ),
        }
        if usage_row is not None:
            usage_route_id = usage_row.get("route_id")
            usage_session = usage_row.get("session")
            asked_model = str(row.get("askedModel", ""))
            served_model = str(row.get("servedModel", ""))
            expected_magpie_model = asked_model.removeprefix("codex/")
            uncached_input = int_value(usage_row.get("in"))
            cached_input = int_value(usage_row.get("cache_read"))
            output_tokens = int_value(usage_row.get("out"))
            upstream_input = int_value(reconciliation.get("upstreamInputTokens"))
            upstream_output = int_value(reconciliation.get("upstreamOutputTokens"))
            uncached_reported = int_value(reconciliation.get("magpieUncachedInputTokens"))
            cached_reported = int_value(reconciliation.get("magpieCachedInputTokens"))
            output_reported = int_value(reconciliation.get("magpieOutputTokens"))
            checks_for_row.update({
                "route_id_matches_usage": str(route_id) == str(usage_route_id),
                "session_matches_run": usage_session == audit.get("session"),
                "provider_and_model_match": (
                    usage_row.get("provider") == "codex"
                    and expected_magpie_model == usage_row.get("model")
                    and asked_model == served_model
                ),
                "upstream_status_success": row.get("status") == 200 and usage_row.get("status") == 200,
                "uncached_input_matches": uncached_input is not None and uncached_input == uncached_reported,
                "cached_input_matches": cached_input is not None and cached_input == cached_reported,
                "prompt_tokens_equal_uncached_plus_cached": (
                    upstream_input is not None
                    and uncached_input is not None
                    and cached_input is not None
                    and upstream_input == uncached_input + cached_input
                ),
                "completion_tokens_match": (
                    output_tokens is not None and output_tokens == upstream_output == output_reported
                ),
            })
        kind = row.get("requestKind")
        if kind == "interactive-user-turn":
            interactive += 1
            expected_markers = row.get("expectedInputMarkers") or []
            markers = row.get("inputMarkerMatches") or []
            if expected_markers:
                checks_for_row["current_user_marker_present"] = all(marker in markers for marker in expected_markers)
            checks_for_row["stream_completed"] = (
                row.get("stream") is True and row.get("terminalStreamMarker") is True
            )
        elif kind == "internal-nonstream":
            internal_nonstream += 1
            checks_for_row["nonstream_json_completed"] = (
                row.get("stream") is not True
                and row.get("responseJsonParsed") is True
                and row.get("responseMetadataError") in (None, "")
            )
        else:
            checks_for_row["request_classification_recognized"] = False

        failed = [name for name, passed in checks_for_row.items() if not passed]
        if failed:
            problems.append({"sequence": seq, "responseId": response_id, "failed": failed})
        joined_rows.append({
            "sequence": seq,
            "requestKind": kind,
            "turn": row.get("turn"),
            "responseId": response_id,
            "routeId": route_id,
            "provider": usage_row.get("provider") if usage_row else None,
            "model": row.get("servedModel"),
            "status": row.get("status"),
            "stream": row.get("stream"),
            "terminalStreamMarker": row.get("terminalStreamMarker"),
            "inputTokens": int_value(reconciliation.get("upstreamInputTokens")),
            "cachedInputTokens": int_value(reconciliation.get("magpieCachedInputTokens")),
            "outputTokens": int_value(reconciliation.get("upstreamOutputTokens")),
            "toolCallCount": len(row.get("toolCalls") or []),
            "checks": checks_for_row,
        })
        if isinstance(response_id, str):
            seen_responses.add(response_id)
        if route_id is not None:
            seen_routes.add(str(route_id))
        if seq is not None:
            seen_sequences.add(seq)

    add_check(checks, "all inference rows have unique response/route/sequence identities and exact Magpie joins", not problems,
              inferenceRows=len(inference), magpieUsageRows=len(usage), uniqueResponseIds=len(seen_responses),
              uniqueRouteIds=len(seen_routes), uniqueSequences=len(seen_sequences), problems=problems)
    add_check(checks, "interactive and internal provider completions are separately classified", len(inference) == len(usage)
              and interactive + internal_nonstream == len(inference), interactiveUserTurns=interactive,
              internalNonstream=internal_nonstream)
    return {"rows": joined_rows, "interactiveUserTurns": interactive, "internalNonstream": internal_nonstream,
            "uniqueResponseIds": len(seen_responses), "uniqueRouteIds": len(seen_routes), "problems": problems}


def review_compaction(report: dict[str, Any], checks: list[dict[str, Any]]) -> dict[str, Any]:
    context = report.get("contextCompaction") or {}
    evidence = report.get("largeContextReadEvidence") or {}
    read_facts = evidence.get("fixture") or {}
    durable_new = context.get("newDurableEntries") or []
    if not durable_new:
        add_check(checks, "production typed summary persisted and covers the successful large Read receipt", False,
                  reason="no new durable summary entry was reported")
        return {"status": "missing"}
    entry_meta = durable_new[-1]
    store_root = Path(str(report.get("isolatedConfig", {}).get("sessionStore", "")))
    session_file = store_root / "sessions" / str(entry_meta.get("file", ""))
    durable_rows = read_jsonl(session_file)
    summary_row = next((r for r in durable_rows if r.get("id") == entry_meta.get("id")), None)
    read_entry_id = (evidence.get("durableReceipts") or [{}])[0].get("receiptEntryId")
    read_seq = (evidence.get("durableReceipts") or [{}])[0].get("receiptSeq")
    read_row = next((r for r in durable_rows if r.get("id") == read_entry_id), None)
    summary = str(summary_row.get("content", "")) if summary_row else ""
    read_content = str(read_row.get("content", "")) if read_row else ""
    nonce = str(report.get("nonce", ""))
    first_marker = str(read_facts.get("firstMarker", ""))
    last_marker = str(read_facts.get("lastMarker", ""))
    source_path = Path(str(read_facts.get("path", "")))
    source_bytes = source_path.read_bytes() if source_path.is_file() else b""
    compaction_trace = next((
        span for span in context.get("newTraceSpans", [])
        if span.get("name") == "compaction"
    ), {})
    trace_attributes = compaction_trace.get("attributes", {})
    try:
        current_tokens = int(trace_attributes.get("current_tokens", 0))
        max_tokens = int(trace_attributes.get("max_tokens", 0))
        items_count = int(trace_attributes.get("items_count", 0))
        compacted_items = int(trace_attributes.get("compacted_items", 0))
        compacted_tokens = int(trace_attributes.get("compacted_tokens", 0))
    except (TypeError, ValueError):
        current_tokens = max_tokens = items_count = compacted_items = compacted_tokens = 0
    unified_summary_done = any(
        event.get("event") == "compaction.unified.done"
        and (event.get("fields") or {}).get("strategy") == "summary"
        for event in context.get("newUnifiedLogEvents", [])
    )
    check_values = {
        "actual_compaction_row_persisted": summary_row is not None and summary_row.get("type") == "compaction",
        "summary_content_hash_matches": summary_row is not None
        and sha256_bytes(summary.encode()) == entry_meta.get("summarySha256"),
        "summary_preserves_early_canary": f"LIVE_MEMORY_{nonce.upper()}=cobalt" in summary,
        "summary_preserves_both_large_read_boundaries": bool(first_marker and last_marker)
        and first_marker in summary and last_marker in summary,
        "same_summary_cut_reaches_successful_read_receipt": summary_row is not None and read_row is not None
        and summary_row.get("compacted_through_entry_id") == read_entry_id
        and read_row.get("seq") == read_seq,
        "read_receipt_matches_source_bytes": bool(source_bytes) and read_content.encode() == source_bytes
        and sha256_bytes(source_bytes) == read_facts.get("sha256"),
        "production_summary_trace_present": trace_attributes.get("strategy_used") == "summary"
        and unified_summary_done and current_tokens > max_tokens > 0
        and items_count > 0 and 0 < compacted_items <= items_count and compacted_tokens > 0,
        "post_compaction_request_contains_latest_prompt": context.get("postCompactionProviderRequestContainsLatestPrompt") is True,
        "post_compaction_request_contains_canary": context.get("postCompactionProviderRequestContainsMemoryCanary") is True,
        "later_answer_recalled_exact_fact": context.get("recalledMemoryFact") is True,
    }
    for name, passed in check_values.items():
        add_check(checks, name, passed,
                  summaryEntryId=entry_meta.get("id") if name.startswith(("actual_compaction", "summary_content", "summary_preserves", "same_summary")) else None,
                  readReceiptEntryId=read_entry_id if name in ("same_summary_cut_reaches_successful_read_receipt", "read_receipt_matches_source_bytes") else None)
    return {
        "status": "pass" if all(check_values.values()) else "fail",
        "summaryEntry": {"id": entry_meta.get("id"), "seq": entry_meta.get("seq"),
                         "summarySha256": entry_meta.get("summarySha256"),
                         "compactedThroughEntryId": summary_row.get("compacted_through_entry_id") if summary_row else None},
        "readReceipt": {"id": read_entry_id, "seq": read_seq, "sourceBytes": len(source_bytes),
                        "sourceSha256": sha256_bytes(source_bytes) if source_bytes else None,
                        "sourceFirstAndLastMarkers": bool(first_marker and last_marker and first_marker in read_content and last_marker in read_content)},
        "trace": {key: trace_attributes.get(key) for key in ("strategy_used", "current_tokens", "max_tokens", "items_count", "compacted_items", "compacted_tokens")},
        "checks": check_values,
    }


def review_recovery(report: dict[str, Any], checks: list[dict[str, Any]]) -> dict[str, Any]:
    recovery = report.get("escapeCancellation") or {}
    fresh = recovery.get("freshTurn") or {}
    new_session_id = recovery.get("newPhysicalSessionId")
    session_store = Path(str(report.get("isolatedConfig", {}).get("sessionStore", "")))
    expected_file = session_store / "sessions" / f"{new_session_id}.jsonl" if new_session_id else None
    fresh_rows = fresh.get("durableUserRows") or []
    fresh_terminal = fresh.get("terminalRuns") or []
    has_successful_terminal = any(row.get("run_outcome") == "success" for row in fresh_terminal)
    checks_for_recovery = {
        "escape_left_unknown_outcome_and_quiescent_owner": recovery.get("unknownOutcomeBeforeNewSession") is True
        and recovery.get("quiescentBeforeNewSession") is True,
        "ordinary_prompt_remained_fenced_without_provider_request": bool(recovery.get("fencedPrompt", {}).get("noticeSeen"))
        and not recovery.get("fencedPrompt", {}).get("providerRequestSequencesAfterPrompt")
        and not recovery.get("fencedPrompt", {}).get("durableUserRows"),
        "new_physical_session_id_differs_and_file_exists": bool(new_session_id)
        and new_session_id != recovery.get("oldPhysicalSessionId")
        and expected_file is not None and expected_file.is_file(),
        "new_session_has_successful_fresh_provider_turn": bool(fresh.get("providerResponseSeen"))
        and bool(fresh_rows) and has_successful_terminal,
        "fresh_user_row_uses_new_physical_session": bool(fresh_rows)
        and all(row.get("physicalSessionId") == new_session_id for row in fresh_rows),
        "old_unknown_state_unchanged_after_new_session": recovery.get("oldUnknownOutcomeUnchangedByNewSession") is True,
        "old_unknown_state_unchanged_after_fresh_turn": bool(fresh)
        and recovery.get("oldUnknownOutcomeUnchangedAfterFreshTurn") is True,
    }
    for name, passed in checks_for_recovery.items():
        add_check(checks, name, passed)
    return {"newPhysicalSessionId": new_session_id,
            "newSessionFileExists": expected_file.is_file() if expected_file else False,
            "freshTurnPresent": bool(fresh), "checks": checks_for_recovery}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--report", required=True, type=Path)
    parser.add_argument("--out", type=Path)
    args = parser.parse_args()
    report = json.loads(args.report.read_text(encoding="utf-8"))
    checks: list[dict[str, Any]] = []
    provider = review_provider(report, checks)
    compaction = review_compaction(report, checks)
    recovery = review_recovery(report, checks)
    harness_failed = [row.get("name") for row in report.get("checks", []) if not row.get("passed")]
    add_check(checks, "runner reported success with no failed acceptance checks",
              report.get("status") == "passed" and report.get("cliExitStatus") == 0 and not harness_failed,
              runnerStatus=report.get("status"), cliExitStatus=report.get("cliExitStatus"),
              failedAcceptanceChecks=harness_failed)
    result = {
        "schemaVersion": 1,
        "runId": report.get("runId"),
        "revision": report.get("revision"),
        "sourceCommit": report.get("revision"),
        "binarySha256": report.get("raceBuild", {}).get("sha256"),
        "reportSha256": sha256_bytes(args.report.read_bytes()),
        "runStatus": report.get("status"),
        "provider": provider,
        "compaction": compaction,
        "recovery": recovery,
        "checks": checks,
        "status": "pass" if all(row.get("passed") for row in checks) else "fail",
    }
    rendered = json.dumps(result, indent=2, ensure_ascii=False) + "\n"
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(rendered, encoding="utf-8")
    if args.out is None:
        print(json.dumps({
            "runId": result["runId"],
            "status": result["status"],
            "providerRows": len(provider["rows"]),
            "providerProblems": len(provider["problems"]),
            "compactionStatus": compaction.get("status"),
            "recoveryChecks": recovery.get("checks"),
            "failedChecks": [check["name"] for check in checks if not check.get("passed")],
        }, indent=2, ensure_ascii=False))
    return 0 if result["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
