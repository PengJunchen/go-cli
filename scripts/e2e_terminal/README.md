# Real PTY terminal E2E

This harness builds and launches the actual `go-cli interactive` binary on a
PTY, serves model responses from a localhost-only HTTP fixture, forwards the
PTY byte stream into xterm.js, and captures the rendered terminal with
headless Chromium. The PNGs are real terminal rendering evidence; they are not
screenshots of a native Terminal window.

Live PTY validation is currently verified on macOS. The harness uses Unix PTY
APIs and is intended to support Linux too, but Linux has not been runtime-tested
here. Windows is not supported by this PTY harness.

## Requirements

- Go 1.24.2 or newer to build the CLI (omit Go only when using `--binary` with
  an existing executable).
- Git and RTK; the harness uses `rtk proxy git` for unformatted Git metadata.
- Python 3.10 or newer (standard library only), with Unix PTY support.
- Node.js 20 or newer and npm, to run the pinned Playwright and xterm.js
  packages installed by `npm ci`.
- Google Chrome, or Playwright's managed Chromium installation.

## Run

Use Node.js 20 or newer. Install the pinned JavaScript dependencies once:

```sh
cd scripts/e2e_terminal
npm ci
cd ../..
rtk python3 scripts/e2e_terminal/run.py
```

Temporary run data is created under Python's `tempfile.gettempdir()` by
default. Select another existing scratch directory with
`--scratch-root /path/to/scratch` when needed.

The script uses `node` from `PATH` and resolves Playwright from the local
`scripts/e2e_terminal/node_modules` installation. On macOS it uses the standard
Google Chrome application path when present; otherwise Playwright uses its
installed Chromium browser. Set `E2E_CHROME_PATH` to use a different Chrome or
Chromium executable.

To use a bundled or separately installed runtime, provide its paths explicitly:

```sh
E2E_NODE=/path/to/node \
E2E_PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
E2E_CHROME_PATH='/path/to/Google Chrome' \
rtk python3 scripts/e2e_terminal/run.py
```

`E2E_PLAYWRIGHT_MODULE` and `E2E_CHROME_PATH` are optional overrides. To use
Playwright's managed Chromium, install it with `npx playwright install chromium`
from this directory and omit `E2E_CHROME_PATH`.

The harness stores PNGs, a JSON run report, and a Markdown evidence summary in
`docs/qa/2026-10-14/terminal/`. Each scenario uses a temporary `HOME` and
workspace. The provider endpoint is configured to use the localhost mock
server, and the CLI receives only a fixture API key, never a live model
credential. The child process removes inherited `NO_COLOR` so the screenshots
exercise the normal xterm color profile even when the operator's shell disables
colors.
