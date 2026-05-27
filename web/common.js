// Shared client-side helpers used by index/waiting/player.html.
// Included via <script src="/static/common.js"></script>; the server
// sends an immutable Cache-Control so the file is hot-cached.

// escapeHTML escapes the five HTML entities that matter for attribute-
// safe and text-safe insertion. Coerces non-string inputs.
window.escapeHTML = function (s) {
  return String(s).replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;',
  })[c]);
};

// fetchWhoami returns {role, hostEnabled, name}. Cached in
// sessionStorage for 60s so page-to-page navigation (library →
// /watch → waiting room) doesn't refetch on every load.
window.fetchWhoami = async function () {
  const KEY = 'wp_whoami_v1';
  const TTL_MS = 60_000;
  try {
    const raw = sessionStorage.getItem(KEY);
    if (raw) {
      const cached = JSON.parse(raw);
      if (cached && cached.at && (Date.now() - cached.at) < TTL_MS && cached.v) {
        return cached.v;
      }
    }
  } catch (_) { /* sessionStorage may be disabled */ }
  try {
    const r = await fetch('/api/whoami');
    if (!r.ok) return null;
    const d = await r.json();
    try {
      sessionStorage.setItem(KEY, JSON.stringify({ at: Date.now(), v: d }));
    } catch (_) { /* quota/disabled — fine, we just re-fetch next time */ }
    return d;
  } catch (_) {
    return null;
  }
};

// openLiveEvents opens an EventSource('/events') and wires the
// pagehide cleanup that prevents bfcache-held connections from
// counting as ghost viewers. The caller provides onState(state); a
// JSON parse failure is logged and skipped.
window.openLiveEvents = function (onState) {
  const es = new EventSource('/events');
  es.onmessage = (e) => {
    try { onState(JSON.parse(e.data)); }
    catch (err) { console.warn('events: bad payload', err); }
  };
  window.addEventListener('pagehide', () => { try { es.close(); } catch (_) {} });
  return es;
};
