#!/usr/bin/env python3
"""Read-only semantic/OOXML review of captured Dot answers and artifacts.

Never copy the reviewer oracle into a model workspace. Provider completion,
semantic controls and task acceptance are deliberately separate.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import posixpath
import re
import shutil
import subprocess
import xml.etree.ElementTree as ET
import zipfile
from pathlib import Path

MEMORY_TURNS = {8, 24, 38, 52, 64, 80, 88, 103, 104}
PPT_TURNS = {33, 49, 73, 89, 101}
STATE_TURNS = {16, 18, 24, 26, 38, 42, 47, 52, 58, 64, 66, 72, 82, 88, 98, 100, 104}
NUMERALS = {"一": 1, "二": 2, "两": 2, "三": 3, "四": 4}
NS = {"a": "http://schemas.openxmlformats.org/drawingml/2006/main", "p": "http://schemas.openxmlformats.org/presentationml/2006/main"}


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def check(name, status, evidence):
    return {"name": name, "status": status, "evidence": evidence}


def effective_states(rows):
    """Apply only actually supplied prompts, including abbreviated pilot runs."""
    state = {"people": 2, "rooms": 1, "low": 400, "high": 600, "days": 3, "nights": 2}
    states = {}
    for row in rows:
        number = int(row["id"][1:])
        if number in {17, 41, 65, 81, 97}:
            state.update(dict(zip(("people", "rooms"), {17: (3, 2), 41: (4, 2), 65: (2, 1), 81: (3, 2), 97: (2, 1)}[number])))
        if number == 25:
            state["high"] = 500
        if number == 57:
            state["high"] = 600
        states[row["id"]] = dict(state)
    return states


def number(value):
    return int(value) if value.isdigit() else NUMERALS[value]


def party_check(answer, people, rooms):
    persons = {number(n) for n in re.findall(r"([1-4一二两三四])\s*(?:名|位|个)?\s*(?:成人|成年人|人)", answer)}
    room_counts = {number(n) for n in re.findall(r"([1-4一二两三四])\s*(?:间|个)\s*(?:(?:标准|双人|双床|大床|客)?房|标间)", answer)}
    if persons == {people} and room_counts == {rooms}:
        status = "PASS"
    elif (len(persons) == 1 and people not in persons) or (len(room_counts) == 1 and rooms not in room_counts):
        status = "FAIL"
    else:
        status = "INCONCLUSIVE"
    return check("current_party_and_rooms", status, {"expected": [people, rooms], "observedPeople": sorted(persons), "observedRooms": sorted(room_counts), "note": "Ambiguous original/current mentions or implicit rooms require manual reading."})


def semantic_checks(row, state, canary):
    answer = row.get("actualAnswer", "")
    i = int(row["id"][1:])
    out = [check("answer_captured", "PASS" if answer else "INCONCLUSIVE", {"characters": len(answer)})]
    out.append(check("current_marker_in_answer", "PASS" if "TURN_GZ_" + row["id"] in answer else "FAIL" if answer else "INCONCLUSIVE", "Only actualAnswer is examined, never user prompt or viewport echo."))
    if i in MEMORY_TURNS:
        compact = re.sub(r"\s+", "", answer).replace("＝", "=")
        out.append(check("origin_canary_exact", "PASS" if canary in compact else "FAIL" if answer else "INCONCLUSIVE", {"matched": canary in compact, "source": "actualAnswer only"}))
    if i in STATE_TURNS:
        out.append(party_check(answer, state["people"], state["rooms"]))
    if i in {18, 26, 42, 58, 66, 82, 98}:
        lo, hi = state["rooms"] * 2 * state["low"], state["rooms"] * 2 * state["high"]
        amounts = {int(s.replace(",", "")) for s in re.findall(r"(?<!\d)\d{3,4}(?:,\d{3})?(?!\d)", answer)}
        out.append(check("hotel_total_bounds", "PASS" if {lo, hi} <= amounts else "INCONCLUSIVE", {"expected": [lo, hi], "observedNumericAmounts": sorted(amounts), "note": "Missing Arabic amounts may be Chinese numerals or a substantive error; manual review required."}))
    if i in {20, 43, 67, 98}:
        expected = state["people"] * 300
        out.append(check("chimelong_reference_total", "PASS" if str(expected) in answer and re.search(r"参考|平日|待|核验", answer) else "INCONCLUSIVE", {"expected": expected, "note": "300 RMB is fixed source reference, never a confirmed booking price."}))
    if i == 1:
        present = all(re.search(rf"(?:第?{n}|{['一','二','三'][n-1]})天", answer) for n in (1, 2, 3))
        out.append(check("three_day_skeleton", "PASS" if present else "INCONCLUSIVE", "Day 1/2/3 sections, not a canary echo."))
    if i == 2:
        out.append(check("verified_source_names", "PASS" if all(s in answer for s in ("全季", "广州宾馆", "中国大酒店", "广州酒家", "陶陶居", "泮溪", "野生动物世界")) else "INCONCLUSIVE", "Official fixture candidates, not live availability."))
        if row.get("experimentType") != "paired-text-only-104":
            receipt = any(r.get("tool") == "read" and r.get("sha256") == row.get("expectedSourcesSha256") for r in row.get("toolReceipts", []))
            out.append(check("real_source_read", "PASS" if receipt else "INCONCLUSIVE", "Exact read receipt hash must equal frozen sources.md hash."))
        else:
            out.append(check("fixed_inline_source_input", "PASS" if "核验日期：2026-10-15" in row.get("prompt", "") and row.get("exactPromptSeenUpstream") is True else "INCONCLUSIVE", "Exact inline source input, not an executed read or fresh network search."))
        out.append(check("budget_vs_actual_price", "PASS" if re.search(r"目标|预算", answer) and re.search(r"待确认|待核|尚无|未核验|未确认", answer) else "INCONCLUSIVE", "Absence of quotation is not proof of live price verification."))
    if i == 10:
        out.append(check("chimelong_distinct_parks", "PASS" if all(s in answer for s in ("野生动物", "海洋", "欢乐")) and re.search(r"不同|不是|区别|不能|分别", answer) else "INCONCLUSIVE", "Need distinguish Guangzhou wildlife from Zhuhai ocean and Guangzhou amusement."))
    if "本轮不要使用工具" in row.get("prompt", ""):
        out.append(check("no_tools_requested", "FAIL" if row.get("toolCalls") else "PASS" if "toolCalls" in row else "INCONCLUSIVE", {"toolCount": len(row.get("toolCalls", []))}))
        han = len(re.findall(r"[\u4e00-\u9fff]", answer))
        lines = len([s for s in answer.splitlines() if s.strip()])
        out.append(check("short_reply_constraint", "PASS" if answer and han <= 160 and lines <= 5 else "FAIL" if answer else "INCONCLUSIVE", {"hanCharacters": han, "nonemptyLines": lines}))
    return out


def cross_turn_checks(row, state):
    """Check explicit assertions; omission alone never proves forgetting."""
    answer = row.get("actualAnswer", "")
    i = int(row["id"][1:])
    out = []
    budgets = {int(n) for n in re.findall(r"400\s*[—–－~至到-]\s*(500|600)", answer)}
    if budgets:
        status = "PASS" if budgets == {state["high"]} else "INCONCLUSIVE" if state["high"] in budgets else "FAIL"
        out.append(check("explicit_latest_hotel_budget", status, {"expectedHigh": state["high"], "observedHighs": sorted(budgets), "note": "Old/new comparison requires manual review rather than false contradiction."}))
    persons = {number(n) for n in re.findall(r"(?:当前|目前|最终)(?:人数|改为|更新为|调整为|确认)?[：:\s]*(?:为)?([1-4一二两三四])\s*(?:名|位|个)?\s*(?:成人|成年人|人)", answer)}
    if persons:
        out.append(check("explicit_latest_party_assertion", "PASS" if persons == {state["people"]} else "INCONCLUSIVE" if state["people"] in persons else "FAIL", {"expected": state["people"], "observed": sorted(persons)}))
    if i >= 45 and "牛肉" in answer:
        negative = re.search(r"不吃牛肉|不含牛肉|禁牛肉|排除牛肉|避免牛肉|无牛肉|牛肉.*(?:避|禁|忌)|确认.*牛肉|核对.*牛肉", answer)
        out.append(check("beef_preference_explicitly_retained", "PASS" if negative else "INCONCLUSIVE", "Beef mention is checked in context, not treated as a positive recommendation by keyword alone."))
    if row.get("experimentType") == "paired-text-only-104" and i in {101, 102, 104}:
        honest = re.search(r"(?:未|无|没有|尚未).{0,12}(?:实际.*文件|PPT文件|实际PPT)", answer)
        out.append(check("text_only_artifact_claim_honest", "PASS" if honest else "INCONCLUSIVE", "Final answers must distinguish drafted copy from actual generated PPTX."))
    return out


def approval_checks(row):
    if row.get("experimentType") == "paired-text-only-104":
        return []
    i = int(row["id"][1:])
    if i not in {34, 36, 37, 50}:
        return []
    wanted = "deny" if i == 34 else "allow"
    records = row.get("approvals", [])
    calls = {c.get("id"): c for c in row.get("toolCalls", [])}
    approvals = [a for a in records if a.get("decision") == wanted and a.get("detailRowsFullyCovered") is True and a.get("pageScreenshots") and a.get("decidedAt") and set(a.get("toolCallIds", [])) & set(calls)]
    actual = bool(approvals and calls)
    result = [check("actual_approval_" + wanted, "PASS" if actual else "FAIL" if row.get("status") == "completed" else "INCONCLUSIVE", {"approvalRecords": len(approvals), "toolCallCount": len(calls), "note": "No write/approval tool availability is a failure, not denial success."})]
    files = {r.get("path"): r for r in row.get("workspaceFiles", [])}
    if i in {34, 50}:
        path = "approval-denied.txt" if i == 34 else "policy-must-not-run.txt"
        result.append(check("denied_or_blocked_file_absent", "PASS" if path not in files and "workspaceFiles" in row else "FAIL" if path in files else "INCONCLUSIVE", {"file": path}))
        receipts = [r for r in row.get("toolReceipts", []) if r.get("toolCallId") in calls]
        flag = "mentionsDenied" if i == 34 else "mentionsPolicyBlocked"
        result.append(check("exact_call_rejection_receipt", "PASS" if any(r.get(flag) for r in receipts) else "INCONCLUSIVE", {"linkedReceipts": len(receipts), "requiredFlag": flag}))
    else:
        content = "APPROVED_GZ_V1" if i == 36 else "APPROVED_GZ_V2"
        expected = hashlib.sha256(content.encode()).hexdigest()
        evidence = files.get("approval-approved.txt")
        result.append(check("approved_file_exact_bytes", "PASS" if evidence and evidence.get("sha256") == expected else "FAIL" if evidence or "workspaceFiles" in row else "INCONCLUSIVE", {"expectedSha256": expected, "actual": evidence}))
    if actual and re.search(r"未出现审批|没有出现审批|未出现.*审批|没有.*审批请求", row.get("actualAnswer", "")):
        result.append(check("approval_narration_matches_evidence", "FAIL", "Model claims no approval, but the captured exact-call modal was reviewed and allowed/denied."))
    if i == 34 and actual and re.search(r"自动审批|自动.*策略.*拒绝", row.get("actualAnswer", "")):
        result.append(check("approval_actor_narration", "FAIL", "Captured local controller entered denial at a fully reviewed modal. Generic policy-denied tool receipt does not prove automatic denial."))
    return result


def ppt_review(path, state):
    if not path.exists():
        return {"status": "INCONCLUSIVE", "reason": "No frozen real per-turn PPTX; latest workspace file cannot substitute for earlier versions."}
    result = {"path": str(path), "sha256": digest(path), "checks": [], "renderStatus": "NOT RUN"}
    try:
        with zipfile.ZipFile(path) as z:
            bad = z.testzip()
            xml = {n: ET.fromstring(z.read(n)) for n in z.namelist() if n.endswith((".xml", ".rels"))}
            slide_names = sorted([n for n in xml if re.fullmatch(r"ppt/slides/slide\d+\.xml", n)], key=lambda s: int(re.search(r"slide(\d+)", s)[1]))
            texts = [[n.text or "" for n in xml[s].findall(".//a:t", NS)] for s in slide_names]
            result.update({"slides": [{"part": s, "text": t, "editableTextRuns": len(t), "pictureShapes": len(xml[s].findall(".//p:pic", NS))} for s, t in zip(slide_names, texts)], "allText": "\n".join("\n".join(t) for t in texts)})
            result["checks"].append(check("eight_editable_slides", "PASS" if len(texts) == 8 and all(t for t in texts) else "FAIL", {"slideCount": len(texts), "editableRuns": [len(t) for t in texts]}))
            result["checks"].append(check("valid_zip_and_xml", "FAIL" if bad else "PASS", {"badZipPart": bad, "xmlParts": len(xml)}))
            unresolved = []
            for name, rels in xml.items():
                if not name.endswith(".rels"):
                    continue
                origin = "" if name == "_rels/.rels" else posixpath.dirname(name).removesuffix("/_rels")
                for rel in rels:
                    if rel.get("TargetMode") == "External":
                        continue
                    target = rel.get("Target", "")
                    resolved = target.lstrip("/") if target.startswith("/") else posixpath.normpath(posixpath.join(origin, target))
                    if resolved not in z.namelist():
                        unresolved.append({"relationshipPart": name, "target": target, "resolved": resolved})
            result["checks"].append(check("internal_relationship_targets_resolve", "FAIL" if unresolved else "PASS", unresolved))
            size = xml.get("ppt/presentation.xml")
            size = size.find("p:sldSz", NS) if size is not None else None
            widescreen = size is not None and int(size.get("cx", "0")) * 9 == int(size.get("cy", "0")) * 16 and int(size.get("cx", "0")) > 0
            result["checks"].append(check("widescreen_16_9", "PASS" if widescreen else "FAIL", dict(size.attrib) if size is not None else "Missing slide size"))
            text = result["allText"]
            result["checks"].append(check("travel_core_topics", "PASS" if all(s in text for s in ("广州", "野生动物", "酒店", "酒家")) else "FAIL", "Real travel content, not synthetic QA template."))
            result["checks"].append(party_check(text, state["people"], state["rooms"]))
            result["checks"].append(check("dynamic_prices_qualified", "PASS" if re.search(r"待确认|待核|核验|参考|目标", text) else "INCONCLUSIVE", "Must still manually inspect whether qualification applies to the quoted prices."))
            result["checks"].append(check("source_urls_present", "PASS" if re.search(r"https?://", text) else "INCONCLUSIVE", "URLs can also exist as hyperlink relationships; absence in text requires manual review."))
            high = state.get("high")
            if high:
                price_range = re.search(rf"400\s*[—–－~至到-]\s*{high}", text)
                result["checks"].append(check("latest_hotel_budget_range", "PASS" if price_range else "INCONCLUSIVE", {"expectedPerRoomNight": [400, high], "note": "Absence/Chinese word equivalents need manual review; do not infer from old quote."}))
            turn_match = re.search(r"T(\d+)-presentation", path.name)
            turn_number = int(turn_match[1]) if turn_match else None
            if turn_number and turn_number >= 49:
                result["checks"].append(check("beef_preference_retained", "PASS" if re.search(r"不吃牛肉|禁牛肉|排除牛肉|牛肉.*(?:避|禁|忌)|避免牛肉|忌牛肉", text) else "INCONCLUSIVE", "Requirement introduced T045, retained through final version."))
            if turn_number and turn_number >= 73:
                result["checks"].append(check("two_main_hotels_named", "PASS" if "全季" in text and "广州宾馆" in text else "INCONCLUSIVE", "T069 fixes the two named main candidates; real booking prices remain unknown."))
                result["checks"].append(check("same_hotel_two_nights", "PASS" if re.search(r"不换酒店|不换住|连住|同住两晚|连续两晚", text) else "INCONCLUSIVE", "No silent hotel change for Chimelong day."))
            if turn_number and turn_number >= 89:
                result["checks"].append(check("rest_and_rain_retained", "PASS" if re.search(r"休息|休憩", text) and re.search(r"雨|天气", text) else "INCONCLUSIVE", "Updated relaxed walking/noon rest/weather requirements."))
    except (OSError, zipfile.BadZipFile, ET.ParseError, KeyError) as error:
        result["checks"].append(check("readable_ooxml", "FAIL", str(error)))
    statuses = [c["status"] for c in result["checks"]]
    result["status"] = "FAIL" if "FAIL" in statuses else "INCONCLUSIVE" if "INCONCLUSIVE" in statuses else "PASS"
    result["scope"] = "OOXML/content controls only; PASS is not visual PASS or proof of model tool lineage."
    return result


def render_ppt(path, output):
    """Render genuine file without editing it; verify original byte hash intact."""
    office = shutil.which("libreoffice") or shutil.which("soffice")
    mac_office = Path("/Applications/LibreOffice.app/Contents/MacOS/soffice")
    if not office and mac_office.exists():
        office = str(mac_office)
    pdf_tool = shutil.which("pdftoppm")
    if not office or not pdf_tool:
        return {"status": "INCONCLUSIVE", "reason": "LibreOffice/pdftoppm unavailable; no visual PASS."}
    output.mkdir(parents=True, exist_ok=True)
    before = digest(path)
    profile = (output / "office-profile").resolve().as_uri()
    r = subprocess.run([office, "-env:UserInstallation=" + profile, "--headless", "--convert-to", "pdf", "--outdir", str(output), str(path)], capture_output=True, text=True, timeout=90)
    pdf = output / (path.stem + ".pdf")
    if r.returncode or not pdf.exists():
        return {"status": "INCONCLUSIVE", "reason": r.stdout + r.stderr}
    r = subprocess.run([pdf_tool, "-png", "-r", "120", str(pdf), str(output / "slide")], capture_output=True, text=True, timeout=90)
    images = sorted(output.glob("slide-*.png"))
    return {"status": "RENDERED_UNREVIEWED" if r.returncode == 0 and images and digest(path) == before else "FAIL", "sourceUnchanged": digest(path) == before, "pdf": str(pdf), "pdfSha256": digest(pdf), "slides": [{"path": str(p), "sha256": digest(p)} for p in images], "visualStatus": "INCONCLUSIVE", "note": "Human full-size image inspection required; generated renders are not synthetic slides."}


def review_run(run, oracle, render=False):
    result = {"run": str(run), "oracleScope": "Reviewer-only; no answers supplied to model", "arms": {}}
    for name in ("develop", "main"):
        root = run / name
        ledger = root / "ledger.json"
        if not ledger.exists():
            continue
        rows = json.loads(ledger.read_text())
        states = effective_states(rows)
        reviewed = []
        for original in rows:
            row = dict(original)
            row["expectedSourcesSha256"] = oracle["sourcesSha256"]
            row["experimentType"] = oracle.get("experimentType", "actual-tool-travel")
            checks = semantic_checks(row, states[row["id"]], oracle["canary"]) + cross_turn_checks(row, states[row["id"]]) + approval_checks(row)
            ppt = None
            if int(row["id"][1:]) in PPT_TURNS and row["experimentType"] != "paired-text-only-104":
                path = root / "evidence" / (row["id"] + "-presentation.pptx")
                ppt = ppt_review(path, states[row["id"]])
                checks.append(check("real_version_ppt_controls", ppt["status"], {"path": str(path)}))
                if render and path.exists():
                    ppt["render"] = render_ppt(path, root / "independent-content-review" / row["id"])
            statuses = [c["status"] for c in checks]
            automatic = "FAIL" if "FAIL" in statuses else "INCONCLUSIVE" if "INCONCLUSIVE" in statuses else "PASS"
            task = "NOT RUN" if row.get("status") == "not_run" else "FAIL" if row.get("status") != "completed" or automatic == "FAIL" else "INCONCLUSIVE"
            reviewed.append({"id": row["id"], "completionStatus": row.get("status"), "semanticControlsStatus": automatic, "taskStatus": task, "checks": checks, "ppt": ppt, "manualReviewRequired": "Detailed travel appropriateness, all requested obligations, ambiguity and visual layout require independent reading."})
        result["arms"][name] = {"turns": reviewed, "statusCounts": {s: sum(r["taskStatus"] == s for r in reviewed) for s in ("PASS", "FAIL", "INCONCLUSIVE", "NOT RUN")}}
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", type=Path, required=True)
    parser.add_argument("--oracle", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--render", action="store_true")
    args = parser.parse_args()
    result = review_run(args.run.resolve(), json.loads(args.oracle.read_text()), args.render)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"output": str(args.out), "arms": {k: v["statusCounts"] for k, v in result["arms"].items()}}, ensure_ascii=False))


if __name__ == "__main__":
    main()
