#!/usr/bin/env python3
"""Recompute the independent, credential-free evidence ledger for run 193912."""

from __future__ import annotations

import collections
import hashlib
import json
import re
import zipfile
from pathlib import Path, PurePosixPath
from xml.etree import ElementTree


ROOT = Path(__file__).resolve().parents[5]
RUN_ID = "20261014-193912-aafaafae"
RUN_DIR = ROOT / "docs/qa/2026-10-14/live/terminal" / RUN_ID
WORK_ROOT = ROOT / "docs/qa/2026-10-14/live/workspaces" / RUN_ID
WORK = WORK_ROOT / "workspace"
SESSION_DIR = WORK_ROOT / "session-store/sessions"
REVIEW_DIR = ROOT / "docs/qa/2026-10-14/live/review/193912-independent"
OUT = REVIEW_DIR / "live-evidence-review.json"
SOURCE_COMMIT = "bdb76efb0b31152e4c554268b43d39ef6285115a"
ALPHA = "LIVE_FACT_ALPHA_3A6527E73296=blue"
BETA = "LIVE_FACT_BETA_3A6527E73296=green"
CANARY = "LIVE_MEMORY_3A6527E73296=cobalt"
FIRST = "LIVE_CONTEXT_FIRST_3A6527E73296=start"
LAST = "LIVE_CONTEXT_LAST_3A6527E73296=end"


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha_file(path: Path) -> str:
    return sha256(path.read_bytes())


def check(name: str, observed: object, expected: object) -> dict[str, object]:
    return {"name": name, "observed": observed, "expected": expected, "passed": observed == expected}


def slide_texts(path: Path) -> tuple[list[list[str]], dict[str, object]]:
    ns = {
        "p": "http://schemas.openxmlformats.org/presentationml/2006/main",
        "a": "http://schemas.openxmlformats.org/drawingml/2006/main",
        "r": "http://schemas.openxmlformats.org/officeDocument/2006/relationships",
        "rel": "http://schemas.openxmlformats.org/package/2006/relationships",
        "ct": "http://schemas.openxmlformats.org/package/2006/content-types",
    }
    with zipfile.ZipFile(path) as package:
        corrupt = package.testzip()
        names = set(package.namelist())
        types = ElementTree.fromstring(package.read("[Content_Types].xml"))
        main_types = [
            entry.get("ContentType")
            for entry in types.findall("ct:Override", ns)
            if entry.get("PartName") == "/ppt/presentation.xml"
        ]
        presentation = ElementTree.fromstring(package.read("ppt/presentation.xml"))
        rels = ElementTree.fromstring(package.read("ppt/_rels/presentation.xml.rels"))
        rel_targets = {entry.get("Id"): entry.get("Target") for entry in rels.findall("rel:Relationship", ns)}
        references = [
            slide.get(f"{{{ns['r']}}}id")
            for slide in presentation.findall(".//p:sldId", ns)
        ]
        resolved = []
        for ref in references:
            target = rel_targets.get(ref)
            if target is None:
                resolved.append(False)
                continue
            part = PurePosixPath("ppt") / PurePosixPath(target)
            normalized = []
            for segment in part.parts:
                if segment == "..":
                    if normalized:
                        normalized.pop()
                elif segment != ".":
                    normalized.append(segment)
            resolved.append("/".join(normalized) in names)
        parts = sorted(
            name for name in names if re.fullmatch(r"ppt/slides/slide\d+\.xml", name)
        )
        slide_rows = []
        for part in sorted(parts, key=lambda item: int(re.search(r"slide(\d+)", item).group(1))):
            root = ElementTree.fromstring(package.read(part))
            text = [node.text or "" for node in root.findall(".//a:t", ns)]
            text_shapes = sum(bool(node.findall(".//a:t", ns)) for node in root.findall(".//p:sp", ns))
            picture_shapes = len(root.findall(".//p:pic", ns))
            slide_rows.append({"part": part, "text": text, "editable_text_shapes": text_shapes, "picture_shapes": picture_shapes})
        structure = {
            "zip_integrity": corrupt is None,
            "package_parts": len(names),
            "canonical_presentation_content_type": main_types == ["application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"],
            "presentation_slide_relationships_resolve": bool(references) and all(resolved),
            "presentation_slide_count": len(references),
            "slide_parts": len(parts),
            "editable_text_shapes": sum(row["editable_text_shapes"] for row in slide_rows),
            "picture_shapes": sum(row["picture_shapes"] for row in slide_rows),
        }
    return [row["text"] for row in slide_rows], {**structure, "slides": slide_rows}


def main() -> None:
    report_path = RUN_DIR / "run-report.json"
    report = json.loads(report_path.read_text())
    inference = report["providerEvidence"]["inference"]
    usage_rows = report["magpieAudit"]["usage"]
    by_response: dict[str, list[dict[str, object]]] = collections.defaultdict(list)
    for row in usage_rows:
        by_response[str(row.get("response_id"))].append(row)

    provider_rows = []
    row_checks = []
    for item in inference:
        route = item.get("route") or {}
        gateway_usage = item.get("gatewayUsage") or {}
        reconciliation = item.get("usageReconciliation") or {}
        matches = by_response.get(str(item.get("responseId")), [])
        valid = {
            "one_usage_row_for_response": len(matches) == 1,
            "response_route_ids_match": len(matches) == 1
            and str(matches[0].get("route_id")) == str(route.get("id")) == str(gateway_usage.get("route_id")),
            "session_ids_match": len(matches) == 1
            and matches[0].get("session") == route.get("session") == report["magpieAudit"]["session"],
            "provider_and_model_match": item.get("provider") == "codex"
            and str(item.get("askedModel", "")).split("/")[-1]
            == str(item.get("servedModel", "")).split("/")[-1]
            == str(route.get("served", ""))
            == str(gateway_usage.get("model", "")),
            "http_200": item.get("status") == route.get("status") == gateway_usage.get("status") == 200,
            "completed_response_json": item.get("responseJsonParsed") is True and not item.get("responseMetadataError"),
            "usage_reconciles": reconciliation.get("status") == "matched"
            and reconciliation.get("inputMatches") is True
            and reconciliation.get("outputMatches") is True,
            "finish_reason_present": bool(item.get("finishReasons")),
            "stream_end_detected": item.get("stream") is not True or bool(item.get("terminalStreamMarker")),
        }
        row_checks.append({"sequence": item["sequence"], "checks": valid})
        provider_rows.append(
            {
                "sequence": item["sequence"],
                "turn": item.get("turn"),
                "request_kind": item.get("requestKind"),
                "input_markers_expected": item.get("expectedInputMarkers", []),
                "input_markers_matched": item.get("inputMarkerMatches", []),
                "provider": item.get("provider"),
                "asked_model": item.get("askedModel"),
                "served_model": item.get("servedModel"),
                "http_status": item.get("status"),
                "stream": item.get("stream"),
                "finish_reasons": item.get("finishReasons"),
                "terminal_stream_marker_detected": bool(item.get("terminalStreamMarker")) if item.get("stream") is True else None,
                "response_id": item.get("responseId"),
                "upstream_request_id": item.get("requestId"),
                "response_bytes": item.get("responseBytes"),
                "response_sha256": item.get("responseSha256"),
                "response_json_parsed": item.get("responseJsonParsed"),
                "route_id": route.get("id"),
                "route_session": route.get("session"),
                "route_state_sequence": route.get("routeStateSequence"),
                "capture_bridge_sequence_local_only": route.get("captureBridgeSequence"),
                "gateway_usage": {
                    key: gateway_usage.get(key)
                    for key in ("response_id", "route_id", "session", "provider", "req", "model", "status", "in", "out", "cache_read", "cache_write")
                },
                "usage_reconciliation": reconciliation,
                "tool_calls": [
                    {"id": call.get("id"), "name": call.get("name"), "arguments_sha256": call.get("argumentsSha256")}
                    for call in item.get("toolCalls", [])
                ],
                "tool_receipts": [
                    {
                        "tool_call_id": receipt.get("toolCallId"),
                        "tool": receipt.get("tool"),
                        "bytes": receipt.get("bytes"),
                        "sha256": receipt.get("sha256"),
                        "mentions_error": receipt.get("mentionsError"),
                        "mentions_policy_blocked": receipt.get("mentionsPolicyBlocked"),
                    }
                    for receipt in item.get("toolReceipts", [])
                ],
                "identity_checks": valid,
            }
        )

    failed_provider_checks = [
        {"sequence": row["sequence"], "failed": [name for name, passed in row["checks"].items() if not passed]}
        for row in row_checks
        if not all(row["checks"].values())
    ]
    old_id = report["escapeCancellation"]["oldPhysicalSessionId"]
    session_path = SESSION_DIR / f"{old_id}.jsonl"
    old_rows = [json.loads(line) for line in session_path.read_text().splitlines() if line.strip()]
    provider_call_ids = [str(call.get("id")) for item in inference for call in item.get("toolCalls", [])]
    provider_calls: dict[str, dict[str, object]] = {}
    for item in inference:
        for call in item.get("toolCalls", []):
            provider_calls[str(call.get("id"))] = {"id": call.get("id"), "name": call.get("name"), "arguments_sha256": call.get("argumentsSha256")}
    durable_calls = []
    durable_call_checks = []
    for call_id, call in provider_calls.items():
        entries = [row for row in old_rows if row.get("tool_call_id") == call_id and row.get("type") == "tool"]
        receipts = [row for row in entries if "receipt_outcome" in row]
        matching_names = [row for row in entries if row.get("tool_name") == call.get("name")]
        check_row = {
            "provider_tool_call_has_durable_action": bool(entries),
            "provider_tool_name_matches_durable_action": bool(matching_names),
            "provider_tool_call_has_durable_receipt": bool(receipts),
        }
        durable_call_checks.append({"tool_call_id": call_id, "checks": check_row})
        durable_calls.append({
            **call,
            "durable_entry_ids": [row.get("id") for row in entries],
            "durable_receipts": [{"entry_id": row.get("id"), "sequence": row.get("seq"), "outcome": row.get("receipt_outcome"), "resource_state": row.get("resource_state"), "completion_proof": row.get("completion_proof"), "bytes": len(row.get("content", "").encode()), "content_sha256": sha256(row.get("content", "").encode())} for row in receipts],
        })
    source_receipt = next(row for row in old_rows if row.get("seq") == report["largeContextReadEvidence"]["durableReceipts"][0]["receiptSeq"])
    summary = next(row for row in old_rows if row.get("seq") == report["contextCompaction"]["newDurableEntries"][0]["seq"])
    summary_text = summary.get("summary", "")
    cancellation = report["escapeCancellation"]
    new_id = cancellation["newPhysicalSessionId"]
    new_session_path = SESSION_DIR / f"{new_id}.jsonl"
    new_rows = [json.loads(line) for line in new_session_path.read_text().splitlines() if line.strip()]

    nonce = report["nonce"]
    lower_nonce = nonce.lower()
    expected_deck = WORK / f"live_review_{nonce}.pptx"
    before = RUN_DIR / "presentation-before.pptx"
    after = RUN_DIR / "presentation-after.pptx"
    before_text, before_structure = slide_texts(before)
    after_text, after_structure = slide_texts(after)
    with zipfile.ZipFile(before) as before_zip, zipfile.ZipFile(after) as after_zip:
        before_parts = set(before_zip.namelist())
        after_parts = set(after_zip.namelist())
        added_parts = sorted(after_parts - before_parts)
        removed_parts = sorted(before_parts - after_parts)
        changed_parts = sorted(part for part in before_parts & after_parts if before_zip.read(part) != after_zip.read(part))

    report_checks = report.get("checks", [])
    approval_summary = [
        {
            "turn": row.get("turn"),
            "decision": row.get("decision"),
            "tool_name": row.get("toolName"),
            "tool_call_ids": row.get("toolCallIds", []),
            "reviewed_rows": row.get("reviewedRows"),
            "detail_rows": row.get("detailRows"),
            "all_details_reviewed": row.get("detailRowsFullyCovered"),
            "screenshots": row.get("pageScreenshots", []),
        }
        for row in report.get("approvals", [])
    ]
    turns = [
        {
            "turn": row.get("turn"),
            "provider_sequences": row.get("providerRequestSequences", []),
            "response_ids": row.get("upstreamResponseIds", []),
            "tool_call_receipt_matches": row.get("toolCallReceiptMatches", []),
        }
        for row in report.get("turns", [])
    ]
    summary_sequence = next(row["sequence"] for row in provider_rows if row["request_kind"] == "internal-nonstream")
    recall_sequence = next(row["sequence"] for row in provider_rows if row["turn"] == "post-compaction-recall")

    png_paths = sorted(RUN_DIR.glob("*.png"))
    selected_screens = (
        "inspect-fixtures-1-approval-allow-page-1.png",
        "03-deny-approval-deny-page-1.png",
        "06-long-bash-running.png",
        "08-long-bash-followup-complete.png",
        "10-escape-sent.png",
        "12-cancel-fenced-prompt-warning.png",
        "13-session-new-recovery.png",
        "14-after-cancel-fresh-session.png",
        "19-context-compaction-boundary.png",
        "21-post-compaction-recall.png",
    )
    screenshot_hashes = {name: sha_file(RUN_DIR / name) for name in selected_screens if (RUN_DIR / name).is_file()}
    independent_pptx_path = REVIEW_DIR / "pptx-review.json"
    independent_pptx = json.loads(independent_pptx_path.read_text())
    rendered_paths = [Path(path) for path in independent_pptx.get("render", {}).get("pngs", [])]

    deck_review_checks = [
        check("before OOXML archive is valid", before_structure["zip_integrity"], True),
        check("after OOXML archive is valid", after_structure["zip_integrity"], True),
        check("before has canonical main presentation content type", before_structure["canonical_presentation_content_type"], True),
        check("after has canonical main presentation content type", after_structure["canonical_presentation_content_type"], True),
        check("both decks have four resolvable slides", (before_structure["presentation_slide_count"], after_structure["presentation_slide_count"], before_structure["presentation_slide_relationships_resolve"], after_structure["presentation_slide_relationships_resolve"]), (4, 4, True, True)),
        check("both decks contain editable text and no slide pictures", (before_structure["editable_text_shapes"], after_structure["editable_text_shapes"], before_structure["picture_shapes"], after_structure["picture_shapes"]), (13, 13, 0, 0)),
        check("slide payload change is confined to slide 4", (added_parts, removed_parts, changed_parts), ([], [], ["ppt/slides/slide4.xml"])),
        check("v1 has exact expected title and status", (before_text[0][0], before_text[3][1]), (f"LIVE_REVIEW_{nonce.upper()}", "Release status: INITIAL")),
        check("v2 preserves required text and revises only status", (after_text[0][0], after_text[1][1], after_text[2][1:3], after_text[3][1]), (f"LIVE_REVIEW_{nonce.upper()}", "Synthetic checks completed: 53 of 100", ["Synthetic median tool cycle: 3.1 seconds", "Synthetic review facts retained: 7 of 10"], "Release status: REVISED")),
        check("disclosure appears on every slide in both versions", (all("Synthetic QA inputs only - not product measurements" in slide for slide in before_text), all("Synthetic QA inputs only - not product measurements" in slide for slide in after_text)), (True, True)),
        check("workspace deck is byte-identical to frozen v2", sha_file(expected_deck), sha_file(after)),
        check("independent PPTX verifier passed", independent_pptx.get("status"), "pass"),
        check("four independently rendered images are present", len(rendered_paths), 4),
    ]

    report_failed_checks = [row.get("name") for row in report_checks if not row.get("passed")]
    source_diff_sha = hashlib.sha256(b"").hexdigest()
    summary_source_match = summary.get("compacted_through_entry_id") == source_receipt.get("id")
    context_text = source_receipt.get("content", "")
    source_commit_actual = __import__("subprocess").run(
        ["git", "-C", str(ROOT), "rev-parse", "HEAD"], capture_output=True, text=True, check=True
    ).stdout.strip()
    run_checks = [
        check("current checkout is the pinned source revision", source_commit_actual, SOURCE_COMMIT),
        check("runner recorded all acceptance checks passed", (report.get("status"), len(report_checks), len(report_checks) - len(report_failed_checks)), ("passed", 194, 194)),
        check("CLI exited successfully", report.get("cliExitStatus"), 0),
        check("race detector was clean", report.get("raceDetectorClean"), True),
        check("source stayed unchanged during build", report.get("goSourceUnchangedDuringBuild"), True),
        check("build-time source patch snapshots are empty", ((report["goSourceSnapshot"]["staged"]["bytes"], report["goSourceSnapshot"]["staged"]["sha256"]), (report["goSourceSnapshot"]["unstaged"]["bytes"], report["goSourceSnapshot"]["unstaged"]["sha256"])), ((0, source_diff_sha), (0, source_diff_sha))),
        check("all provider/route/session/usage identity checks match", failed_provider_checks, []),
        check("provider tool-call IDs are unique", len(provider_call_ids), len(set(provider_call_ids))),
        check("provider tool calls have name-matched durable actions and receipts", [row for row in durable_call_checks if not all(row["checks"].values())], []),
        check("the actual durable summary directly covers the successful read receipt", summary_source_match, True),
        check("the exact canary and source boundary markers remain in the summary", (CANARY in summary_text, "LIVE_CONTEXT_FIRST_3A6527E73296" in summary_text, "LIVE_CONTEXT_LAST_3A6527E73296" in summary_text), (True, True, True)),
        check("new physical session differs from old and contains its successful durable fresh turn", (new_id != old_id, any(row.get("type") == "user" for row in new_rows), any(row.get("run_outcome") == "success" for row in new_rows)), (True, True, True)),
        check("old unknown result stayed unchanged after new-session use", cancellation.get("oldUnknownOutcomeUnchangedAfterFreshTurn"), True),
    ]

    ledger = {
        "schema_version": 1,
        "run_id": RUN_ID,
        "run_report": {
            "path": str(report_path.relative_to(ROOT)),
            "sha256": sha_file(report_path),
            "reported_status": report.get("status"),
            "reported_check_count": len(report_checks),
            "reported_passed_count": len(report_checks) - len(report_failed_checks),
            "reported_failed_check_names": report_failed_checks,
            "cli_exit_status": report.get("cliExitStatus"),
            "race_detector_clean": report.get("raceDetectorClean"),
            "pty_png_count": len(png_paths),
            "selected_pty_screenshot_sha256": screenshot_hashes,
        },
        "source": {
            "commit_expected_and_observed": source_commit_actual,
            "race_binary_path": str(Path(report["raceBuild"]["binary"]).relative_to(ROOT)),
            "race_binary_sha256": report["raceBuild"].get("sha256"),
            "build_flags": report["raceBuild"].get("flags"),
            "source_unchanged_during_build": report.get("goSourceUnchangedDuringBuild"),
            "staged_source_diff_bytes_and_sha256": [report["goSourceSnapshot"]["staged"].get("bytes"), report["goSourceSnapshot"]["staged"].get("sha256")],
            "unstaged_source_diff_bytes_and_sha256": [report["goSourceSnapshot"]["unstaged"].get("bytes"), report["goSourceSnapshot"]["unstaged"].get("sha256")],
        },
        "runner_claimed_checks": {"checks": [{"name": row.get("name"), "passed": row.get("passed")} for row in report_checks], "failed_names": report_failed_checks},
        "independent_run_checks": run_checks,
        "independent_pptx_checks": deck_review_checks,
        "provider_review": {
            "completed_response_count": len(provider_rows),
            "interactive_stream_count": sum(1 for row in provider_rows if row["request_kind"] == "interactive-user-turn" and row["stream"] is True),
            "internal_nonstream_count": sum(1 for row in provider_rows if row["request_kind"] == "internal-nonstream" and row["stream"] is None),
            "response_ids_unique": len({row["response_id"] for row in provider_rows}) == len(provider_rows),
            "usage_rows_count": len(usage_rows),
            "usage_response_ids_unique": len({row.get("response_id") for row in usage_rows}) == len(usage_rows),
            "identity_mismatches": failed_provider_checks,
            "magpie_session": report["magpieAudit"].get("session"),
            "bridge_session": report["bridge"].get("sessionId"),
            "request_id_note": "No separate upstream request ID was supplied; each requestId is null. Exact response ID, route ID, and session joins are recorded.",
            "capture_sequence_note": "captureBridgeSequence is a local capture sequence, not a gateway-provided identity.",
            "responses": provider_rows,
        },
        "turn_tool_lineage": turns,
        "provider_to_durable_tool_lineage": {"provider_tool_call_count": len(provider_call_ids), "unique_provider_tool_call_ids": len(set(provider_call_ids)), "durable_tool_call_checks": durable_call_checks, "durable_tool_calls": durable_calls},
        "approval_records": approval_summary,
        "filesystem_effects": {
            "alpha_fixture_sha256": sha_file(WORK / "scan-a.txt"),
            "alpha_fact_present": ALPHA in (WORK / "scan-a.txt").read_text(),
            "beta_fixture_sha256": sha_file(WORK / "nested/scan-b.txt"),
            "beta_fact_present": BETA in (WORK / "nested/scan-b.txt").read_text(),
            "denied_write_target_absent": not (WORK / f"blocked_{lower_nonce}.txt").exists(),
            "policy_rejected_marker_absent": not (WORK / f"python_policy_{lower_nonce}.txt").exists(),
            "approved_edit_final_content": (WORK / f"approved_{lower_nonce}.txt").read_text(),
            "long_bash_started_and_finished": (WORK / f"long_{lower_nonce}.started").exists() and (WORK / f"long_{lower_nonce}.finished").exists(),
            "long_bash_finished_content": (WORK / f"long_{lower_nonce}.finished").read_text(),
            "cancel_started_and_finished_absent": (WORK / f"cancel_{lower_nonce}.started").exists() and not (WORK / f"cancel_{lower_nonce}.finished").exists(),
        },
        "presentation": {
            "fixture_sha256": report["pptxFixture"].get("sha256"),
            "before_sha256": sha_file(before),
            "after_sha256": sha_file(after),
            "workspace_after_sha256": sha_file(expected_deck),
            "helper_after_create_sha256": sha_file(RUN_DIR / "model-helper-after-create-create_live_deck.go.txt"),
            "helper_after_edit_sha256": sha_file(RUN_DIR / "model-helper-after-edit-create_live_deck.go.txt"),
            "ooxml_added_parts": added_parts,
            "ooxml_removed_parts": removed_parts,
            "ooxml_changed_parts": changed_parts,
            "before_structure": before_structure,
            "after_structure": after_structure,
            "independent_verifier_path": str(independent_pptx_path.relative_to(ROOT)),
            "independent_verifier_status": independent_pptx.get("status"),
            "independent_rendered_pages": independent_pptx.get("render", {}).get("pages", []),
            "visual_review_document": "docs/qa/2026-10-14/live/review/193912-independent/pptx-visual-review.md",
        },
        "compaction": {
            "read_receipt": {
                "entry_id": source_receipt.get("id"),
                "sequence": source_receipt.get("seq"),
                "tool_name": source_receipt.get("tool_name"),
                "tool_call_id": source_receipt.get("tool_call_id"),
                "outcome": source_receipt.get("receipt_outcome"),
                "bytes": len(context_text.encode()),
                "sha256": sha256(context_text.encode()),
                "first_and_last_marker_present": FIRST in context_text and LAST in context_text,
            },
            "summary_entry": {
                "entry_id": summary.get("id"),
                "sequence": summary.get("seq"),
                "compacted_through_entry_id": summary.get("compacted_through_entry_id"),
                "same_receipt_boundary": summary_source_match,
                "summary_bytes": len(summary_text.encode()),
                "summary_sha256": sha256(summary_text.encode()),
                "retained_canary": CANARY in summary_text,
                "retained_first_and_last_markers": "LIVE_CONTEXT_FIRST_3A6527E73296" in summary_text and "LIVE_CONTEXT_LAST_3A6527E73296" in summary_text,
            },
            "production_trace_attributes": report["contextCompaction"]["newTraceSpans"][0].get("attributes"),
            "internal_summary_response_sequence": summary_sequence,
            "post_compaction_recall_response_sequence": recall_sequence,
            "post_compaction_request_has_canary": report["contextCompaction"].get("postCompactionProviderRequestContainsMemoryCanary"),
            "post_compaction_request_has_latest_prompt": report["contextCompaction"].get("postCompactionProviderRequestContainsLatestPrompt"),
            "model_recalled_exact_canary": report["contextCompaction"].get("recalledMemoryFact"),
        },
        "cancel_and_recovery": {
            "old_session_id": old_id,
            "cancel_tool_call_ids": cancellation.get("toolCallIds"),
            "receipt_outcome": cancellation["durableToolActions"][0].get("receiptOutcome"),
            "resource_state": cancellation["durableToolActions"][0].get("resourceState"),
            "completion_proof": cancellation["durableToolActions"][0].get("completionProof"),
            "fenced_prompt_had_provider_requests": bool(cancellation["fencedPrompt"].get("providerRequestsContainingMarker")),
            "fenced_prompt_had_durable_user_rows": bool(cancellation["fencedPrompt"].get("durableUserRows")),
            "new_session_id": new_id,
            "new_session_differs_from_old": new_id != old_id,
            "new_session_durable_rows": [{"seq": row.get("seq"), "type": row.get("type"), "id": row.get("id"), "run_outcome": row.get("run_outcome")} for row in new_rows],
            "fresh_turn_provider_response_seen": cancellation["freshTurn"].get("providerResponseSeen"),
            "fresh_turn_tool_call_count": cancellation["freshTurn"].get("toolCallCount"),
            "old_unknown_unchanged_after_fresh_turn": cancellation.get("oldUnknownOutcomeUnchangedAfterFreshTurn"),
        },
        "limitations": [
            "No OS-level filesystem/process sandbox was established.",
            "The provider supplied no separate requestId; the response/route/session join is exact.",
            "PTY screenshots are headless renders of live captured terminal bytes.",
            "The production trace labels trigger_reason as unknown; no manual transcript trimming or synthetic provider history was used.",
        ],
    }
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(ledger, indent=2, sort_keys=True) + "\n")
    print(f"wrote {OUT.relative_to(ROOT)}")
    print(f"sha256 {sha_file(OUT)}")
    print(f"provider failures {len(failed_provider_checks)}; runner failures {len(report_failed_checks)}")
    print(f"independent PPTX failures {sum(not row['passed'] for row in deck_review_checks)}")
    print(f"independent run check failures {sum(not row['passed'] for row in run_checks)}")


if __name__ == "__main__":
    main()
