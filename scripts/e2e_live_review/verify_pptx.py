#!/usr/bin/env python3
"""Independently check a Dot-produced PPTX and its second-turn edit."""

from __future__ import annotations

import argparse
import hashlib
import json
import posixpath
import shutil
import struct
import subprocess
import zipfile
from pathlib import Path, PurePosixPath
from typing import Any
from xml.etree import ElementTree as ET


NS = {
    "a": "http://schemas.openxmlformats.org/drawingml/2006/main",
    "p": "http://schemas.openxmlformats.org/presentationml/2006/main",
    "r": "http://schemas.openxmlformats.org/officeDocument/2006/relationships",
    "pr": "http://schemas.openxmlformats.org/package/2006/relationships",
}
PRESENTATION_REL = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide"
CONTENT_TYPES_NS = "http://schemas.openxmlformats.org/package/2006/content-types"
MAIN_PRESENTATION_CONTENT_TYPE = "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"
SLIDE_CONTENT_TYPE = "application/vnd.openxmlformats-officedocument.presentationml.slide+xml"
SLIDE_MASTER_CONTENT_TYPE = "application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"
SLIDE_LAYOUT_CONTENT_TYPE = "application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"
THEME_CONTENT_TYPE = "application/vnd.openxmlformats-officedocument.theme+xml"


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def png_dimensions(path: Path) -> tuple[int, int] | None:
    with path.open("rb") as source:
        header = source.read(24)
    if len(header) < 24 or header[:8] != b"\x89PNG\r\n\x1a\n":
        return None
    return struct.unpack(">II", header[16:24])


def clean_text(value: str) -> str:
    return " ".join(value.split())


def shape_text(shape: ET.Element) -> str:
    paragraphs: list[str] = []
    for paragraph in shape.findall(".//a:p", NS):
        text = "".join(node.text or "" for node in paragraph.findall(".//a:t", NS))
        if text:
            paragraphs.append(text)
    return clean_text(" ".join(paragraphs))


def transform_signature(shape: ET.Element) -> tuple[str, ...] | None:
    transform = shape.find(".//a:xfrm", NS)
    if transform is None:
        transform = shape.find(".//p:xfrm", NS)
    if transform is None:
        return None
    offset = transform.find("a:off", NS)
    extent = transform.find("a:ext", NS)
    if offset is None:
        offset = transform.find("p:off", NS)
    if extent is None:
        extent = transform.find("p:ext", NS)
    if offset is None or extent is None:
        return None
    return (
        offset.attrib.get("x", ""),
        offset.attrib.get("y", ""),
        extent.attrib.get("cx", ""),
        extent.attrib.get("cy", ""),
    )


def fail(message: str, errors: list[str]) -> None:
    errors.append(message)


def target_part(source_part: str, target: str) -> str:
    if target.startswith("/"):
        return posixpath.normpath(target.lstrip("/"))
    return posixpath.normpath(str(PurePosixPath(source_part).parent.joinpath(target)))


def inspect_pptx(path: Path) -> tuple[dict[str, Any], list[str]]:
    errors: list[str] = []
    result: dict[str, Any] = {
        "path": str(path.resolve()),
        "exists": path.is_file(),
        "sha256": sha256(path) if path.is_file() else None,
        "slide_count": 0,
        "editable_text_shapes": 0,
        "picture_shapes": 0,
        "slides": [],
        "package_parts": 0,
    }
    if not path.is_file():
        fail(f"missing PPTX: {path}", errors)
        return result, errors

    try:
        with zipfile.ZipFile(path) as package:
            bad_member = package.testzip()
            if bad_member:
                fail(f"ZIP CRC error in {bad_member}", errors)
            members = set(package.namelist())
            result["package_parts"] = len(members)
            required = {
                "[Content_Types].xml",
                "ppt/presentation.xml",
                "ppt/_rels/presentation.xml.rels",
            }
            missing = sorted(required - members)
            if missing:
                fail(f"missing required OOXML parts: {', '.join(missing)}", errors)
                return result, errors

            content_types = ET.fromstring(package.read("[Content_Types].xml"))
            if content_types.tag != f"{{{CONTENT_TYPES_NS}}}Types":
                fail("[Content_Types].xml has an unexpected root element", errors)
            overrides: dict[str, str] = {}
            for override in content_types.findall(f"{{{CONTENT_TYPES_NS}}}Override"):
                part_name = override.attrib.get("PartName", "")
                content_type = override.attrib.get("ContentType", "")
                if not part_name.startswith("/"):
                    fail(f"content-type override has a non-absolute PartName: {part_name!r}", errors)
                elif part_name in overrides:
                    fail(f"duplicate content-type override for {part_name}", errors)
                else:
                    overrides[part_name] = content_type
                    if part_name.lstrip("/") not in members:
                        fail(f"content-type override points to missing part: {part_name}", errors)

            expected_content_types = {
                "/ppt/presentation.xml": MAIN_PRESENTATION_CONTENT_TYPE,
            }
            for member in members:
                if member.startswith("ppt/slides/slide") and member.endswith(".xml"):
                    expected_content_types[f"/{member}"] = SLIDE_CONTENT_TYPE
                elif member.startswith("ppt/slideMasters/slideMaster") and member.endswith(".xml"):
                    expected_content_types[f"/{member}"] = SLIDE_MASTER_CONTENT_TYPE
                elif member.startswith("ppt/slideLayouts/slideLayout") and member.endswith(".xml"):
                    expected_content_types[f"/{member}"] = SLIDE_LAYOUT_CONTENT_TYPE
                elif member.startswith("ppt/theme/") and member.endswith(".xml"):
                    expected_content_types[f"/{member}"] = THEME_CONTENT_TYPE
            for part_name, expected_type in expected_content_types.items():
                actual_type = overrides.get(part_name)
                if actual_type != expected_type:
                    fail(
                        f"wrong or missing OOXML content type for {part_name}: "
                        f"expected {expected_type!r}, found {actual_type!r}",
                        errors,
                    )

            for rel_name in sorted(name for name in members if name.endswith(".rels")):
                rel_path = PurePosixPath(rel_name)
                source_name = rel_path.name[:-5]
                source_part = str(rel_path.parent.parent / source_name) if source_name else ""
                root = ET.fromstring(package.read(rel_name))
                for relationship in root.findall("pr:Relationship", NS):
                    if relationship.attrib.get("TargetMode") == "External":
                        continue
                    target = relationship.attrib.get("Target", "")
                    resolved = target_part(source_part, target)
                    if resolved not in members:
                        fail(f"relationship in {rel_name} points to missing part: {resolved}", errors)

            presentation = ET.fromstring(package.read("ppt/presentation.xml"))
            slide_size = presentation.find("p:sldSz", NS)
            result["slide_size_emu"] = (
                {"width": slide_size.attrib.get("cx"), "height": slide_size.attrib.get("cy")}
                if slide_size is not None else None
            )
            rels_root = ET.fromstring(package.read("ppt/_rels/presentation.xml.rels"))
            relationships = {
                node.attrib.get("Id", ""): node.attrib.get("Target", "")
                for node in rels_root.findall("pr:Relationship", NS)
                if node.attrib.get("Type", "") == PRESENTATION_REL
            }
            slide_ids = presentation.findall("p:sldIdLst/p:sldId", NS)
            slide_parts: list[str] = []
            for slide_id in slide_ids:
                rel_id = slide_id.attrib.get(f"{{{NS['r']}}}id", "")
                target = relationships.get(rel_id)
                if not target:
                    fail(f"presentation slide relationship missing for {rel_id!r}", errors)
                    continue
                part = target_part("ppt/presentation.xml", target)
                if part not in members:
                    fail(f"slide relationship points to missing part: {part}", errors)
                    continue
                slide_parts.append(part)

            result["slide_count"] = len(slide_parts)
            for slide_index, part in enumerate(slide_parts, start=1):
                root = ET.fromstring(package.read(part))
                slide_text: list[str] = []
                text_shapes = 0
                picture_shapes = len(root.findall(".//p:pic", NS))
                shape_signatures: list[dict[str, Any]] = []
                slide_tree = root.find("p:cSld/p:spTree", NS)
                rels_part = str(PurePosixPath(part).parent / "_rels" / f"{PurePosixPath(part).name}.rels")
                slide_relationships: dict[str, str] = {}
                if rels_part in members:
                    slide_rels_root = ET.fromstring(package.read(rels_part))
                    slide_relationships = {
                        node.attrib.get("Id", ""): node.attrib.get("Target", "")
                        for node in slide_rels_root.findall("pr:Relationship", NS)
                    }
                if slide_tree is None:
                    fail(f"slide {slide_index} has no shape tree", errors)
                    shape_nodes: list[ET.Element] = []
                else:
                    shape_nodes = [
                        node for node in list(slide_tree)
                        if node.tag.rsplit("}", 1)[-1] in {"sp", "pic", "graphicFrame", "cxnSp", "grpSp"}
                    ]
                for shape in root.findall(".//p:sp", NS):
                    text = shape_text(shape)
                    if text:
                        slide_text.append(text)
                        text_shapes += 1
                for shape in shape_nodes:
                    local_name = shape.tag.rsplit("}", 1)[-1]
                    nonvisual = shape.find(".//p:cNvPr", NS)
                    signature: dict[str, Any] = {
                        "kind": local_name,
                        "name": nonvisual.attrib.get("name", "") if nonvisual is not None else "",
                        "transform_emu": transform_signature(shape),
                    }
                    if local_name == "pic":
                        blip = shape.find(".//a:blip", NS)
                        embed_id = blip.attrib.get(f"{{{NS['r']}}}embed", "") if blip is not None else ""
                        target = slide_relationships.get(embed_id, "")
                        media_part = target_part(part, target) if target else ""
                        if media_part in members:
                            signature["embedded_media_sha256"] = hashlib.sha256(package.read(media_part)).hexdigest()
                        else:
                            signature["embedded_media_sha256"] = None
                    shape_signatures.append(signature)
                result["editable_text_shapes"] += text_shapes
                result["picture_shapes"] += picture_shapes
                result["slides"].append({
                    "number": slide_index,
                    "part": part,
                    "editable_text_shapes": text_shapes,
                    "picture_shapes": picture_shapes,
                    "shape_text": slide_text,
                    "text": clean_text(" ".join(slide_text)),
                    "shape_structure": shape_signatures,
                })
                if text_shapes == 0:
                    fail(f"slide {slide_index} has no editable text-bearing shape", errors)

            if not slide_parts:
                fail("presentation has no resolvable slide parts", errors)
    except (zipfile.BadZipFile, ET.ParseError, KeyError, OSError) as exc:
        fail(f"cannot parse PPTX package: {exc}", errors)
    return result, errors


def check_expected_content(deck: dict[str, Any], fixture: dict[str, Any], expected_status: str, errors: list[str]) -> None:
    slides = deck.get("slides", [])
    expected_slide_count = int(fixture["expected_slide_count"])
    if deck.get("slide_count") != expected_slide_count:
        fail(f"expected {expected_slide_count} slides, found {deck.get('slide_count')}", errors)
    if deck.get("editable_text_shapes", 0) < 8:
        fail(f"expected at least 8 editable text shapes, found {deck.get('editable_text_shapes', 0)}", errors)
    if slides and clean_text(str(fixture["title"])) not in slides[0].get("text", ""):
        fail("slide 1 does not contain the exact fixture title", errors)
    slide_titles = fixture.get("slide_titles", [])
    if len(slide_titles) != expected_slide_count:
        fail(f"fixture must provide {expected_slide_count} distinct slide titles", errors)
    elif len({clean_text(str(title)) for title in slide_titles}) != expected_slide_count:
        fail("fixture slide titles are not distinct", errors)
    else:
        for slide_index, expected_title in enumerate(slide_titles):
            if slide_index >= len(slides):
                break
            if clean_text(str(expected_title)) not in slides[slide_index].get("shape_text", []):
                fail(f"slide {slide_index + 1} is missing its exact editable title {expected_title!r}", errors)
    if len(slides) >= expected_slide_count and slides[3].get("editable_text_shapes", 0) < 1:
        fail("slide 4 must contain editable text", errors)
    if len(slides) >= expected_slide_count and clean_text(str(expected_status)) not in slides[3].get("text", ""):
        fail(f"slide 4 does not contain the expected status marker {expected_status!r}", errors)

    whole_text = clean_text(" ".join(slide.get("text", "") for slide in slides))
    for required in [fixture["title"], fixture["scope_disclosure"], expected_status]:
        if clean_text(str(required)) not in whole_text:
            fail(f"required text absent: {required!r}", errors)
    for fact in fixture["facts"]:
        required = clean_text(str(fact["exact_text"]))
        if required not in whole_text:
            fail(f"fixture fact absent or altered: {required!r}", errors)
    forbidden_status = fixture["revised_status"] if expected_status == fixture["initial_status"] else fixture["initial_status"]
    if clean_text(str(forbidden_status)) in whole_text:
        fail(f"unexpected status marker present: {forbidden_status!r}", errors)


def render_deck(path: Path, render_dir: Path, soffice: str | None, pdftoppm: str | None, expected_pages: int) -> dict[str, Any]:
    result: dict[str, Any] = {"status": "unavailable", "pdf": None, "pngs": [], "pages": [], "reason": None}
    soffice_path = soffice or shutil.which("soffice")
    pdftoppm_path = pdftoppm or shutil.which("pdftoppm")
    if not soffice_path or not pdftoppm_path:
        result["reason"] = "soffice and/or pdftoppm not found"
        return result
    render_dir.mkdir(parents=True, exist_ok=True)
    try:
        converted = subprocess.run(
            [soffice_path, "--headless", "--convert-to", "pdf", "--outdir", str(render_dir), str(path.resolve())],
            capture_output=True,
            text=True,
            check=False,
            timeout=120,
        )
        pdf = render_dir / f"{path.stem}.pdf"
        if converted.returncode != 0 or not pdf.is_file():
            result["status"] = "failed"
            result["reason"] = clean_text((converted.stderr or converted.stdout or "soffice conversion failed"))
            return result
        converted_pngs = subprocess.run(
            [pdftoppm_path, "-png", "-r", "120", str(pdf), str(render_dir / "slide")],
            capture_output=True,
            text=True,
            check=False,
            timeout=120,
        )
        if converted_pngs.returncode != 0:
            result["status"] = "failed"
            result["reason"] = clean_text((converted_pngs.stderr or converted_pngs.stdout or "pdftoppm failed"))
            return result
        result.update({
            "status": "rendered",
            "pdf": str(pdf.resolve()),
            "pdf_sha256": sha256(pdf),
            "pngs": [str(file.resolve()) for file in sorted(render_dir.glob("slide-*.png"))],
        })
        if not result["pngs"]:
            result["status"] = "failed"
            result["reason"] = "conversion returned no PNG pages"
        elif len(result["pngs"]) != expected_pages:
            result["status"] = "failed"
            result["reason"] = f"expected {expected_pages} rendered pages, found {len(result['pngs'])}"
        else:
            for png in sorted(render_dir.glob("slide-*.png")):
                dimensions = png_dimensions(png)
                result["pages"].append({
                    "path": str(png.resolve()),
                    "sha256": sha256(png),
                    "bytes": png.stat().st_size,
                    "width_px": dimensions[0] if dimensions else None,
                    "height_px": dimensions[1] if dimensions else None,
                })
    except (OSError, subprocess.TimeoutExpired) as exc:
        result["status"] = "failed"
        result["reason"] = str(exc)
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", required=True, type=Path)
    parser.add_argument("--before", required=True, type=Path, help="PPTX captured after turn 1, before turn 2")
    parser.add_argument("--after", required=True, type=Path, help="PPTX captured after turn 2")
    parser.add_argument("--out", required=True, type=Path, help="write JSON review report")
    parser.add_argument("--render-dir", type=Path, help="render after-turn slides to PDF/PNG when tools are available")
    parser.add_argument("--soffice", help="optional path to LibreOffice soffice")
    parser.add_argument("--pdftoppm", help="optional path to pdftoppm")
    args = parser.parse_args()

    report: dict[str, Any] = {"schema_version": 1, "status": "failed", "checks": [], "errors": [], "render": None}
    try:
        fixture = json.loads(args.fixture.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        report["errors"].append(f"cannot read fixture JSON: {exc}")
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(report, indent=2))
        return 1

    before, before_errors = inspect_pptx(args.before)
    after, after_errors = inspect_pptx(args.after)
    report["fixture"] = {
        "path": str(args.fixture.resolve()),
        "sha256": sha256(args.fixture),
        "nonce": fixture.get("nonce"),
        "artifact_basename": fixture.get("artifact_basename"),
    }
    report["before"] = before
    report["after"] = after
    report["errors"].extend([f"before: {e}" for e in before_errors])
    report["errors"].extend([f"after: {e}" for e in after_errors])
    if not before_errors:
        check_expected_content(before, fixture, fixture["initial_status"], report["errors"])
    if not after_errors:
        check_expected_content(after, fixture, fixture["revised_status"], report["errors"])

    if before.get("sha256") == after.get("sha256"):
        fail("PPTX SHA-256 did not change between user turns", report["errors"])
    if before.get("slide_count") != after.get("slide_count"):
        fail("slide count changed during the requested in-place edit", report["errors"])
    if before.get("slide_count") == after.get("slide_count") and before.get("slides") and after.get("slides"):
        initial = clean_text(str(fixture["initial_status"]))
        revised = clean_text(str(fixture["revised_status"]))
        before_text = [
            [clean_text(value).replace(initial, "<STATUS>") for value in slide.get("shape_text", [])]
            for slide in before["slides"]
        ]
        after_text = [
            [clean_text(value).replace(revised, "<STATUS>") for value in slide.get("shape_text", [])]
            for slide in after["slides"]
        ]
        if before_text != after_text:
            fail("slide text changed beyond the requested status replacement", report["errors"])
        before_structure = [slide.get("shape_structure", []) for slide in before["slides"]]
        after_structure = [slide.get("shape_structure", []) for slide in after["slides"]]
        if before_structure != after_structure:
            fail("slide shape order, names, geometry, or embedded images changed during the status-only edit", report["errors"])
    structural_errors = list(report["errors"])

    if args.render_dir:
        report["render"] = render_deck(
            args.after,
            args.render_dir,
            args.soffice,
            args.pdftoppm,
            int(fixture["expected_slide_count"]),
        )
        if report["render"]["status"] == "failed":
            report["errors"].append(f"render failed: {report['render']['reason']}")
    report["human_visual_review"] = {
        "status": "pending",
        "instruction": "Inspect each unmodified rendered PNG at full slide size; record findings in visual-review-template.md.",
    }
    report["checks"] = [
        {"name": "valid OOXML ZIP and resolvable presentation relationships", "status": "pass" if not before_errors and not after_errors else "fail"},
        {"name": "four slides with editable text and exact fixture content", "status": "pass" if not structural_errors else "fail"},
        {"name": "turn 2 changes INITIAL to REVISED and preserves fixture facts", "status": "pass" if before.get("sha256") != after.get("sha256") and not structural_errors else "fail"},
    ]
    if args.render_dir:
        render_status = report["render"]["status"]
        report["checks"].append({
            "name": "after-turn deck rendered to page images for human inspection",
            "status": "pass" if render_status == "rendered" else "fail" if render_status == "failed" else "inconclusive",
        })
    else:
        report["checks"].append({"name": "after-turn deck rendered to page images for human inspection", "status": "not requested"})

    report["status"] = "pass" if not report["errors"] else "failed"
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
