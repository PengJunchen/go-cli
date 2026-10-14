#!/usr/bin/env node
"use strict";

const fs = require("node:fs");
const path = require("node:path");
const readline = require("node:readline");
const { chromium } = process.env.E2E_PLAYWRIGHT_MODULE
  ? require(process.env.E2E_PLAYWRIGHT_MODULE)
  : require("playwright");

const root = __dirname;
const xtermDir = path.join(root, "node_modules", "@xterm", "xterm", "lib");
const fitDir = path.join(root, "node_modules", "@xterm", "addon-fit", "lib");
const chromePath = process.env.E2E_CHROME_PATH || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";

function reply(value) {
  process.stdout.write(`${JSON.stringify(value)}\n`);
}

async function main() {
  const viewport = {
    width: Number(process.env.E2E_VIEWPORT_WIDTH || 1440),
    height: Number(process.env.E2E_VIEWPORT_HEIGHT || 900),
  };
  const browser = await chromium.launch({
    headless: true,
    executablePath: fs.existsSync(chromePath) ? chromePath : undefined,
    args: ["--disable-dev-shm-usage"],
  });
  const page = await browser.newPage({
    viewport,
    deviceScaleFactor: 1,
  });
  await page.setContent(`<!doctype html><html><head><meta charset="utf-8"></head><body><div id="terminal"></div></body></html>`);
  await page.addStyleTag({ path: path.join(root, "node_modules", "@xterm", "xterm", "css", "xterm.css") });
  await page.addStyleTag({ content: `html,body,#terminal{width:100%;height:100%;margin:0;overflow:hidden;background:#0b0d10}body{padding:0}#terminal{box-sizing:border-box;padding:8px}` });
  await page.addScriptTag({ path: path.join(xtermDir, "xterm.js") });
  await page.addScriptTag({ path: path.join(fitDir, "addon-fit.js") });
  const dimensions = await page.evaluate(() => {
    const term = new Terminal({
      allowProposedApi: true,
      cursorBlink: false,
      fontFamily: "Menlo, Monaco, 'SF Mono', monospace",
      fontSize: 14,
      lineHeight: 1.15,
      scrollback: 4000,
      theme: {
        background: "#0b0d10",
        foreground: "#d8dee9",
        cursor: "#d8dee9",
        selectionBackground: "#40536b",
      },
    });
    const fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(document.getElementById("terminal"));
    fit.fit();
    window.__e2eTerminal = term;
    window.__e2eFit = fit;
    return { cols: term.cols, rows: term.rows };
  });
  await page.waitForTimeout(250);
  reply({ ready: true, ...dimensions });

  const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  let serial = Promise.resolve();
  rl.on("line", (line) => {
    serial = serial.then(async () => {
      const request = JSON.parse(line);
      if (request.op === "write") {
        const bytes = Buffer.from(request.data, "base64");
        await page.evaluate((encoded) => new Promise((resolve) => {
          const raw = atob(encoded);
          const data = new Uint8Array(raw.length);
          for (let i = 0; i < raw.length; i += 1) data[i] = raw.charCodeAt(i);
          window.__e2eTerminal.write(data, resolve);
        }), bytes.toString("base64"));
        await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        reply({ ok: true });
        return;
      }
      if (request.op === "snapshot") {
        const terminal = await page.evaluate(() => {
          const buffer = window.__e2eTerminal.buffer.active;
          const start = buffer.viewportY;
          const lines = [];
          for (let i = start; i < start + window.__e2eTerminal.rows; i += 1) {
            const line = buffer.getLine(i);
            if (line) lines.push(line.translateToString(true));
          }
          let coloredCells = 0;
          let occupiedCells = 0;
          for (let row = 0; row < window.__e2eTerminal.rows; row += 1) {
            const line = buffer.getLine(start + row);
            if (!line) continue;
            for (let col = 0; col < window.__e2eTerminal.cols; col += 1) {
              const cell = line.getCell(col);
              if (!cell) continue;
              if (cell.getChars()) occupiedCells += 1;
              if (cell.getFgColorMode() !== 0) coloredCells += 1;
            }
          }
          return {
            lines,
            cols: window.__e2eTerminal.cols,
            rows: window.__e2eTerminal.rows,
            colored_cells: coloredCells,
            occupied_cells: occupiedCells,
          };
        });
        await page.locator("#terminal").screenshot({ path: request.path, animations: "disabled" });
        reply({ ok: true, ...terminal });
        return;
      }
      if (request.op === "screen") {
        const terminal = await page.evaluate(() => {
          const buffer = window.__e2eTerminal.buffer.active;
          const lines = [];
          for (let i = buffer.viewportY; i < buffer.viewportY + window.__e2eTerminal.rows; i += 1) {
            const line = buffer.getLine(i);
            if (line) lines.push(line.translateToString(true));
          }
          return {
            lines,
            cols: window.__e2eTerminal.cols,
            rows: window.__e2eTerminal.rows,
          };
        });
        reply({ ok: true, ...terminal });
        return;
      }
      if (request.op === "resize") {
        await page.setViewportSize({ width: request.width, height: request.height });
        const dimensions = await page.evaluate(() => {
          window.__e2eFit.fit();
          return { cols: window.__e2eTerminal.cols, rows: window.__e2eTerminal.rows };
        });
        await page.waitForTimeout(100);
        reply({ ok: true, ...dimensions });
        return;
      }
      if (request.op === "close") {
        await browser.close();
        reply({ ok: true });
        process.exit(0);
      }
      throw new Error(`unsupported operation: ${request.op}`);
    }).catch((error) => {
      reply({ ok: false, error: String(error && error.stack || error) });
    });
  });
}

main().catch((error) => {
  reply({ ready: false, error: String(error && error.stack || error) });
  process.exitCode = 1;
});
