/*
 * Lightweight client-side i18n for the RedundaNet website.
 *
 * - English lives in the HTML (the source of truth). Elements that should be
 *   translatable carry a `data-i18n="key"` attribute (and `data-i18n-placeholder`
 *   for input placeholders). The <html> element may carry `data-i18n-title`.
 * - Per-language dictionaries live in /i18n/<lang>.json mapping key -> string.
 *   Missing keys gracefully fall back to the original English from the DOM.
 * - Language is chosen from ?lang=, then localStorage, then navigator.language.
 */
(function () {
  "use strict";

  var SUPPORTED = ["en", "it", "es", "fr", "de"];
  var NAMES = { en: "English", it: "Italiano", es: "Español", fr: "Français", de: "Deutsch" };
  var STORAGE_KEY = "rn_lang";
  var dicts = {}; // lang -> { key: value }

  function initialLang() {
    try {
      var q = new URLSearchParams(window.location.search).get("lang");
      if (q && SUPPORTED.indexOf(q) !== -1) return q;
      var stored = window.localStorage.getItem(STORAGE_KEY);
      if (stored && SUPPORTED.indexOf(stored) !== -1) return stored;
    } catch (e) {
      /* ignore */
    }
    var nav = (navigator.language || "en").slice(0, 2).toLowerCase();
    return SUPPORTED.indexOf(nav) !== -1 ? nav : "en";
  }

  function cacheOriginals() {
    document.querySelectorAll("[data-i18n]").forEach(function (el) {
      if (el.getAttribute("data-i18n-orig") === null) {
        el.setAttribute("data-i18n-orig", el.innerHTML);
      }
    });
    document.querySelectorAll("[data-i18n-placeholder]").forEach(function (el) {
      if (el.getAttribute("data-i18n-ph-orig") === null) {
        el.setAttribute("data-i18n-ph-orig", el.getAttribute("placeholder") || "");
      }
    });
    var root = document.documentElement;
    if (root.getAttribute("data-i18n-title-orig") === null) {
      root.setAttribute("data-i18n-title-orig", document.title);
    }
  }

  function apply(lang) {
    var dict = dicts[lang] || {};
    document.querySelectorAll("[data-i18n]").forEach(function (el) {
      var key = el.getAttribute("data-i18n");
      var v = dict[key];
      el.innerHTML = v != null ? v : el.getAttribute("data-i18n-orig");
    });
    document.querySelectorAll("[data-i18n-placeholder]").forEach(function (el) {
      var key = el.getAttribute("data-i18n-placeholder");
      var v = dict[key];
      el.setAttribute("placeholder", v != null ? v : el.getAttribute("data-i18n-ph-orig"));
    });
    var root = document.documentElement;
    var tkey = root.getAttribute("data-i18n-title");
    if (tkey) {
      var tv = dict[tkey];
      document.title = tv != null ? tv : root.getAttribute("data-i18n-title-orig");
    }
    root.lang = lang;
    try {
      window.localStorage.setItem(STORAGE_KEY, lang);
    } catch (e) {
      /* ignore */
    }
    document.querySelectorAll("[data-lang-option]").forEach(function (b) {
      b.classList.toggle("active", b.getAttribute("data-lang-option") === lang);
    });
  }

  function setLang(lang) {
    if (SUPPORTED.indexOf(lang) === -1) lang = "en";
    if (lang === "en" || dicts[lang]) {
      apply(lang);
      return;
    }
    fetch("/i18n/" + lang + ".json")
      .then(function (r) {
        if (!r.ok) throw new Error("load failed");
        return r.json();
      })
      .then(function (d) {
        dicts[lang] = d;
        apply(lang);
      })
      .catch(function () {
        apply("en");
      });
  }

  function injectStyles() {
    var css =
      ".lang-switcher{display:inline-flex;gap:4px;align-items:center;margin-left:16px}" +
      ".lang-btn{background:transparent;border:1px solid var(--border,#334155);color:var(--text-muted,#94a3b8);" +
      "border-radius:6px;padding:3px 7px;font:600 0.72rem/1 inherit;cursor:pointer;transition:all .15s}" +
      ".lang-btn:hover{color:var(--text,#f8fafc);border-color:var(--primary,#6366f1)}" +
      ".lang-btn.active{background:var(--primary,#6366f1);color:#fff;border-color:var(--primary,#6366f1)}" +
      "@media(max-width:768px){.lang-switcher{margin-left:0;flex-wrap:wrap}}";
    var style = document.createElement("style");
    style.textContent = css;
    document.head.appendChild(style);
  }

  function buildSwitcher() {
    var host = document.querySelector("[data-lang-switcher]");
    if (!host) return;
    SUPPORTED.forEach(function (lang) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "lang-btn";
      b.setAttribute("data-lang-option", lang);
      b.textContent = lang.toUpperCase();
      b.title = NAMES[lang];
      b.addEventListener("click", function () {
        setLang(lang);
      });
      host.appendChild(b);
    });
  }

  function init() {
    cacheOriginals();
    injectStyles();
    buildSwitcher();
    setLang(initialLang());
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
