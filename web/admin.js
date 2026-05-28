// Admin panel client. Polls /admin/api/stats every 5s, re-renders the
// four panels, and wires action buttons to the corresponding POST
// endpoints. No SSE here — the stats panel doesn't need sub-second
// latency, and polling keeps the surface simple.

const POLL_MS = 5000;

const $ = (id) => document.getElementById(id);

function fmtBytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 || i === 0 ? 0 : v >= 10 ? 1 : 2) + ' ' + units[i];
}
function fmtSeconds(s) {
  if (!s || s < 0) return '—';
  s = Math.floor(s);
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s/60) + 'm ' + (s%60) + 's';
  if (s < 86400) return Math.floor(s/3600) + 'h ' + Math.floor((s%3600)/60) + 'm';
  return Math.floor(s/86400) + 'd ' + Math.floor((s%86400)/3600) + 'h';
}
function fmtTimecode(s) {
  if (!s || s < 0) return '0:00';
  s = Math.floor(s);
  const h = Math.floor(s/3600);
  const m = Math.floor((s%3600)/60);
  const ss = s%60;
  const p = (n) => String(n).padStart(2, '0');
  return h > 0 ? h + ':' + p(m) + ':' + p(ss) : m + ':' + p(ss);
}
function fmtPct(num, den) {
  if (!den) return '—';
  return ((num / den) * 100).toFixed(1) + '%';
}
function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;',
  })[c]);
}
function setStatus(text, kind) {
  const el = $('footer-status');
  if (!el) return;
  el.innerHTML = '— · <span class="' + (kind || '') + '">' + escapeHTML(text) + '</span>';
}

async function api(path, opts) {
  opts = opts || {};
  opts.credentials = 'same-origin';
  const r = await fetch(path, opts);
  if (r.status === 401) {
    // Cookie expired — bounce to sign-in.
    window.location.href = '/admin/login';
    throw new Error('unauthorized');
  }
  if (!r.ok) {
    const body = await r.text().catch(() => '');
    throw new Error(r.status + ' ' + (body || r.statusText));
  }
  if (r.status === 204) return null;
  return r.json();
}

async function refresh() {
  try {
    const s = await api('/admin/api/stats');
    renderSession(s.session);
    renderCache(s.cache);
    renderLibrary(s.library);
    renderViewers(s.viewers);
    setStatus('ok', 'ok');
  } catch (err) {
    setStatus('error: ' + err.message, 'err');
    console.error('refresh:', err);
  }
}

function renderSession(s) {
  $('sess-title').textContent = s.title || '— idle —';
  $('sess-key').textContent = s.ratingKey || '—';
  if (s.ratingKey) {
    $('sess-pos').textContent = fmtTimecode(s.positionSec) + ' / ' + fmtTimecode(s.durationSec);
  } else {
    $('sess-pos').textContent = '—';
  }
  $('sess-playing').textContent = s.ratingKey ? (s.playing ? 'yes' : 'paused') : '—';
  $('sess-token').textContent = s.sessionToken || '0';
  $('btn-session-restart').disabled = !s.ratingKey;
}

function renderCache(c) {
  $('cache-entries').textContent = c.entries.toLocaleString();
  $('cache-bytes').textContent = fmtBytes(c.totalBytes);
  $('cache-max').textContent = fmtBytes(c.maxBytes);
  $('cache-fill').textContent = fmtPct(c.totalBytes, c.maxBytes);
  const rows = $('cache-rows');
  if (!c.perMovie || c.perMovie.length === 0) {
    rows.innerHTML = '<tr class="empty-row"><td colspan="6">cache is empty</td></tr>';
    return;
  }
  rows.innerHTML = c.perMovie.map(m =>
    '<tr>' +
      '<td>' + escapeHTML(m.ratingKey) + '</td>' +
      '<td>' + m.entries.toLocaleString() + '</td>' +
      '<td>' + fmtBytes(m.bytes) + '</td>' +
      '<td>' + fmtSeconds(m.newestAgeSec) + '</td>' +
      '<td>' + fmtSeconds(m.oldestAgeSec) + '</td>' +
      '<td><button class="row-action" data-action="clear-movie" data-key="' + escapeHTML(m.ratingKey) + '">Clear</button></td>' +
    '</tr>'
  ).join('');
}

function renderLibrary(l) {
  $('lib-titles').textContent = l.titles.toLocaleString();
  $('lib-cached').textContent = l.cachedAt ? new Date(l.cachedAt).toLocaleString() : '—';
  $('lib-age').textContent = l.ageSec > 0 ? fmtSeconds(l.ageSec) + ' ago' : '—';
  const health = $('lib-health');
  if (l.healthy) {
    health.innerHTML = '<span style="color: var(--signal)">● healthy</span>';
  } else {
    health.innerHTML = '<span style="color: var(--warn)">● unreachable</span>';
  }
}

function renderViewers(viewers) {
  const rows = $('viewer-rows');
  if (!viewers || viewers.length === 0) {
    rows.innerHTML = '<tr class="empty-row"><td colspan="6">no connections</td></tr>';
    return;
  }
  rows.innerHTML = viewers.map(v =>
    '<tr>' +
      '<td class="' + (v.host ? 'role-host' : 'role-viewer') + '">' + (v.host ? '◆ host' : '○ viewer') + '</td>' +
      '<td>' + escapeHTML(v.name || 'guest') + '</td>' +
      '<td>' + escapeHTML(v.ip) + '</td>' +
      '<td>' + fmtSeconds(v.connectedSec) + '</td>' +
      '<td>' + escapeHTML(v.id) + '</td>' +
      '<td><button class="row-action" data-action="kick" data-id="' + escapeHTML(v.id) + '">Kick</button></td>' +
    '</tr>'
  ).join('');
}

async function doAction(label, fn) {
  setStatus(label + '…', '');
  try {
    await fn();
    setStatus(label + ' · done', 'ok');
    await refresh();
  } catch (err) {
    setStatus(label + ' · failed: ' + err.message, 'err');
    console.error(label, err);
  }
}

// --- whoami ---
api('/admin/api/whoami').then(d => {
  if (d && d.email) $('who-email').textContent = d.email;
});

// --- panel-level buttons ---
$('btn-session-restart').addEventListener('click', () => {
  if (!confirm('Restart Plex session at the current host position?\n\nViewers will see a brief reload as the new transcode spins up.')) return;
  doAction('session restart', () => api('/admin/api/session/restart', {method: 'POST'}));
});
$('btn-cache-clear').addEventListener('click', () => {
  if (!confirm('Wipe ALL cached segments? This frees disk space but every viewer will re-fetch from Plex on next watch.')) return;
  doAction('cache clear', () => api('/admin/api/cache/clear', {method: 'POST'}));
});
$('btn-cache-prune').addEventListener('click', () => {
  const days = parseInt($('prune-days').value, 10);
  if (!days || days <= 0) { alert('enter a positive number of days'); return; }
  if (!confirm('Remove cached segments older than ' + days + ' day' + (days === 1 ? '' : 's') + '?')) return;
  doAction('prune ' + days + 'd', () => api('/admin/api/cache/prune?days=' + days, {method: 'POST'}));
});
$('btn-library-refresh').addEventListener('click', () => {
  doAction('library refresh', () => api('/admin/api/library/refresh', {method: 'POST'}));
});

// Event delegation for row buttons (clear-movie / kick).
document.body.addEventListener('click', (e) => {
  const btn = e.target.closest('button.row-action');
  if (!btn) return;
  const action = btn.dataset.action;
  if (action === 'clear-movie') {
    const key = btn.dataset.key;
    if (!confirm('Clear all cached segments for ratingKey ' + key + '?')) return;
    doAction('clear movie ' + key, () => api('/admin/api/cache/clear?ratingKey=' + encodeURIComponent(key), {method: 'POST'}));
  } else if (action === 'kick') {
    const id = btn.dataset.id;
    if (!confirm('Kick this connection? The viewer\'s browser will auto-reconnect.')) return;
    doAction('kick ' + id, () => api('/admin/api/viewers/kick?id=' + encodeURIComponent(id), {method: 'POST'}));
  }
});

refresh();
setInterval(refresh, POLL_MS);
