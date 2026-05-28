// When the user navigates back from /watch via the browser's back
// button, Chrome / Safari restore this page from the bfcache with the
// previously-clicked movie button still stuck in its disabled
// "cueing the projector…" state. Forcing a reload on a persisted
// pageshow rebuilds the DOM from a clean fetch so buttons reset.
window.addEventListener('pageshow', (e) => {
  if (e.persisted) location.reload();
});

const statusEl = document.getElementById('status');
const groupsEl = document.getElementById('groups');
const searchEl = document.getElementById('search');
const countEl  = document.getElementById('count');

let allMovies = [];

// "The Matrix" → "Matrix" so it groups under M, not T.
function sortKey(title) {
  return title.replace(/^(the|a|an)\s+/i, '').toLocaleLowerCase();
}
function bucketFor(title) {
  const c = sortKey(title).charAt(0).toUpperCase();
  return /[A-Z]/.test(c) ? c : '#';
}

// escapeHTML provided by /static/common.js

let isHost = true; // optimistic until /api/whoami answers

// --- Resume-after-restart banner --------------------------------------------
// Asks the server "is there a persisted resume hint?" — populated after a
// container restart or idle shutdown when state.ratingKey is empty but a
// prior session left a state.json. Host buttons trigger a /control load at
// the saved offset (Resume) or 0 (Start over); the Dismiss button hides
// the banner client-side for this tab only (the server hint persists).
const resumeBanner    = document.getElementById('resume-banner');
const resumeBannerTitle    = document.getElementById('rb-title');
const resumeBannerPosition = document.getElementById('rb-position');
const resumeBannerSaved    = document.getElementById('rb-saved');
const resumeBannerResume   = document.getElementById('rb-resume');
const resumeBannerRestart  = document.getElementById('rb-restart');
const resumeBannerDismiss  = document.getElementById('rb-dismiss');
let resumeHint = null;

function fmtHMS(s) {
  if (!isFinite(s) || s < 0) return '0:00';
  s = Math.floor(s);
  const h = Math.floor(s/3600);
  const m = Math.floor((s%3600)/60);
  const ss = s%60;
  const p = (n) => String(n).padStart(2, '0');
  return h > 0 ? h + ':' + p(m) + ':' + p(ss) : m + ':' + p(ss);
}
function fmtSavedAgo(savedAtUnix) {
  if (!savedAtUnix) return '—';
  const sec = Math.max(0, Math.floor(Date.now() / 1000) - savedAtUnix);
  if (sec < 60)    return sec + 's ago';
  if (sec < 3600)  return Math.floor(sec / 60) + ' min ago';
  if (sec < 86400) return Math.floor(sec / 3600) + ' h ago';
  return Math.floor(sec / 86400) + ' d ago';
}
function showResumeBanner(hint) {
  resumeHint = hint;
  resumeBannerTitle.textContent    = hint.title || hint.ratingKey;
  resumeBannerPosition.textContent = 'paused at ' + fmtHMS(hint.positionSec);
  if (hint.durationSec > 0) {
    resumeBannerPosition.textContent += ' of ' + fmtHMS(hint.durationSec);
  }
  resumeBannerSaved.textContent = fmtSavedAgo(hint.savedAtUnix);
  resumeBanner.classList.remove('hidden');
}
function hideResumeBanner() {
  resumeBanner.classList.add('hidden');
  resumeHint = null;
}
async function doResumeAction(restart) {
  if (!resumeHint || !isHost) return;
  const btn = restart ? resumeBannerRestart : resumeBannerResume;
  btn.disabled = true;
  const orig = btn.textContent;
  btn.textContent = 'cueing…';
  try {
    const body = { action: 'load', ratingKey: resumeHint.ratingKey };
    if (!restart) body.positionSec = resumeHint.positionSec;
    const r = await fetch('/control', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error(await r.text());
    location.href = '/watch';
  } catch (e) {
    btn.disabled = false;
    btn.textContent = orig + ' · retry';
    console.error('resume:', e);
  }
}
resumeBannerResume.addEventListener('click',  () => doResumeAction(false));
resumeBannerRestart.addEventListener('click', () => doResumeAction(true));
resumeBannerDismiss.addEventListener('click', hideResumeBanner);

// Probe initial state for a resume hint at page load. Doesn't need to
// wait for SSE — the snapshot endpoint reflects the same data.
fetch('/api/state').then(r => r.ok ? r.json() : null).then(st => {
  if (st && !st.ratingKey && st.resume) showResumeBanner(st.resume);
}).catch(() => {});

// Time formatter used in the resume modal subtitle. Hours-aware but
// drops the leading 0:00:0X for sub-hour positions to match the
// scrub-bar formatting on /watch.
function fmtTime(sec) {
  sec = Math.max(0, Math.floor(sec));
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  const pad = n => String(n).padStart(2, '0');
  return h > 0 ? h + ':' + pad(m) + ':' + pad(s) : m + ':' + pad(s);
}

// Send a load command and navigate to /watch on success. Shared by
// both the direct-click path and the "Start over" branch of the
// resume modal. `b` is the originating button so we can flash a retry
// state if /control errors out.
async function sendLoad(b, m, opts) {
  b.disabled = true;
  b.querySelector('.title').textContent = 'cueing the projector…';
  try {
    const body = { action: 'load', ratingKey: m.ratingKey };
    if (opts && opts.restart) body.restart = true;
    const r = await fetch('/control', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error(await r.text());
    location.href = '/watch';
  } catch (e) {
    b.disabled = false;
    b.querySelector('.title').textContent = m.title + ' · retry';
    console.error(e);
  }
}

// Resume / Start over modal wiring. Reused across all film buttons —
// we just stash the originating button + movie on the modal element
// and read them back when one of the actions fires.
const resumeOverlay   = document.getElementById('resume-overlay');
const resumeSubtitle  = document.getElementById('resume-subtitle');
const resumeGoBtn     = document.getElementById('resume-go');
const resumeRestartBtn = document.getElementById('resume-restart');
const resumeCancelBtn = document.getElementById('resume-cancel');
let resumeCtx = null; // { button, movie, positionSec }

function openResumeModal(ctx) {
  resumeCtx = ctx;
  resumeSubtitle.innerHTML =
    '<span class="movie-title">' + escapeHTML(ctx.movie.title) + '</span>' +
    ' paused at <span class="at-time">' + fmtTime(ctx.positionSec) + '</span>';
  resumeOverlay.classList.remove('hidden');
  // Defer focus so the click that opened the modal doesn't immediately
  // trigger Resume via the synthetic activation on the focused button.
  setTimeout(() => resumeGoBtn.focus(), 0);
}
function closeResumeModal() {
  resumeOverlay.classList.add('hidden');
  resumeCtx = null;
}
resumeGoBtn.onclick = () => {
  if (!resumeCtx) return;
  // No /control needed — the session is already alive. Just hop to
  // /watch and the player picks up the host's position over SSE.
  location.href = '/watch';
};
resumeRestartBtn.onclick = () => {
  if (!resumeCtx) return;
  const { button, movie } = resumeCtx;
  closeResumeModal();
  sendLoad(button, movie, { restart: true });
};
resumeCancelBtn.onclick = () => {
  if (resumeCtx && resumeCtx.button) {
    resumeCtx.button.disabled = false;
    resumeCtx.button.querySelector('.title').textContent = resumeCtx.movie.title;
  }
  closeResumeModal();
};
window.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !resumeOverlay.classList.contains('hidden')) {
    resumeCancelBtn.click();
  }
});

function makeButton(m) {
  const b = document.createElement('button');
  b.type = 'button';
  b.innerHTML =
    '<span class="title">' + escapeHTML(m.title) + '</span>' +
    (m.year ? '<span class="year">' + m.year + '</span>' : '');
  b.onclick = async () => {
    if (!isHost) return; // viewer: no-op
    // Probe current state first so we can offer Resume/Start-over
    // when the user clicks a movie that's already loaded. Probe
    // failure is non-fatal — fall through to a normal load.
    try {
      const r = await fetch('/api/state', { cache: 'no-store' });
      if (r.ok) {
        const st = await r.json();
        if (st.ratingKey === m.ratingKey && st.positionSec > 1) {
          openResumeModal({ button: b, movie: m, positionSec: st.positionSec });
          return;
        }
      }
    } catch (_) { /* ignore — proceed with normal load */ }
    sendLoad(b, m);
  };
  return b;
}

function render(movies) {
  groupsEl.innerHTML = '';
  if (movies.length === 0) {
    groupsEl.innerHTML = '<div class="empty">Nothing matches that search.</div>';
    return;
  }

  const byLetter = new Map();
  for (const m of movies) {
    const k = bucketFor(m.title);
    if (!byLetter.has(k)) byLetter.set(k, []);
    byLetter.get(k).push(m);
  }

  const letters = [...byLetter.keys()].sort((a, b) => {
    if (a === '#') return 1;
    if (b === '#') return -1;
    return a.localeCompare(b);
  });

  letters.forEach((letter, idx) => {
    const films = byLetter.get(letter);
    const section = document.createElement('section');
    section.className = 'letter-group';
    section.style.animationDelay = Math.min(idx * 40, 600) + 'ms';

    const mark = document.createElement('div');
    mark.className = 'letter-mark';
    mark.innerHTML =
      '<span class="glyph">' + letter + '</span>' +
      '<span class="meta">' + films.length + ' title' + (films.length === 1 ? '' : 's') + '</span>';
    section.appendChild(mark);

    const ul = document.createElement('ul');
    ul.className = 'films';
    for (const m of films) {
      const li = document.createElement('li');
      li.appendChild(makeButton(m));
      ul.appendChild(li);
    }
    section.appendChild(ul);
    groupsEl.appendChild(section);
  });
}

function applyFilter() {
  const t0 = performance.now();
  const q = searchEl.value.trim().toLocaleLowerCase();
  const list = q
    ? allMovies.filter(m => m.title.toLocaleLowerCase().includes(q))
    : allMovies;
  render(list);
  countEl.textContent = list.length + ' / ' + allMovies.length + ' titles';
  console.log('search filter+render:', (performance.now() - t0).toFixed(1), 'ms',
              '·', list.length, 'of', allMovies.length);
}

async function load() {
  try {
    const r = await fetch('/api/movies');
    if (!r.ok) throw new Error(await r.text());
    allMovies = await r.json();
    allMovies.sort((a, b) => sortKey(a.title).localeCompare(sortKey(b.title)));
    statusEl.remove();
    countEl.textContent = allMovies.length + ' titles';
    render(allMovies);
  } catch (e) {
    statusEl.textContent = 'Couldn’t load the library. Try again in a moment.';
    console.error(e);
  }
}

function renderWho(role, name) {
  const el = document.getElementById('who');
  el.classList.toggle('host', role === 'host');
  el.innerHTML =
    '<span class="nm">' + escapeHTML(name || 'guest') + '</span>' +
    '<span class="dot"></span>' +
    '<span class="role">' + role + '</span>';
  el.hidden = false;
}

async function loadWhoami() {
  const d = await fetchWhoami();
  if (!d) { renderWho('viewer', 'guest'); return; }
  isHost = d.role === 'host';
  renderWho(d.role, d.name);
  if (!isHost) {
    document.body.classList.add('guest');
    document.getElementById('guest').classList.add('show');
  }
}

searchEl.addEventListener('input', applyFilter);
loadWhoami();
load();

// Live "who's here" chip. Opens the same SSE stream the player uses
// so the library shows the room's real-time roster — the user
// browsing the library counts as connected too, so the host can see
// "3 in the room" before picking a movie. Closed on pagehide so a
// bfcache stash doesn't leak an extra phantom viewer for ~1 minute.
const hereEl       = document.getElementById('here');
const hereLabelEl  = hereEl.querySelector('.here-label');
const hereRosterEl = hereEl.querySelector('.here-roster');
function renderHere(viewers) {
  if (!Array.isArray(viewers) || viewers.length === 0) {
    hereEl.hidden = true;
    return;
  }
  hereEl.hidden = false;
  hereLabelEl.textContent = viewers.length + ' in the room';
  hereRosterEl.innerHTML = viewers.map(v =>
    '<div class="row' + (v.host ? ' host' : '') + '">' +
      '<span class="nm">' + escapeHTML(v.name || 'guest') + '</span>' +
      '<span class="role">' + (v.host ? 'host' : 'viewer') + '</span>' +
    '</div>'
  ).join('');
}
openLiveEvents((s) => {
  if (s && s.viewers) renderHere(s.viewers);
});
