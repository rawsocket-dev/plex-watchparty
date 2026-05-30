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
// escapeHTML is provided by /admin/static/common.js (loaded before
// this script). Keep the local reference short for readability.
const escapeHTML = window.escapeHTML;
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
    // Cookie expired — bounce to the shared Google sign-in.
    window.location.href = '/login';
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
    const [s, bw, audit] = await Promise.all([
      api('/admin/api/stats'),
      api('/admin/api/bandwidth/history'),
      api('/admin/api/audit'),
    ]);
    if (s.version) $('build-ver').textContent = s.version;
    renderSession(s.session, s.lifecycle);
    renderBandwidth(bw.samples);
    renderCache(s.cache);
    renderLibrary(s.library);
    renderViewers(s.viewers);
    renderAudit(audit);
    setStatus('ok', 'ok');
  } catch (err) {
    setStatus('error: ' + err.message, 'err');
    console.error('refresh:', err);
  }
}

function fmtKbps(k) {
  if (!k || k <= 0) return '0 kbps';
  if (k >= 1000) return (k/1000).toFixed(1) + ' Mbps';
  return k + ' kbps';
}

function renderBandwidth(samples) {
  if (!Array.isArray(samples) || samples.length === 0) return;
  const now    = samples[samples.length - 1].kbps;
  const peak   = samples.reduce((m, s) => s.kbps > m ? s.kbps : m, 0);
  const sum    = samples.reduce((a, s) => a + s.kbps, 0);
  const avg    = Math.round(sum / samples.length);
  $('bw-now').textContent  = fmtKbps(now);
  $('bw-peak').textContent = fmtKbps(peak);
  $('bw-avg').textContent  = fmtKbps(avg);

  // Build the polyline + fill polygon. SVG viewBox is 600×80; we
  // map sample index → x (0..600) and kbps → y (80..0, inverted).
  // Y axis scales to peak (or a 1-Mbps floor so a quiet room
  // doesn't render a noise floor that fills the chart).
  const svg = $('bw-spark');
  const line = svg.querySelector('.bw-line');
  const fill = svg.querySelector('.bw-fill');
  const W = 600, H = 80;
  const yMax = Math.max(peak, 1000); // 1 Mbps floor
  const pts = samples.map((s, i) => {
    const x = (i / (samples.length - 1)) * W;
    const y = H - (s.kbps / yMax) * (H - 2) - 1; // 1px padding top/bottom
    return x.toFixed(1) + ',' + y.toFixed(1);
  });
  line.setAttribute('points', pts.join(' '));
  // Polygon = polyline + bottom-right + bottom-left to close the fill.
  fill.setAttribute('points', pts.join(' ') + ' ' + W + ',' + H + ' 0,' + H);
}

function renderSession(s, lifecycle) {
  $('sess-title').textContent = s.title || '— idle —';
  $('sess-key').textContent = s.ratingKey || '—';
  if (s.ratingKey) {
    $('sess-pos').textContent = fmtTimecode(s.positionSec) + ' / ' + fmtTimecode(s.durationSec);
  } else {
    $('sess-pos').textContent = '—';
  }
  $('sess-playing').textContent = s.ratingKey ? (s.playing ? 'yes' : 'paused') : '—';
  $('sess-token').textContent = s.sessionToken || '0';
  if (lifecycle) {
    $('sess-age').textContent = lifecycle.sessionAgeSec >= 0
      ? fmtSeconds(lifecycle.sessionAgeSec) : '—';
    const restartTotal = (lifecycle.restartsBySeek || 0)
      + (lifecycle.restartsByAuto || 0)
      + (lifecycle.restartsByRecover || 0)
      + (lifecycle.restartsByAdmin || 0);
    $('sess-restarts').textContent = restartTotal + ' '
      + '(seek ' + (lifecycle.restartsBySeek || 0)
      + ' · auto ' + (lifecycle.restartsByAuto || 0)
      + ' · recover ' + (lifecycle.restartsByRecover || 0)
      + ' · admin ' + (lifecycle.restartsByAdmin || 0) + ')';
  }
  $('btn-session-restart').disabled = !s.ratingKey;
  $('btn-session-stop').disabled = !s.ratingKey;
}

function renderCache(c) {
  $('cache-entries').textContent = c.entries.toLocaleString();
  $('cache-bytes').textContent = fmtBytes(c.totalBytes);
  $('cache-max').textContent = fmtBytes(c.maxBytes);
  $('cache-fill').textContent = fmtPct(c.totalBytes, c.maxBytes);
  const total = (c.hits || 0) + (c.misses || 0);
  $('cache-hitrate').textContent = total > 0
    ? fmtPct(c.hits, total) + ' (' + c.hits.toLocaleString() + ' / ' + total.toLocaleString() + ')'
    : '— (no requests yet)';
  if (c.diskTotalBytes > 0) {
    $('cache-disk').textContent = fmtBytes(c.freeBytes) + ' free of ' + fmtBytes(c.diskTotalBytes);
  } else {
    $('cache-disk').textContent = '—';
  }
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
    rows.innerHTML = '<tr class="empty-row"><td colspan="9">no connections</td></tr>';
    return;
  }
  rows.innerHTML = viewers.map(v => {
    // Position cell: heartbeat timestamp + at-position. If we've
    // never heard from this client (e.g. it just connected and the
    // first 5s heartbeat is still pending), render an em-dash.
    let pos = '—';
    if (v.heartbeatAgeSec >= 0) {
      const state = v.paused ? '⏸' : '▶';
      pos = state + ' ' + fmtTimecode(v.posSec || 0);
      if (v.heartbeatAgeSec > 10) {
        // Stale heartbeat — show how stale so an operator can tell
        // "viewer's been backgrounded" from "still actively playing."
        pos += ' (' + fmtSeconds(v.heartbeatAgeSec) + ' ago)';
      }
    }
    return '<tr>' +
      '<td class="' + (v.isActiveHost ? 'role-host' : 'role-viewer') + '">' + (v.isActiveHost ? '◆ host' : '○ viewer') + '</td>' +
      '<td>' + escapeHTML(v.name || 'guest') + (v.conns > 1 ? ' <span class="conns">×' + v.conns + '</span>' : '') + '</td>' +
      '<td>' + escapeHTML(v.ip) + '</td>' +
      '<td>' + fmtKbps(v.kbps || 0) + '</td>' +
      '<td>' + pos + '</td>' +
      '<td>' + fmtSeconds(v.connectedSec) + '</td>' +
      '<td>' + escapeHTML(v.id) + '</td>' +
      '<td>' + (v.isActiveHost
        ? '<span class="host-badge">▶ host</span>'
        : '<button class="row-action" data-action="make-host" data-id="' + escapeHTML(v.id) + '">Make host</button>') + '</td>' +
      '<td>' +
        '<button class="row-action" data-action="kick" data-id="' + escapeHTML(v.id) + '">Kick</button>' +
        (v.email ? ' <button class="row-action" data-action="set-alias" data-email="' + escapeHTML(v.email) + '">Alias</button>' : '') +
      '</td>' +
    '</tr>';
  }).join('');
}

function renderAliases(list) {
  const tbody = $('alias-rows');
  if (!tbody) return;
  if (!list || list.length === 0) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="3">No aliases set.</td></tr>';
    return;
  }
  tbody.innerHTML = list.map((a) =>
    '<tr>' +
    '<td class="mono">' + escapeHTML(a.email) + '</td>' +
    '<td>' + escapeHTML(a.alias) + '</td>' +
    '<td><button class="row-action" data-action="remove-alias" data-email="' + escapeHTML(a.email) + '">Remove</button></td>' +
    '</tr>'
  ).join('');
}

async function loadAliases() {
  try {
    const d = await api('/admin/api/aliases');
    renderAliases(d.aliases || []);
  } catch (err) {
    console.error('loadAliases:', err);
  }
}

function fmtWhen(unix) {
  if (!unix) return '—';
  return new Date(unix * 1000).toLocaleString();
}
function renderAudit(events) {
  const tbody = $('audit-rows');
  if (!tbody) return;
  if (!events || events.length === 0) {
    tbody.innerHTML = '<tr><td colspan="6">No events yet.</td></tr>';
    return;
  }
  tbody.innerHTML = events.map((e) =>
    '<tr>' +
    '<td class="mono">' + escapeHTML(fmtWhen(e.unix)) + '</td>' +
    '<td>' + escapeHTML(e.type || '') + '</td>' +
    '<td class="mono">' + escapeHTML(e.email || '') + '</td>' +
    '<td>' + escapeHTML(e.role || '') + '</td>' +
    '<td class="mono">' + escapeHTML(e.ip || '') + '</td>' +
    '<td>' + escapeHTML(e.detail || '') + '</td>' +
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
$('btn-session-stop').addEventListener('click', () => {
  if (!confirm('End the watch session and send everyone to the lobby?\n\nThe Plex transcoder will stop. Every connected /watch page will reload into the waiting room.')) return;
  doAction('send to lobby', () => api('/admin/api/session/stop', {method: 'POST'}));
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
$('alias-add').addEventListener('submit', (e) => {
  e.preventDefault();
  const email = $('alias-email').value.trim();
  const alias = $('alias-name').value.trim();
  if (!email || !alias) { alert('enter both an email and an alias'); return; }
  doAction('set alias ' + email, async () => {
    await api('/admin/api/aliases/set?email=' + encodeURIComponent(email) + '&alias=' + encodeURIComponent(alias), {method: 'POST'});
    $('alias-email').value = '';
    $('alias-name').value = '';
    await loadAliases();
  });
});

// Event delegation for row buttons (clear-movie / kick / make-host / remove-alias / set-alias).
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
  } else if (action === 'make-host') {
    api('/admin/api/host/set?id=' + encodeURIComponent(btn.dataset.id), { method: 'POST' })
      .then(() => refresh())
      .catch(err => setStatus('make-host: ' + err.message, 'err'));
    return;
  } else if (action === 'remove-alias') {
    const email = btn.dataset.email;
    if (!confirm('Remove the alias for ' + email + '?')) return;
    doAction('remove alias ' + email, async () => {
      await api('/admin/api/aliases/remove?email=' + encodeURIComponent(email), {method: 'POST'});
      await loadAliases();
    });
  } else if (action === 'set-alias') {
    // Prefill the Aliases form with this viewer's email, then focus the
    // alias input — the admin just types a name and submits.
    $('alias-email').value = btn.dataset.email;
    $('alias-name').focus();
    $('alias-name').scrollIntoView({behavior: 'smooth', block: 'center'});
    return;
  }
});

refresh();
loadAliases();
setInterval(refresh, POLL_MS);
