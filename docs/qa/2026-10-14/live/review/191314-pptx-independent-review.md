# Independent PPTX review: live run 20261014-191314-3ac94c9e

**PPTX subcase: PASS. Overall live run: PASS.** This review covers the frozen two-deck artifact and full-size renders. Provider and tool-call attribution was separately joined in the [independent live-run review](191314-independent-live-review.md).

- Fixture: `terminal/20261014-191314-3ac94c9e/presentation-fixture.json`, SHA-256 `5417a7cfe37891c16e263fb8af7009bce2eac75e2aec68069a8c6badb3fa1bab`, nonce `E08CCC4BB0E4`.
- Frozen v1: `terminal/20261014-191314-3ac94c9e/presentation-before.pptx`, 7,694 bytes, SHA-256 `cca4816269fe1a542503b913c6736fffff2bee83707fdfe395ff4fbdc6a60c7f`.
- Frozen v2: `terminal/20261014-191314-3ac94c9e/presentation-after.pptx`, 7,712 bytes, SHA-256 `5fd55eee190ea5775a8dfa0b756706c0ed9fd58359e5682d79ea560b67eeb9a8`.
- The workspace artifact `live_review_E08CCC4BB0E4.pptx` has the same SHA-256 as frozen v2.
- Both decks have canonical main-part MIME `application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml`, valid OOXML ZIP packages, four slides, 13 editable text shapes and no picture shapes. The packages each contain 16 parts; only `ppt/slides/slide4.xml` differs between v1 and v2.
- The independent structural verifier passed: slide titles, fixture facts, disclosure and editable structure are preserved; slide 4 changes only `Release status: INITIAL` to `Release status: REVISED`.
- The model helper snapshots are 9,200 bytes each. Their SHA-256 hashes are `3170820767d3ebf1e4270617a50916e44910f79593ec268dd4d4e931372a5cb9` (after create) and `2205ff04e028b802e6391108a93f3e811529fba4038af33d71300f4f4c763c2b` (after edit). The source diff changes only the release-status constant from `INITIAL` to `REVISED`; the title nonce, target basename and remaining helper code are unchanged. The full-run ledger joins both distinct user turns, upstream responses, model-requested calls and durable successful receipts.
- I independently rendered v2 with the bundled LibreOffice and `pdftoppm`. The four independent PNG hashes match the harness-rendered PNGs exactly: `1cce556dcf727f0bda625301f5c4451825e914352fb5da7afde544d43711fd95`, `49acc0614677e0b6b392141aef0f99042e2027dd3a97a2510bc9be334ec14e83`, `56e351710ca04911dbbb6beca76081eccda0038a531ba437948cc1efdc6e0f39`, and `29b4f8ac3863166fc02855a73b5f35f8a3ec93cd56647e8f01823a392672775a`. All are 1601×900.

## Full-size visual findings

All four unmodified 1601×900 render images were inspected at original size. Text is readable, with no clipping, overlap, empty slide or broken image. The exact synthetic-QA disclosure is visible on all slides. Slide 4 visibly shows `REVISED`. The deck uses a consistent sparse layout, clear title/body hierarchy, generous whitespace and consistent left alignment. The sparse treatment suits this brief; it is visually plain but does not impair comprehension.

The independent verifier report, PDF and PNGs are under `review/191314-independent/`. The report's human visual review field records this inspection. The harness-owned `terminal/.../pptx-review.json` is left unchanged.
