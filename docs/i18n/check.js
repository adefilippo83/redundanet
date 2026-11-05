// Checks that every data-i18n key in docs/**/*.html exists in every dictionary here, that all dictionaries share one key set, and that no value has an em or en dash: `node docs/i18n/check.js`.
"use strict";
const fs = require("fs");
const path = require("path");

const docs = path.join(__dirname, "..");
const langs = ["it", "es", "fr", "de", "pt", "zh", "ja", "ko", "ru"];
const skip = /^(talk|redundanet3|linuxday[a-z0-9]*)\.html$/;

function pages(dir) {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) return e.name === "i18n" ? [] : pages(p);
    return e.name.endsWith(".html") && !skip.test(e.name) ? [p] : [];
  });
}

const used = new Set();
const files = pages(docs);
for (const p of files) {
  const raw = fs.readFileSync(p, "utf8");
  for (const m of raw.matchAll(/data-i18n(?:-placeholder|-title)?="([^"]+)"/g)) used.add(m[1]);
}

let ok = true;
let ref = null;
for (const lang of langs) {
  const d = JSON.parse(fs.readFileSync(path.join(__dirname, lang + ".json"), "utf8"));
  const keys = new Set(Object.keys(d));
  const missing = [...used].filter((k) => !keys.has(k));
  const unused = [...keys].filter((k) => !used.has(k));
  const dashes = [...keys].filter((k) => /[–—]/.test(d[k]));
  const empty = [...keys].filter((k) => typeof d[k] !== "string" || !d[k].trim());
  for (const [name, list] of [["missing", missing], ["unused", unused], ["dashes", dashes], ["empty", empty]]) {
    if (list.length) {
      ok = false;
      console.log(`${lang}: ${name} ${list.length}: ${list.slice(0, 10).join(", ")}`);
    }
  }
  if (ref && (keys.size !== ref.size || [...keys].some((k) => !ref.has(k)))) {
    ok = false;
    console.log(`${lang}: key set differs from ${langs[0]}`);
  }
  ref = ref || keys;
  console.log(`${lang}: ${keys.size} keys`);
}
console.log(`${used.size} keys used across ${files.length} pages; ${ok ? "OK" : "PROBLEMS"}`);
process.exit(ok ? 0 : 1);
