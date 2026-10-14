# Independent PPTX review: live run 20261014-190417-930a46d9

**PPTX subcase: PASS. Full live run: still in progress; provider/tool provenance and context-compaction acceptance remain pending.** This review covers only the frozen two-deck artifact and rendered slides; it does not by itself establish that Dot created or edited them.

- Fixture: `terminal/20261014-190417-930a46d9/presentation-fixture.json`, SHA-256 `11f99f6dcb3da9b9b517116aeda3bcb8709733d18bd68db4064cd0c4dc84ba49`, nonce `A18BD3BD0108`.
- Frozen v1: `terminal/20261014-190417-930a46d9/presentation-before.pptx`, 7,161 bytes, SHA-256 `10f0a7be8452ea6fb241c2209f29e235aae7e4ef1d7dbd42b9bf147758c5d26b`.
- Frozen v2: `terminal/20261014-190417-930a46d9/presentation-after.pptx`, 7,179 bytes, SHA-256 `a44eada22036438be5dd01a3c86def640a74408e78c40d82c6f7e996e18d1d90`.
- Live workspace artifact SHA-256 `a44eada22036438be5dd01a3c86def640a74408e78c40d82c6f7e996e18d1d90` matches frozen v2 byte-for-byte.
- Both decks parse as valid OOXML ZIP packages with 16 parts, resolvable presentation relationships, four slides, 13 editable text shapes, no picture shapes, and canonical main-part content type `application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml`.
- The independent structural verifier passed. Slide count, text shape order, geometry, and fixture content are preserved; the only requested text change is slide 4 `Release status: INITIAL` → `Release status: REVISED`.
- The real helper snapshots are `1c62f8cfde73e951534a70e511560e133a317271236a60978cbdaee7d4ab8e4d` (after create) and `eee70772f5d67b54e5892422b00f44dd03cf85a0d18df58c92890606ed84bd64` (after edit). Their diff changes only `releaseStatus` from `INITIAL` to `REVISED`. Full run report/provider tool-call and receipt joins are still required to establish that the live Dot turn modified this same helper and existing deck.
- I independently rendered the after deck with bundled LibreOffice and `pdftoppm`. All four independent render PNG hashes match the harness PNG hashes exactly: `6d2df335ffb58ce3adbe70b8666abd541a136341c6f6bddde62c13d22d8b3ab8, c403e5fd575e6754edf2ebd324063b359317fd4a123931dbda3eb0486cea4fe5, 54bbcea27f714b210fdfff27262382b9f0e0d97e0c6556478191953a2a8f8d69, 236ec41151ad3a0e6eb4b93f88bee52f3c1dacfe8d10e4c713903b82fecc2dff`.

## Full-size visual findings

All four unmodified 1601×900 renders were inspected at original size. Text remains readable, with no clipped text, overlap, empty page, or broken image. The synthetic-QA disclosure appears on all four slides. Slide 4's `REVISED` value is visible. The deck uses a sparse, restrained layout with ample whitespace; title/body placement and color contrast are consistent throughout.

The independent verifier output is `review/190417-independent/pptx-review.json`; PNGs and PDF are in `review/190417-independent/render/`. Human visual review is marked pass in that JSON. The overall live run remains pending until its provider/session evidence is independently checked.
