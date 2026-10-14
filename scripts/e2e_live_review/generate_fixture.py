#!/usr/bin/env python3
"""Generate a nonce-bound prompt fixture for a real Dot presentation run."""

from __future__ import annotations

import argparse
import json
import re
import secrets
from pathlib import Path


def make_fixture(nonce: str | None = None) -> dict[str, object]:
    nonce = nonce or secrets.token_hex(5).upper()
    nonce = nonce.upper()
    if not re.fullmatch(r"[A-F0-9]{8,24}", nonce):
        raise ValueError("nonce must contain 8 to 24 hexadecimal characters")
    completed = secrets.randbelow(31) + 50
    total = 100
    latency_tenths = secrets.randbelow(31) + 20
    retained = secrets.randbelow(4) + 7
    retained_total = 10

    facts = [
        {
            "label": "Synthetic checks completed",
            "value": f"{completed} of {total}",
            "exact_text": f"Synthetic checks completed: {completed} of {total}",
        },
        {
            "label": "Synthetic median tool cycle",
            "value": f"{latency_tenths // 10}.{latency_tenths % 10} seconds",
            "exact_text": f"Synthetic median tool cycle: {latency_tenths // 10}.{latency_tenths % 10} seconds",
        },
        {
            "label": "Synthetic review facts retained",
            "value": f"{retained} of {retained_total}",
            "exact_text": f"Synthetic review facts retained: {retained} of {retained_total}",
        },
    ]
    title = f"LIVE_REVIEW_{nonce}"
    slide_titles = [
        title,
        "What Was Checked",
        "Tool Cycle and Retained Facts",
        "Release Status",
    ]
    initial_status = "Release status: INITIAL"
    revised_status = "Release status: REVISED"
    scope_disclosure = "Synthetic QA inputs only - not product measurements"

    first_prompt = f"""Create an editable four-slide PowerPoint at `live_review_{nonce}.pptx` in the current workspace. Use the actual tools available in this CLI to create the file, then inspect the saved file and report its path and slide count.

Use only the supplied fixture facts below. Label the figures synthetic on the slides; do not present them as measured product performance or add outside factual claims.

Required content:
- Use these exact, distinct editable slide titles in order: {json.dumps(slide_titles, ensure_ascii=False)}.
- A concise four-slide briefing with distinct titles and editable text shapes (not slide-sized screenshots).
- Include each exact metric line verbatim on a slide: `{facts[0]['exact_text']}`; `{facts[1]['exact_text']}`; `{facts[2]['exact_text']}`.
- Include the exact disclosure `{scope_disclosure}`.
- Include the exact marker `{initial_status}` on slide 4.
- Keep the writing plain and the visual hierarchy clear. Do not add unsupported numbers, citations, or claims.

This is turn 1 of a live tool-use acceptance run. The file must be created through model-requested CLI tool calls; do not claim success before the file exists and has been inspected.
"""
    second_prompt = f"""Edit the existing `live_review_{nonce}.pptx` in place using the available CLI tools. This is a separate follow-up user turn. Change only the slide-4 status marker `{initial_status}` to `{revised_status}`. Preserve the four-slide structure, exact title, all three exact metric lines, and disclosure. Save the edited PPTX, inspect it, and report the changed text and path. Do not create a replacement from scratch or claim success before verifying the existing file changed.
"""

    return {
        "schema_version": 1,
        "fixture_kind": "synthetic-live-presentation-acceptance",
        "nonce": nonce,
        "artifact_basename": f"live_review_{nonce}.pptx",
        "expected_slide_count": 4,
        "title": title,
        "slide_titles": slide_titles,
        "facts": facts,
        "scope_disclosure": scope_disclosure,
        "initial_status": initial_status,
        "revised_status": revised_status,
        "first_turn_prompt": first_prompt,
        "second_turn_prompt": second_prompt,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, help="write fixture JSON here; stdout when omitted")
    parser.add_argument("--nonce", help="reuse the live harness nonce so prompts and artifacts share one run ID")
    args = parser.parse_args()
    fixture = make_fixture(args.nonce)
    encoded = json.dumps(fixture, ensure_ascii=False, indent=2) + "\n"
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(encoded, encoding="utf-8")
    else:
        print(encoded, end="")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
