# Live PPT acceptance fixture

This fixture is input for the live Dot session. It does not create or modify a PowerPoint file. Generate a fresh `fixture.json` for each run with `scripts/e2e_live_review/generate_fixture.py`, then send the generated prompts to Dot in two separate user turns.

The JSON carries a fresh nonce, four exact distinct slide titles, three synthetic QA measurements, exact required slide text, and two prompts. All numbers are intentionally generated test data, not measurements of Dot, Magpie, or any product. Keep the generated JSON next to the run evidence and record its SHA-256.

The first prompt requests an editable four-slide PPTX in the run workspace. The second prompt is sent as a new CLI turn in the same session after the first file exists. It asks Dot to change `Release status: INITIAL` to `Release status: REVISED`. The second-turn prompt must not be submitted until the first-turn tool calls complete and the verifier has captured the initial PPTX hash.

The independent verifier checks the actual output package. It does not author slides, repair slides, or accept a harness-generated deck as Dot output. Render the returned file to PNG for visual review; structural validation alone is not a visual pass.
