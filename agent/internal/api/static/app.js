// The agent's page works without this file: every view is rendered by the
// agent and every change is a form. The script keeps the parts marked
// data-live current, by fetching the same view again and swapping them in,
// and sends the language menu when it changes. It talks to the agent's own
// origin only; the content policy allows nothing else.
(function () {
  'use strict';
  document.documentElement.classList.add('js');

  document.querySelectorAll('select[data-autosubmit]').forEach(function (el) {
    el.addEventListener('change', function () { el.form.submit(); });
  });

  var live = document.querySelectorAll('[data-live][id]');
  if (!live.length || !window.fetch || !window.DOMParser) { return; }
  var gone = document.getElementById('gone');
  var busy = false;

  function refresh() {
    if (busy || document.hidden) { return; }
    busy = true;
    fetch(location.pathname, { headers: { 'X-RedundaNet-Live': '1' }, credentials: 'same-origin', cache: 'no-store' })
      .then(function (r) {
        if (r.status === 401) { if (gone) { gone.hidden = false; } return null; }
        return r.ok ? r.text() : null;
      })
      .then(function (html) {
        if (!html) { return; }
        if (gone) { gone.hidden = true; }
        var doc = new DOMParser().parseFromString(html, 'text/html');
        document.querySelectorAll('[data-live][id]').forEach(function (el) {
          var fresh = doc.getElementById(el.id);
          // A part the reader is in (a focused button, a selection in the
          // log) is left alone until they leave it.
          if (!fresh || el.contains(document.activeElement) || fresh.innerHTML === el.innerHTML) { return; }
          var sel = window.getSelection && window.getSelection();
          if (sel && !sel.isCollapsed && el.contains(sel.anchorNode)) { return; }
          el.className = fresh.className;
          el.innerHTML = fresh.innerHTML;
        });
      })
      .catch(function () { /* the agent is restarting; the next round tells */ })
      .then(function () { busy = false; });
  }
  setInterval(refresh, 4000);
  document.addEventListener('visibilitychange', refresh);
})();
