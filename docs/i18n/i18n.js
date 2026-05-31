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

  var SUPPORTED = ["en", "it", "es", "fr", "de", "pt", "zh", "ja", "ko", "ru"];
  var NAMES = {
    en: "English",
    it: "Italiano",
    es: "Español",
    fr: "Français",
    de: "Deutsch",
    pt: "Português",
    zh: "中文",
    ja: "日本語",
    ko: "한국어",
    ru: "Русский",
  };
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
    var sel = document.querySelector("[data-lang-select]");
    if (sel) sel.value = lang;
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
      ".lang-switcher{position:relative;display:inline-flex;align-items:center;margin-left:16px}" +
      ".lang-switcher::after{content:'\\25BE';position:absolute;right:9px;top:50%;" +
      "transform:translateY(-50%);pointer-events:none;color:var(--text-muted,#94a3b8);font-size:0.7rem}" +
      ".lang-select{appearance:none;-webkit-appearance:none;background:var(--bg-card,#1e293b);" +
      "color:var(--text,#f8fafc);border:1px solid var(--border,#334155);border-radius:6px;" +
      "padding:6px 26px 6px 10px;font:600 0.8rem/1 inherit;cursor:pointer;transition:border-color .15s}" +
      ".lang-select:hover{border-color:var(--primary,#6366f1)}" +
      ".lang-select:focus{outline:none;border-color:var(--primary,#6366f1)}" +
      ".lang-select option{background:var(--bg-card,#1e293b);color:var(--text,#f8fafc)}" +
      "@media(max-width:768px){.lang-switcher{margin-left:0}}";
    var style = document.createElement("style");
    style.textContent = css;
    document.head.appendChild(style);
  }

  function buildSwitcher() {
    var host = document.querySelector("[data-lang-switcher]");
    if (!host) return;
    var select = document.createElement("select");
    select.className = "lang-select";
    select.setAttribute("data-lang-select", "");
    select.setAttribute("aria-label", "Language");
    SUPPORTED.forEach(function (lang) {
      var o = document.createElement("option");
      o.value = lang;
      o.textContent = NAMES[lang];
      select.appendChild(o);
    });
    select.addEventListener("change", function () {
      setLang(select.value);
    });
    host.appendChild(select);
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
