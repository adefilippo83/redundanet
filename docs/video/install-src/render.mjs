// Renders the film to PNG frames with the Chrome on this machine.
//   node render.mjs frames            every frame at 30 a second
//   node render.mjs stills 2 7 12     single stills at those seconds
import { chromium } from "playwright-core";
import { mkdirSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const [mode, ...rest] = process.argv.slice(2);
const FPS = 30;

const browser = await chromium.launch({ channel: "chrome" });
const page = await browser.newPage({ viewport: { width: 1920, height: 1080 }, deviceScaleFactor: 1 });
await page.goto(pathToFileURL(path.join(here, "index.html")).href + "?still");
await page.evaluate(() => window.ready);
for (const f of ["600 46px Inter", "700 46px Inter", "500 46px Inter", "400 32px 'JetBrains Mono'", "700 32px 'JetBrains Mono'"]) await page.evaluate((f) => document.fonts.load(f), f);
const duration = await page.evaluate(() => window.DURATION);
const canvas = page.locator("#c");

async function shot(t, file) {
  await page.evaluate((t) => window.seek(t), t);
  await canvas.screenshot({ path: file });
}

if (mode === "stills") {
  mkdirSync(path.join(here, "stills"), { recursive: true });
  for (const s of rest) await shot(parseFloat(s), path.join(here, "stills", `t${String(s).padStart(4, "0")}.png`));
} else {
  mkdirSync(path.join(here, "frames"), { recursive: true });
  const n = duration * FPS;
  for (let i = 0; i < n; i++) await shot(i / FPS, path.join(here, "frames", `f${String(i).padStart(4, "0")}.png`));
}
await browser.close();
