"""Validate audit artifacts only; this does not run or certify product tests."""

import csv
import hashlib
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

BASE = Path(__file__).resolve().parents[1]
ROOT = BASE.parents[2]
errors = []


def check(condition, message):
    if not condition:
        errors.append(message)


counts = {
    "RT": 11, "LLM": 16, "PRD": 10, "TOOL": 11, "APR": 8,
    "SES": 14, "CMP": 4, "MEM": 5, "UI": 16, "CLI": 17,
    "MCP": 9, "EXT": 7, "SKILL": 4, "TRACE": 6, "VFY": 11,
}
expected = {
    f"{prefix}-{number:0{3 if prefix in ('TOOL', 'APR') else 2}d}"
    for prefix, count in counts.items() for number in range(1, count + 1)
}
plan = json.loads((BASE / "proposed-tasks.json").read_text())
tasks = plan["tasks"]
index = {task["id"]: task for task in tasks}
check(len(index) == len(tasks), "duplicate task IDs")
phases = {phase["id"] for phase in plan["phases"]}
for task in tasks:
    check(task.get("phase_id") in phases, f"invalid phase: {task['id']}")
    for dep in task["depends_on"]:
        check(dep in index, f"unknown dependency: {task['id']} -> {dep}")
    for field in ("deliverable", "verification", "rule_mapping", "commit_spec"):
        check(bool(task.get(field)), f"empty {field}: {task['id']}")
    if task["role"] == "builder":
        check(bool(re.search(r"\bTest\w+", " ".join(task["verification"]))),
              f"missing planned behavior test: {task['id']}")

seen, active = set(), set()


def walk(task_id):
    if task_id in active:
        errors.append(f"dependency cycle: {task_id}")
        return
    if task_id in seen or task_id not in index:
        return
    active.add(task_id)
    for dep in index[task_id]["depends_on"]:
        walk(dep)
    active.remove(task_id)
    seen.add(task_id)


walk("R8-REV-01")
release_tasks = set(seen)
check({t["id"] for t in tasks if t["status"] != "deferred"} <= release_tasks,
      "default release does not depend on every non-deferred task")
for task_id in index:
    walk(task_id)
mapped = {finding for task in tasks for finding in task["rule_mapping"]}
builder_mapped = {finding for task in tasks if task["role"] == "builder"
                  for finding in task["rule_mapping"]}
check(mapped == expected | {"D-01"}, f"task finding mismatch: {sorted(mapped ^ (expected | {'D-01'}))}")
check(expected <= builder_mapped, f"no builder: {sorted(expected - builder_mapped)}")

mapping = list(csv.DictReader((BASE / "findings-to-iterations.csv").open()))
id_column = next((name for name in ("finding_id", "finding", "id") if name in mapping[0]), None)
check(id_column is not None, "missing CSV finding ID column")
if id_column:
    ids = [row[id_column] for row in mapping]
    check(len(ids) == len(set(ids)), "duplicate finding rows")
    check(set(ids) in (expected, expected | {"D-01"}), "CSV finding registry mismatch")
    for row in mapping:
        finding = row[id_column]
        source_report = BASE / row["source_report"]
        check(source_report.exists() and finding in source_report.read_text(),
              f"CSV finding source missing: {finding}")
        expected_refs = {task["id"] for task in tasks if finding in task["rule_mapping"]}
        expected_builders = {task_id for task_id in expected_refs if index[task_id]["role"] == "builder"}
        expected_phases = {index[task_id]["phase_id"] for task_id in expected_refs}
        check(set(row["task_ids"].split(";")) == expected_refs, f"CSV complete task mapping mismatch: {finding}")
        check(set(row["builder_task_ids"].split(";")) == expected_builders, f"CSV builder mapping mismatch: {finding}")
        check(set(row["phase_ids"].split(";")) == expected_phases, f"CSV phase mapping mismatch: {finding}")
        refs = set(re.findall(r"\bR\d+-(?:DES|BLD|REV)-\d+\b", " ".join(row.values())))
        check(bool(refs), f"CSV row has no task: {finding}")
        for ref in refs:
            check(ref in index, f"CSV unknown task: {finding} -> {ref}")
            if ref in index:
                check(finding in index[ref]["rule_mapping"], f"CSV/task mismatch: {finding} -> {ref}")

inventory = list(csv.DictReader((BASE / "source-inventory.csv").open()))
coverage = list(csv.DictReader((BASE / "source-coverage.csv").open()))
inventory_paths = {row["path"] for row in inventory}
check(len(inventory_paths) == len(inventory) == 810, "invalid baseline inventory")
check(len({row["path"] for row in coverage}) == len(coverage) == 810, "duplicate/missing coverage paths")
check({row["path"] for row in coverage} == inventory_paths, "coverage/inventory mismatch")
for row in inventory:
    source = ROOT / row["path"]
    check(source.exists() and hashlib.sha256(source.read_bytes()).hexdigest() == row["sha256"],
          f"baseline file changed: {row['path']}")
production = [row for row in coverage if row["kind"] == "go_source"]
check(len(production) == 321, "production source count mismatch")
for row in production:
    check(bool(row["audit_report"]), f"source unmapped: {row['path']}")
    check(not re.search(r"pending|not reviewed|unmapped", row["source_review_level"], re.I),
          f"source review scope pending: {row['path']}")

for document in BASE.glob("*.md"):
    for match in re.finditer(r"\]\(([^)]+)\)", document.read_text()):
        ref = match.group(1).strip("<>")
        if "://" in ref or ref.startswith("#"):
            continue
        path = Path(re.sub(r":\d+(?:-\d+)?$", "", ref.split("#")[0]))
        target = path if path.is_absolute() else document.parent / path
        check(target.exists(), f"broken local link: {document.name} -> {ref}")

result = {
    "scope": "Audit artifact structure, baseline hashes, task DAG and finding mappings only; not product test execution or release approval.",
    "checked_at": datetime.now(timezone.utc).isoformat(),
    "baseline_commit": plan["baseline_commit"],
    "status": "pass" if not errors else "fail",
    "baseline_files_checked": len(inventory),
    "production_source_files": len(production),
    "stable_findings": len(expected),
    "product_decisions": 1,
    "tasks": len(tasks),
    "default_release_tasks": len(release_tasks),
    "deferred_tasks": sum(task["status"] == "deferred" for task in tasks),
    "mapping_rows": len(mapping),
    "errors": errors,
    "artifact_sha256": {path.name: hashlib.sha256(path.read_bytes()).hexdigest()
                        for path in sorted(BASE.iterdir()) if path.is_file()},
}
(BASE / "evidence" / "audit-artifact-validation.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
print(json.dumps({key: value for key, value in result.items() if key != "artifact_sha256"}, ensure_ascii=False, indent=2))
sys.exit(bool(errors))
