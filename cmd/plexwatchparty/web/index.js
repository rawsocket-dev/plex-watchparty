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

let isHost = false;       // true only when THIS user is the active host (can pick)
let lastActiveHostName;   // tracks election / hand-off changes seen over the SSE

// --- Resume-after-restart banner --------------------------------------------
// Asks the server "is there a persisted resume hint?" — populated after a
// container restart or idle shutdown when state.ratingKey is empty but a
// prior session left a state.json. Host buttons trigger a /control load at
// the saved offset (Resume) or 0 (Start over); the Dismiss button hides
// the banner and remembers (in localStorage) that this exact hint was
// dismissed, so it stays hidden across reloads — permanently for that movie
// (the key is the ratingKey, so a fresh save of the same movie won't bring it
// back). A hint for a DIFFERENT movie is a different key and still shows.
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
// Dismissal identity, keyed by ratingKey ONLY (not savedAtUnix): once the
// user dismisses the resume prompt for a movie it must never reappear for
// that same movie, even if the server later re-saves its position with a
// fresher timestamp. A DIFFERENT movie is a different key, so its hint still
// shows.
function resumeHintKey(hint) {
  return 'rb-dismissed:' + (hint.ratingKey || '');
}
function resumeHintDismissed(hint) {
  try { return localStorage.getItem(resumeHintKey(hint)) === '1'; }
  catch (e) { return false; }
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
  if (resumeHint) {
    try {
      // Persist the dismissal per-movie (key is the ratingKey) so it sticks
      // across reloads AND future saves of the same title — dismissing a
      // movie hides its resume prompt for good. Keys are bounded by the
      // number of distinct movies ever dismissed, so no pruning is needed.
      localStorage.setItem(resumeHintKey(resumeHint), '1');
    } catch (e) {}
  }
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
    const body = { action: 'load', ratingKey: resumeHint.ratingKey, autoplay: true };
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
  if (st && !st.ratingKey && st.resume && !resumeHintDismissed(st.resume)) {
    showResumeBanner(st.resume);
  }
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

// Compact Plex rating chip: gold critic star + audience marker, each
// shown only when Plex actually has that score (0/absent → omitted).
// Returns '' when neither exists, so most-obscure titles stay clean.
function ratingHTML(m) {
  const parts = [];
  if (m.rating > 0)         parts.push('<span class="rt-c">★ ' + m.rating.toFixed(1) + '</span>');
  if (m.audienceRating > 0) parts.push('<span class="rt-a">👥 ' + m.audienceRating.toFixed(1) + '</span>');
  return parts.length ? '<span class="rating">' + parts.join(' · ') + '</span>' : '';
}

// Stable per-title hue (0–360) so each poster gets its own duotone tint
// that doesn't shuffle between renders. Hashes the ratingKey (falling
// back to the title) — same inputs always land on the same colour.
function hueFor(seed) {
  const s = String(seed || '');
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return h % 360;
}

function makeButton(m) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'film';
  if (m.ratingKey) b.dataset.key = m.ratingKey;
  // Velvet poster card: a 2/3 artwork tile, then the title and a .meta row
  // (rating + year) BELOW the card so nothing overlaps the artwork. The
  // .title/.meta/.rating/.year classes and the click handler are unchanged.
  const hue = hueFor(m.ratingKey || m.title);
  const grad =
    'linear-gradient(155deg, oklch(0.44 0.15 ' + hue + '), oklch(0.2 0.1 ' + hue + '))';
  // Real Plex art (served unauthenticated at /poster/<key>.jpg) is LAZY-loaded:
  // render only the per-title gradient now and stash the URL in data-poster;
  // observePosters() swaps the image in when the card nears the viewport. With
  // thousands of titles this avoids firing thousands of /poster requests just
  // for sitting on the library. The gradient stays as the placeholder and as
  // the fallback when a title has no art (the layer simply never paints).
  const posterAttr = m.ratingKey
    ? ' data-poster="/poster/' + encodeURIComponent(m.ratingKey) + '.jpg"'
    : '';
  // Rating (.rt-c critic / .rt-a audience, each only when Plex has it) and
  // the year share a .meta row under the card.
  const rating = ratingHTML(m);
  const year = m.year ? '<span class="year">' + m.year + '</span>' : '';
  const meta = (rating || year) ? '<span class="meta">' + rating + year + '</span>' : '';
  b.innerHTML =
    '<span class="poster"' + posterAttr + ' style="background-image:' + grad + '"></span>' +
    '<span class="title">' + escapeHTML(m.title) + '</span>' +
    meta;
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

// --- Lazy poster loading ----------------------------------------------------
// A library of thousands of titles must not fire a /poster request per card on
// load. Each card renders with just its gradient + a data-poster URL; this
// observer swaps the real art in only when the card nears the viewport, then
// stops watching it. The gradient stays beneath as placeholder + fallback.
function loadPosterNow(el) {
  const url = el.dataset.poster;
  if (!url) return;
  el.style.backgroundImage = "url('" + url + "'), " + el.style.backgroundImage;
  delete el.dataset.poster;
  if (posterObserver) posterObserver.unobserve(el);
}
const posterObserver = ('IntersectionObserver' in window)
  ? new IntersectionObserver((entries) => {
      for (const e of entries) if (e.isIntersecting) loadPosterNow(e.target);
    }, { rootMargin: '200% 0px' }) // at rest keep ~2 screens of posters loaded
  : null;
function observePosters() {
  const lazy = groupsEl.querySelectorAll('.poster[data-poster]');
  if (!posterObserver) { // no IntersectionObserver support → just load them all
    lazy.forEach(loadPosterNow);
    return;
  }
  posterObserver.disconnect(); // drop targets from a previous render
  lazy.forEach(el => posterObserver.observe(el));
}

// Velocity-aware preloading. The observer's 2-screen window is plenty at rest,
// but a fast scroll/fling can outrun the network and reach cards whose art
// hasn't downloaded yet. While scrolling quickly we fire poster requests
// further ahead — scaled by scroll speed, so the faster you go the more lead
// time the network gets — capped so a fling-to-the-bottom doesn't request the
// whole library at once.
function preloadAhead(lookaheadPx) {
  const vh = window.innerHeight;
  const limit = vh + lookaheadPx;
  // querySelectorAll returns only the still-unloaded posters, in document
  // (vertical) order, so we can stop at the first one past the window.
  for (const el of groupsEl.querySelectorAll('.poster[data-poster]')) {
    const top = el.getBoundingClientRect().top;
    if (top > limit) break;           // everything after is further down
    if (top > -vh) loadPosterNow(el); // within reach (skip ones already scrolled well past)
  }
}
let lastScrollY = window.scrollY;
let lastScrollT = performance.now();
let scrollScheduled = false;
window.addEventListener('scroll', () => {
  if (scrollScheduled) return;     // coalesce to one measurement per frame
  scrollScheduled = true;
  requestAnimationFrame(() => {
    scrollScheduled = false;
    const y = window.scrollY, t = performance.now();
    const v = Math.abs(y - lastScrollY) / Math.max(1, t - lastScrollT); // px per ms
    lastScrollY = y; lastScrollT = t;
    // Below a gentle-scroll threshold the observer's base window already
    // covers it; above it, look ahead to roughly where they'll be in ~0.7s.
    if (v > 0.4) preloadAhead(Math.min(v * 700, window.innerHeight * 16));
  });
}, { passive: true });

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

  // Watch the freshly-rendered cards; only on-screen posters fetch their art.
  observePosters();
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

// applyWhoami sets host UI from whoami. Picking is gated on being the
// single ACTIVE host (isActiveHost), NOT on host-eligibility — otherwise
// every eligible friend would see the picker and read as a host. The guest
// card shows for everyone who isn't currently driving.
function applyWhoami(d) {
  isHost = !!(d && d.isActiveHost);
  renderWho(isHost ? 'host' : 'viewer', d && d.name);
  document.body.classList.toggle('guest', !isHost);
  document.getElementById('guest').classList.toggle('show', !isHost);
}

async function loadWhoami() {
  applyWhoami(await fetchWhoami());
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
  if (!s) return;
  if (s.viewers) renderHere(s.viewers);
  // Re-fetch whoami (cache-bypassing) whenever the active host changes, so
  // the picker / guest card track election + hand-off without a reload.
  const name = s.activeHostName || '';
  if (name !== lastActiveHostName) {
    lastActiveHostName = name;
    fetch('/api/whoami', { cache: 'no-store' }).then(r => r.ok ? r.json() : null).then(applyWhoami).catch(() => {});
  }
});
