# Live PPTX visual review: `20261014-174155-f895248a`

This record is limited to the Dot-produced presentation artifacts. The overall artifact-focus run failed later during a large-context turn; that failure is documented separately and this deck review does not upgrade the run to an end-to-end pass.

## Provenance

- Fixture: `docs/qa/2026-10-14/live/terminal/20261014-174155-f895248a/presentation-fixture.json`
- Fixture SHA-256: `10c0885c299aff28fb516b56a7076611ca9e56bb213a6f116b90c14575fec50d`
- Turn-1 snapshot: `docs/qa/2026-10-14/live/terminal/20261014-174155-f895248a/presentation-before.pptx`
- Turn-1 SHA-256: `202165d15d777b86694555d12946f285a79ab724f104fb7462ce986c2b97883a`
- Turn-2 snapshot: `docs/qa/2026-10-14/live/terminal/20261014-174155-f895248a/presentation-after.pptx`
- Turn-2 SHA-256: `11a97de1cb7575c6c0f41b214bf9d3bd1cc2ac794cae2d19b9686372c8ce940e`
- Verifier: `docs/qa/2026-10-14/live/review/174155-pptx-review.json` — `pass`, no errors
- Render: `docs/qa/2026-10-14/live/review/174155-pptx-render/presentation-after.pdf`, SHA-256 `9389a79b9f12bf4aee5ceebd6a0f724ccaff4aa28edc04efe5d76a5369b5e24a`
- Renderer: LibreOffice headless and `pdftoppm`, 120 DPI; each PNG is 1601 × 900 pixels
- Reviewer: independent visual review, 2026-10-14

## Full-size slide inspection

| Slide | PNG and SHA-256 | Full-size observations | Result |
| ---: | --- | --- | --- |
| 1 | `pptx-render/slide-1.png` — `58931c53ec5a4199ea61be877770714b93297b881dee0f7e373ab0627bdb92b9` | Nonce title, subtitle, and synthetic-input disclosure are readable with strong contrast. Minimal cover composition; no clipping or overlap. | PASS |
| 2 | `pptx-render/slide-2.png` — `a51aca7eb963dfa870f9d9e42981e218cc472b8567ec159e7924f30cc0b2c7e8` | Title, exact synthetic checks metric, and disclosure are readable. The slide is sparse, with ample whitespace; no clipping or overlap. | PASS |
| 3 | `pptx-render/slide-3.png` — `bf0969c9d085e6e38c1f481fe54640c4c8c02f64f19a7197d0ebe494f6e1c3c3` | Both exact synthetic metrics, the title, and disclosure fit and remain legible. No clipping, overlap, or broken visual assets. | PASS |
| 4 | `pptx-render/slide-4.png` — `c1e5c55a6c1d1ea23e4453d40c4bd940497f0f365311a31de75534e8545cdb78` | `Release status: REVISED` is visible beneath the title. Disclosure is readable; no clipping or overlap. | PASS |

## Decision

**Visual result: PASS for the four rendered slides.** The deck uses a restrained, highly sparse text-only layout. All content is legible at the full 1601 × 900 render size, all slides have a clear title and sufficient contrast, and no text or shape is clipped or overlapping. The status change is visible on slide 4.

The independent OOXML verifier also passed: both files are valid four-slide packages with 13 editable text shapes and zero picture shapes; the turn-2 artifact hash differs; the four exact titles, nonce, fixture facts, and disclosure remain; and its text/shape-structure comparison found only the requested slide-4 status replacement. The Dot workspace `live_review_3A61DCA80B0C.pptx` after-turn SHA-256 matches the frozen turn-2 snapshot.

This review used a LibreOffice-rendered PDF/PNG, not PowerPoint. The presentation run is not accepted as a whole: the subsequent real context-compaction attempt ended with a durable `run_outcome=error`, and no post-compaction canary recall was completed.
