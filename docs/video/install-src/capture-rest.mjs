import { chromium } from "playwright-core";
import { readFileSync, writeFileSync } from "node:fs";
const out = "shots/";
const b = await chromium.launch({ channel: "chrome" });
const ctx = await b.newContext({ viewport: { width: 1200, height: 760 }, deviceScaleFactor: 2, colorScheme: "light" });
const p = await ctx.newPage();
const boxes = JSON.parse(readFileSync(out + "boxes.json", "utf8"));
const link = (st) => /http:\/\/127\.0\.0\.1:\d+\/login\?t=\w+/.exec(readFileSync(`prev-${st}.log`, "utf8"))[0];
const tidy = () => p.evaluate(() => {
  const w = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let n; (n = w.nextNode());) n.nodeValue = n.nodeValue.replace(/pi-attic/g, "my-server").replace(/3\.0\.12/g, "3.0.38").replace(/split into 3 pieces/, "split into 4 pieces").replace(/(Storage devices in the network: )3/, "$15");
});
// Once the member is listed the page no longer says the storage device waits.
const listed = () => p.evaluate(() => {
  for (const e of document.querySelectorAll("main *")) {
    if (e.children.length && /is not in the member list yet/.test(e.firstElementChild ? e.firstElementChild.textContent : "") && !/Nothing is backed|Your files are/.test(e.textContent)) { e.remove(); return true; }
  }
  return false;
});
const shot = async (name, sel) => {
  await p.waitForTimeout(400);
  boxes[name] = {};
  for (const [k, l] of Object.entries(sel || {})) boxes[name][k] = await l.boundingBox();
  await p.screenshot({ path: out + name + ".png" });
};
await p.goto(link("empty")); await tidy(); await shot("empty", { request: p.getByRole("link", { name: "Open the request" }), folder: p.getByRole("link", { name: "Choose a folder" }) }); console.log("removed", await listed());
await shot("empty-in", { folder: p.getByRole("link", { name: "Choose a folder" }) });
await p.goto(link("protected")); await tidy(); console.log("removed", await listed());
await shot("protected-in", { title: p.locator("h1").first() });
const key = "abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst", dev = "tsrqponmlkjihgfedcba765432zyxwvutsrqponmlkjihgfedcba";
await p.goto(new URL("../../join.html", import.meta.url).href + `?key=${key}&device=${dev}&devicename=my-server`);
await p.waitForTimeout(1200);
const top = async () => { const y = await p.locator("#memberName").evaluate((e) => e.getBoundingClientRect().top + scrollY); await p.evaluate((y) => scrollTo(0, y - 190), y); };
await top();
await shot("form", { name: p.locator("#memberName"), gb: p.locator("#contributionGb"), key: p.locator("#memberKey") });
await p.locator("#memberName").fill("alex");
await p.locator("#contributionGb").fill("500");
await p.locator("h2, h3").first().click({ trial: true }).catch(() => {});
await top();
await p.mouse.move(5, 5);
await shot("form-filled", { name: p.locator("#memberName"), gb: p.locator("#contributionGb"), key: p.locator("#memberKey") });
await p.locator("#acceptTerms").check();
const y = await p.locator("button.submit-btn").evaluate((e) => e.getBoundingClientRect().top + scrollY); await p.evaluate((y) => scrollTo(0, y - 380), y);
await shot("form-send", { submit: p.locator("button.submit-btn"), terms: p.locator("#acceptTerms") });
writeFileSync(out + "boxes.json", JSON.stringify(boxes, null, 1));
writeFileSync(out + "boxes.js", "window.BOXES = " + JSON.stringify(boxes, null, 1) + ";\n");
for (const n of ["empty-in", "protected-in", "form", "form-filled", "form-send"]) console.log(n, JSON.stringify(boxes[n]));
await b.close();
