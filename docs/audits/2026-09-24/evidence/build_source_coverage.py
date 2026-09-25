#!/usr/bin/env python3
"""Build the per-file coverage ledger from the fixed source inventory."""

from __future__ import annotations

import csv
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "source-inventory.csv"
OUTPUT = ROOT / "source-coverage.csv"

MODULES = {
    "internal/core": (
        "01-runtime.md",
        "manifested source: implementation contracts and cross-file call paths",
        "all test paths enumerated; cited lifecycle/history/cancel/continuation tests inspected in detail; no suite run in this lane",
    ),
    "internal/llm": (
        "02-llm-production.md",
        "production source manifest and static request/stream path review; no real provider call",
        "test files enumerated; no full suite run; targeted temporary repros noted in report",
    ),
    "internal/production": (
        "02-llm-production.md",
        "production source manifest and static runtime wiring review",
        "test files enumerated; no full suite run; targeted temporary repros noted in report",
    ),
    "internal/session": (
        "04-session-compaction.md",
        "all target production source files individually reviewed",
        "test files enumerated and selected assertions inspected; repository test suite not run in this lane",
    ),
    "internal/compaction": (
        "04-session-compaction.md",
        "all target production source files individually reviewed",
        "test files enumerated and selected assertions inspected; repository test suite not run in this lane",
    ),
    "internal/memory": (
        "04-session-compaction.md",
        "all target production source files individually reviewed",
        "test files enumerated and selected assertions inspected; repository test suite not run in this lane",
    ),
    "internal/tui": (
        "05-ui-review.md",
        "all listed TUI production files individually reviewed",
        "test files listed; no full suite in this lane; no real PTY/visual E2E",
    ),
    "internal/term": (
        "05-ui-review.md",
        "all four terminal-size production files reviewed",
        "no tests found; platform runtime behavior not verified",
    ),
    "cmd/cli": (
        "06-cli-config-acp.md; 09-verification.md",
        "source manifest; entry/wiring paths traced, remaining scope structural",
        "test path inventoried; go test -race ./cmd/cli passed per 09",
    ),
    "internal/cli": (
        "06-cli-config-acp.md; selected paths in 01/02/04/05",
        "source manifest; selected assembly/session/UI/provider call paths traced, remaining scope structural",
        "test files inventoried; related targeted tests/repros are recorded in individual reports; no assertion-by-assertion claim",
    ),
    "internal/config": (
        "06-cli-config-acp.md",
        "source manifest; configuration and runtime exposure paths reviewed",
        "test files inventoried; full assertion semantics not reviewed",
    ),
    "internal/acp": (
        "06-cli-config-acp.md",
        "source manifest; HTTP/stdio/client/server identity and event paths reviewed",
        "test files inventoried; no remote ACP/browser E2E",
    ),
    "internal/web": (
        "06-cli-config-acp.md; 05-ui-review.md",
        "source manifest; echo/SSE wiring and UI projection boundaries reviewed",
        "test path inventoried; no browser journey or visual E2E",
    ),
    "internal/extension": (
        "07-extensions-tracing.md",
        "per-file source scope and implementation boundaries reviewed",
        "test files listed with per-area coverage notes; not executed in this lane",
    ),
    "internal/mcp": (
        "07-extensions-tracing.md",
        "per-file source scope and implementation boundaries reviewed",
        "test files listed with per-area coverage notes; no standard external MCP server interoperability run",
    ),
    "internal/skill": (
        "07-extensions-tracing.md",
        "per-file source scope and implementation boundaries reviewed",
        "test files listed with per-area coverage notes; not executed in this lane",
    ),
    "internal/tracing": (
        "07-extensions-tracing.md",
        "per-file source scope and implementation boundaries reviewed",
        "test files listed with per-area coverage notes; no standard collector/broker interoperability run",
    ),
    "internal/mock": (
        "09-verification.md",
        "all 15 production source files individually reviewed",
        "all 17 test files reviewed; default suite and mock-tag suite execution are separately recorded; mocks do not prove real upstream interoperability",
    ),
    "internal/verify": (
        "09-verification.md",
        "all 7 production source files individually reviewed",
        "all 8 test files reviewed; helper-only log/leak targets and scanner outcomes are separately recorded",
    ),
    "internal/tools": (
        "03-tools-approval.md",
        "all 70 production source files individually listed and reviewed",
        "all 55 test source files included in module coverage; default test execution is recorded in 09; the one e2e-tagged internal file is not covered by the tests-package e2e command",
    ),
    "internal/approval": (
        "03-tools-approval.md",
        "all 11 production source files individually listed and reviewed",
        "all 12 test source files included in module coverage; default test execution is recorded in 09",
    ),
    "tests": (
        "09-verification.md; related reports where cited",
        "inventory and build-tag/skip/verify-target mapping; no claim that every test assertion was source-reviewed",
        "default, mock-tag, and e2e-tag execution status is differentiated in 09; integration scenarios not implied by test count",
    ),
    "root": (
        "09-verification.md",
        "tracked path inventory plus selected build, workflow, project-document and metadata review",
        "not applicable / no blanket validation claim",
    ),
}

FULL_SOURCE_MODULES = {
    "internal/tools",
    "internal/approval",
    "internal/session",
    "internal/compaction",
    "internal/memory",
    "internal/tui",
    "internal/term",
    "internal/extension",
    "internal/mcp",
    "internal/skill",
    "internal/tracing",
    "internal/mock",
    "internal/verify",
}

RELATED_TEST_MODULES = {
    "internal/core",
    "internal/llm",
    "internal/production",
    "internal/tools",
    "internal/approval",
    "internal/session",
    "internal/compaction",
    "internal/memory",
    "internal/tui",
    "internal/extension",
    "internal/mcp",
    "internal/skill",
    "internal/tracing",
    "internal/mock",
    "internal/verify",
}

NOT_EXECUTED = {
    "internal/core/e2e_stoplength_test.go",
    "internal/core/e2e_streaming_pipeline_test.go",
    "internal/cli/integration_test.go",
    "internal/cli/line_editor_pty_linux_test.go",
    "internal/cli/resize_other_test.go",
    "internal/llm/e2e_cycler_test.go",
    "internal/llm/e2e_thinking_test.go",
    "internal/memory/e2e_test.go",
    "internal/tools/e2e_streaming_test.go",
    "internal/tui/e2e_render_test.go",
}


def main() -> None:
    with SOURCE.open(newline="", encoding="utf-8") as f:
        files = list(csv.DictReader(f))

    with OUTPUT.open("w", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        writer.writerow(
            [
                "path",
                "module",
                "kind",
                "lines",
                "sha256",
                "audit_report",
                "coverage_bucket",
                "source_review_level",
                "execution_evidence",
                "test_evidence_level",
            ]
        )
        for item in files:
            module = item["module"]
            report, source_level, test_level = MODULES.get(
                module,
                (
                    "09-verification.md",
                    "inventory only; no dedicated source review manifest found",
                    "inventory only; no dedicated test review manifest found",
                ),
            )
            if item["kind"] == "go_test":
                source_level = "test source: " + test_level
                test_level = "see report execution matrix; inventory or manifest is not a pass result"
                coverage_bucket = (
                    "associated_test_reading_or_per_file_coverage"
                    if module in RELATED_TEST_MODULES
                    else "inventory_and_execution_target_mapping"
                )
                if item["path"] in NOT_EXECUTED:
                    execution_evidence = "not executed in recorded commands/host"
                elif "/e2e_" in item["path"] or item["path"].rsplit("/", 1)[-1].startswith("e2e_"):
                    execution_evidence = "e2e-tag lane recorded; see 09 for package scope and skips"
                elif module == "internal/mock" and "mock_" in item["path"].rsplit("/", 1)[-1]:
                    execution_evidence = "mock-tag lane recorded; see 09"
                else:
                    execution_evidence = "default race target recorded; package/host limits in 09"
            elif item["kind"] == "go_source":
                coverage_bucket = (
                    "full_source_files_reviewed"
                    if module in FULL_SOURCE_MODULES
                    else "manifest_and_module_or_call_path_scope"
                )
                execution_evidence = "not applicable to production source"
            elif item["kind"] != "go_source":
                source_level = "tracked-file inventory; scope-specific review only as cited"
                test_level = "not a Go test file"
                coverage_bucket = "non_go_inventory_or_selected_asset_review"
                execution_evidence = "not a Go test file"
                if item["path"] == "Dockerfile":
                    source_level = "selected static review: amd64 binary build vs unpinned Alpine runtime platform; no Docker/Buildx execution"
            writer.writerow(
                [
                    item["path"],
                    module,
                    item["kind"],
                    item["lines"],
                    item["sha256"],
                    report,
                    coverage_bucket,
                    source_level,
                    execution_evidence,
                    test_level,
                ]
            )

    print(f"wrote {len(files)} rows to {OUTPUT}")


if __name__ == "__main__":
    main()
