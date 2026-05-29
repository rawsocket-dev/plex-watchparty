const v = document.getElementById('v');
const titleEl = document.getElementById('title');
const syncEl  = document.getElementById('sync');
const joinEl  = document.getElementById('join');
const bwEl    = document.getElementById('bw');
// #wrap is the fullscreen target and carries the .playing / .fill / .fs
// state classes the top-bar status lights key off. Declared up here with
// the other element refs so applyState (and any early caller) can touch
// it without tripping a temporal-dead-zone error.
const wrapEl  = document.getElementById('wrap');

// isHost tracks whether this connection IS the active host — drives
// control visibility, scrub-bar seek guards, and join-overlay autoplay.
// Viewers can't drive playback; their interactions silently no-op and
// their buttons are hidden via the .viewer body class.
let isHost = false;            // "is the ACTIVE host" — drives control visibility
let lastActiveHostName;        // undefined initially
function applyWhoami(d) {
  if (!d) return;
  isHost = !!d.isActiveHost;
  myName = d.name || '';
  document.body.classList.toggle('viewer', !isHost);
  const al = document.getElementById('admin-link');
  if (al) al.hidden = !d.isAdmin;
}
fetchWhoami().then(applyWhoami);
// Hand-off picker: the "Driving" chip (#host-cell) is the trigger. For the
// active host it opens a popover of connected users; clicking one POSTs to
// /api/host/handoff. Viewers see the chip read-only (no chevron, no open).
let myName = '';
const hostCell    = document.getElementById('host-cell');
const handoffPop  = document.getElementById('handoff-pop');
const handoffList = document.getElementById('handoff-list');
function openHandoff()  { if (handoffPop) handoffPop.hidden = false; if (hostCell) hostCell.classList.add('open'); }
function closeHandoff() { if (handoffPop) handoffPop.hidden = true;  if (hostCell) hostCell.classList.remove('open'); }
if (hostCell) {
  hostCell.addEventListener('click', (e) => {
    if (!isHost) return;
    if (e.target.closest('#handoff-pop')) return; // row clicks handled below
    if (handoffPop && handoffPop.hidden) openHandoff(); else closeHandoff();
  });
}
if (handoffList) {
  handoffList.addEventListener('click', (e) => {
    const btn = e.target.closest('.pop-u');
    if (!btn) return;
    const id = btn.getAttribute('data-id');
    if (id) fetch('/api/host/handoff?id=' + encodeURIComponent(id), { method: 'POST' }).catch(() => {});
    closeHandoff();
  });
}
// Outside-click closes the popover.
document.addEventListener('click', (e) => {
  if (!handoffPop || handoffPop.hidden) return;
  if (e.target.closest('#host-cell')) return;
  closeHandoff();
});
// Re-fetch whoami (cache-bypassing) whenever the active host changes, so
// control visibility tracks election/hand-off without a reload.
function refreshHostUI(state) {
  const name = state.activeHostName || '';
  if (name !== lastActiveHostName) {
    lastActiveHostName = name;
    fetch('/api/whoami', { cache: 'no-store' }).then(r => r.ok ? r.json() : null).then(applyWhoami).catch(() => {});
  }
  const hc = document.getElementById('host-cell');
  const hn = document.getElementById('host-name');
  const chev = hc && hc.querySelector('.chev');
  // The driving chip is always shown now. The active host sees their own
  // name + a chevron and can click to hand off; viewers see who's driving,
  // read-only.
  if (hc) hc.hidden = false;
  if (hn) hn.textContent = isHost ? ((myName ? myName + ' ' : '') + '(you)') : (name || 'nobody');
  if (hc) hc.classList.toggle('click', isHost);
  if (chev) chev.hidden = !isHost;
  if (!isHost) closeHandoff();
}
const scrubHit      = document.getElementById('scrub-hit');
const scrubTrack    = document.getElementById('scrub-track');
const scrubFill     = document.getElementById('scrub-fill');
const scrubBuffered = document.getElementById('scrub-buffered');
const scrubSeekable = document.getElementById('scrub-seekable');
const scrubThumb    = document.getElementById('scrub-thumb');
const scrubTooltip  = document.getElementById('scrub-tooltip');
const scrubTime     = document.getElementById('scrub-time');

// --- scrub bar --------------------------------------------------------------
function fmtTime(s) {
  if (!isFinite(s) || s < 0) return '0:00';
  s = Math.floor(s);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  const mm = String(m).padStart(h > 0 ? 2 : 1, '0');
  const ss = String(sec).padStart(2, '0');
  return h > 0 ? h + ':' + mm + ':' + ss : mm + ':' + ss;
}
// Server pushes the movie duration via SSE state (sourced from Plex
// metadata at load time — authoritative). We prefer it over v.duration
// because with our incrementally-loaded VOD playlist v.duration only
// reflects the fragments hls.js has seen so far, NOT the actual movie
// length. Using v.duration would shrink the scrub bar to "buffered so
// far" instead of "the whole movie".
let serverDuration = 0;
function knownDuration() {
  if (serverDuration > 0) return serverDuration;
  if (isFinite(v.duration) && v.duration > 0) return v.duration;
  return 0;
}
let scrubDragging = false;
let scrubPendingPct = null;
function pctFromEvent(e) {
  const rect = scrubHit.getBoundingClientRect();
  const cx = (e.touches ? e.touches[0].clientX : e.clientX);
  const x = cx - rect.left;
  return Math.max(0, Math.min(1, x / rect.width));
}
function rawPctFromEvent(e) {
  // Unclamped, for diagnostic logging only.
  const rect = scrubHit.getBoundingClientRect();
  const cx = (e.touches ? e.touches[0].clientX : e.clientX);
  return ((cx - rect.left) / rect.width);
}
function paintAt(pct) {
  scrubFill.style.width  = (pct * 100) + '%';
  scrubThumb.style.left  = (pct * 100) + '%';
  const dur = knownDuration();
  if (dur > 0) {
    scrubTooltip.textContent = fmtTime(pct * dur);
    scrubTooltip.style.left  = (pct * 100) + '%';
  }
}
function updateScrub() {
  const dur = knownDuration();
  const cur = v.currentTime || 0;
  if (dur > 0) {
    scrubHit.classList.add('ready');
    if (!scrubDragging) paintAt(cur / dur);
    if (v.buffered.length > 0) {
      const buf = v.buffered.end(v.buffered.length - 1);
      scrubBuffered.style.width = Math.min(100, (buf / dur) * 100) + '%';
    }
    // Cached ranges drive the seekable visualization. v.seekable would
    // only show the current Plex session's range; we want the union of
    // everything we have on disk so users can see all instant-seek spots.
    if (lastState && lastState.cachedRanges) {
      // Build / update child bands. Reuse existing children to avoid
      // creating + destroying DOM every tick.
      const ranges = lastState.cachedRanges;
      while (scrubSeekable.children.length < ranges.length) {
        const band = document.createElement('div');
        band.className = 'cached-band';
        scrubSeekable.appendChild(band);
      }
      while (scrubSeekable.children.length > ranges.length) {
        scrubSeekable.removeChild(scrubSeekable.lastChild);
      }
      for (let i = 0; i < ranges.length; i++) {
        const [s, e] = ranges[i];
        const band = scrubSeekable.children[i];
        band.style.left  = (s / dur * 100) + '%';
        band.style.width = ((e - s) / dur * 100) + '%';
      }
    } else {
      while (scrubSeekable.firstChild) scrubSeekable.removeChild(scrubSeekable.firstChild);
    }
    scrubTime.textContent = fmtTime(cur) + ' / ' + fmtTime(dur);
  } else {
    scrubHit.classList.remove('ready');
    scrubFill.style.width = '0%';
    scrubBuffered.style.width = '0%';
    while (scrubSeekable.firstChild) scrubSeekable.removeChild(scrubSeekable.firstChild);
    scrubTime.textContent = fmtTime(cur) + ' / —';
  }
}
v.addEventListener('timeupdate', updateScrub);
v.addEventListener('durationchange', updateScrub);
v.addEventListener('progress', updateScrub);

// Hover: keep the tooltip following the cursor with the target time.
scrubHit.addEventListener('mousemove', (e) => {
  if (knownDuration() <= 0) return;
  const pct = pctFromEvent(e);
  if (scrubDragging) { paintAt(pct); scrubPendingPct = pct; }
  else {
    scrubTooltip.textContent = fmtTime(pct * knownDuration());
    scrubTooltip.style.left  = (pct * 100) + '%';
  }
});
// Mouse drag — listeners live on window so the cursor can leave the
// track mid-drag and still get the seek when released.
scrubHit.addEventListener('mousedown', (e) => {
  if (!isHost) return;
  if (knownDuration() <= 0) return;
  e.preventDefault();
  scrubDragging = true;
  scrubHit.classList.add('dragging');
  const pct = pctFromEvent(e);
  const raw = rawPctFromEvent(e);
  console.log('scrub mousedown: pct=', pct.toFixed(3),
              ' raw=', raw.toFixed(3),
              ' clientX=', e.clientX,
              ' rect=', scrubHit.getBoundingClientRect().left.toFixed(0) + '..' +
                        (scrubHit.getBoundingClientRect().left + scrubHit.getBoundingClientRect().width).toFixed(0));
  paintAt(pct);
  scrubPendingPct = pct;
});
window.addEventListener('mousemove', (e) => {
  if (!scrubDragging || knownDuration() <= 0) return;
  const pct = pctFromEvent(e);
  paintAt(pct);
  scrubPendingPct = pct;
});
window.addEventListener('mouseup', () => {
  if (!scrubDragging) return;
  scrubDragging = false;
  scrubHit.classList.remove('dragging');
  commitScrubSeek();
});
// Touch
scrubHit.addEventListener('touchstart', (e) => {
  if (!isHost) return;
  if (knownDuration() <= 0) return;
  e.preventDefault();
  scrubDragging = true;
  scrubHit.classList.add('dragging');
  const pct = pctFromEvent(e);
  paintAt(pct);
  scrubPendingPct = pct;
}, { passive: false });
scrubHit.addEventListener('touchmove', (e) => {
  if (!scrubDragging || knownDuration() <= 0) return;
  e.preventDefault();
  const pct = pctFromEvent(e);
  paintAt(pct);
  scrubPendingPct = pct;
}, { passive: false });
scrubHit.addEventListener('touchend', () => {
  if (!scrubDragging) return;
  scrubDragging = false;
  scrubHit.classList.remove('dragging');
  commitScrubSeek();
});

// Wall-clock time (ms) of our last local play/pause/seek so we can
// ignore SSE state broadcasts that pre-date it. Without this, an SSE
// message that was in-flight when the user dragged the scrub bar can
// arrive a moment later carrying the OLD position and yank the player
// back. 500 ms tolerance for client↔server clock skew.
let lastLocalActionMs = 0;
function commitScrubSeek() {
  const dur = knownDuration();
  if (scrubPendingPct == null || dur <= 0) {
    console.log('scrub commit skipped: pct=', scrubPendingPct, ' dur=', dur);
    scrubPendingPct = null;
    return;
  }
  const target = scrubPendingPct * dur;
  scrubPendingPct = null;
  // Post the RAW target — do NOT clamp to v.seekable. v.seekable
  // reflects only the current Plex transcode session's range, not the
  // full movie length. Clamping here would silently downgrade any
  // forward seek past the transcoded edge into "seek to the edge" and
  // the server would never see a target that triggers a Restart. The
  // server (HandleControl) is the authority: target > EdgeSec() and
  // not in cache → Restart + bump SessionToken → client reattaches
  // with a new playlist starting at the requested position. Setting
  // v.currentTime to a value outside v.seekable lets the browser
  // silently clamp locally; the reattach moves us to the real target
  // once the new playlist arrives.
  lastLocalActionMs = Date.now();
  v.currentTime = target;
  post('seek', { positionSec: target });
}

// --- bandwidth poll ---------------------------------------------------------
async function pollBandwidth() {
  // Skip the round-trip when the tab is hidden — the UI it feeds
  // isn't visible, and a bfcache'd / backgrounded tab racking up
  // polls every 2 s wastes server work for nothing.
  if (document.visibilityState === 'hidden') return;
  try {
    const r = await fetch('/api/bandwidth');
    if (!r.ok) return;
    const d = await r.json();
    if (d.mineKbps === 0 && d.totalKbps === 0) {
      bwEl.hidden = true;
      return;
    }
    bwEl.hidden = false;
    // Fill just the .v inside the bandwidth cell — the .lbl above
    // already says "User BW / Total", and the unit is appended once at
    // the end so both numbers share a single unit label.
    const bwV = bwEl.querySelector('.v');
    bwV.innerHTML =
      '<span class="you">' + (d.mineKbps / 1000).toFixed(1) + '</span>' +
      '<span class="slash">/</span>' +
      '<span class="room">' + (d.totalKbps / 1000).toFixed(1) + '</span>' +
      '<span class="unit">Mbps</span>';
  } catch (_) { /* network blip — try again next tick */ }
}

// Render the viewer count + roster tooltip from the SSE state's
// .viewers field. Source of truth is the SSE connection list, not
// bandwidth tracking — names come from the wp_name cookie set at
// login, with "guest" as the fallback for anyone who skipped it.
const viewersEl = document.getElementById('viewers');
const viewersCountEl = viewersEl.querySelector('.count');
const viewersRosterEl = viewersEl.querySelector('.roster');
// escapeHTML provided by /static/common.js
function renderViewers(list) {
  if (!Array.isArray(list) || list.length === 0) {
    viewersEl.hidden = true;
    if (handoffList) handoffList.innerHTML = '';
    return;
  }
  viewersEl.hidden = false;
  viewersCountEl.textContent = list.length + ' viewer' + (list.length === 1 ? '' : 's');
  viewersRosterEl.innerHTML = list.map(v =>
    '<div class="row' + (v.host ? ' host' : '') + '">' +
      '<span class="nm">' + escapeHTML(v.name || 'guest') + '</span>' +
      '<span class="role">' + (v.host ? 'host' : 'viewer') + '</span>' +
    '</div>'
  ).join('');
  // Build the hand-off popover from connected users, omitting the host
  // (self) — only one user holds control at a time, marked host:true.
  if (handoffList) {
    const others = (list || []).filter(v => v.id && !v.host);
    handoffList.innerHTML = others.length
      ? others.map(v => {
          const nm = v.name || v.id;
          const initial = escapeHTML((nm.trim()[0] || '?').toUpperCase());
          return '<button class="pop-u" data-id="' + escapeHTML(v.id) + '">' +
            '<span class="av">' + initial + '</span>' +
            '<span class="nm">' + escapeHTML(nm) + '</span>' +
            '<span class="arr">→</span></button>';
        }).join('')
      : '<div class="pop-empty">No one else here yet.</div>';
  }
}
function fmtRate(kbps) {
  if (kbps >= 1000) return (kbps / 1000).toFixed(1) + ' Mbps';
  return kbps + ' kbps';
}
setInterval(pollBandwidth, 2000);
pollBandwidth();

// Browser autoplay policy: v.play() is rejected until the user has
// interacted with the page. We satisfy that with a visible overlay
// the viewer clicks once. After that, all subsequent /events-driven
// play/pause calls work normally.
let userJoined = false;
function dismissJoin() {
  userJoined = true;
  joinEl.classList.add('hidden');
}
joinEl.addEventListener('click', () => {
  if (lastState && lastState.ratingKey) {
    const target = targetFromState(lastState);
    if (isFinite(target) && Math.abs(v.currentTime - target) > DRIFT) {
      applying = true;
      v.currentTime = target;
      setTimeout(() => { applying = false; }, 250);
    }
  }
  // The 'play' event only fires once v.play() actually resolves —
  // which can be a second or two after the click, while hls.js
  // buffers its first segment. In that gap a 3 s broadcast tick
  // carrying the still-Playing=false server state can sneak in,
  // run applyState's post-join sync, and v.pause() us right as
  // playback was about to begin. Stamping lastLocalActionMs makes
  // applyState reject those stale broadcasts; the optimistic
  // post('play') tells the server NOW so the very next broadcast
  // already carries Playing=true. Host-only; viewers can't drive.
  lastLocalActionMs = Date.now();
  if (isHost && lastState && lastState.ratingKey && !lastState.playing) {
    post('play');
  }
  v.play().catch(() => {});
  dismissJoin();
});
joinEl.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); joinEl.click(); }
});

// Pick a random tagline pair so the join screen feels alive across
// reloads. Pure cosmetic — the click handler doesn't care which one
// is shown.
const joinTaglines = [
  ['Take your seat.',        'click anywhere to join'],
  ['Settle in.',             'tap any spot to begin'],
  ['House lights dimming.',  'one click and we roll'],
  ['Find your spot.',        'press to begin'],
  ['Pull up a chair.',       'click here when ready'],
  ['Almost showtime.',       'click anywhere to join'],
];
function pickJoinTagline() {
  const [label, hint] = joinTaglines[Math.floor(Math.random() * joinTaglines.length)];
  const lbl = joinEl.querySelector('.label');
  const hnt = joinEl.querySelector('.hint');
  if (lbl) lbl.textContent = label;
  if (hnt) hnt.textContent = hint;
}
// The overlay is up the moment /watch loads. With the migration's
// thinner client, there's no buffer gate to wait for — a single user
// gesture is all we need to satisfy autoplay policy. Light it up now.
pickJoinTagline();
joinEl.classList.add('ready');


let loadedKey = null;       // ratingKey currently attached to the <video>
let lastSessionToken = 0;  // Plex session token last seen; used to detect transcoder restarts
let applying = false;      // suppress echo while we apply server state
// reattaching covers the gap from "hls.destroy() fires v.pause()" to
// "new hls.js instance is actually playing." Without it, the
// browser-driven pause that destroy() emits would echo to /control
// and the server would record an unsolicited pause. Cleared by the
// first 'playing' event after the reattach; the 8s safety timer is
// a backstop for the case where the new instance never plays.
let reattaching = false;
const DRIFT = 0.75;        // seconds of tolerable drift before a hard seek

function setStatus(text, kind) {
  syncEl.textContent = text;
  syncEl.classList.remove('locked', 'drifting');
  if (kind === 'locked') syncEl.classList.add('locked');
  if (kind === 'drifting') syncEl.classList.add('drifting');
}

// Show a non-interactive overlay during a mid-session transcoder restart
// (host seeked past Plex's transcoded edge → new SessionToken from server).
// restartPending lets the 'playing' listener below know it's safe to
// auto-dismiss the overlay — without it we'd accidentally hide the
// initial join overlay the moment the browser's autoplay-after-prior-
// gesture lets the video start.
let restartPending = false;
function showRestartOverlay() {
  joinEl.classList.remove('hidden', 'ready');
  const label = joinEl.querySelector('.label');
  const hint  = joinEl.querySelector('.hint');
  if (label) label.textContent = 'Reseating the audience…';
  if (hint)  hint.textContent  = 'preparing the new scene';
  restartPending = true;
}

// Auto-dismiss only when we're recovering from a restart. The normal
// initial-join overlay must wait for the user's click — otherwise
// returning to a movie that the browser autoplays on remembers the
// gesture would never show the play button at all.
v.addEventListener('playing', () => {
  if (restartPending) {
    restartPending = false;
    joinEl.classList.add('hidden');
  }
  // First 'playing' after a reattach means the new hls.js instance is
  // settled. Re-arm pause/play echoes so subsequent user actions land
  // on the server as expected.
  if (reattaching) {
    reattaching = false;
  }
});

// maybeAutoplay is the single gate for auto-dismissing the join
// overlay. Three independent conditions all have to be true:
//   1. authoritative state has arrived (so we know whether the server
//      wants playback to be running),
//   2. the server says playing=true and v is currently paused,
//   3. the browser has enough buffered data that v.play() has a real
//      chance of succeeding (readyState ≥ HAVE_CURRENT_DATA).
//
// Both 'canplay' (signal #3 just became true) and applyState (signals
// #1 / #2 just became true) call this; whichever happens last triggers
// the actual v.play(). On success the overlay auto-dismisses; on
// autoplay-denied the overlay stays for the user's manual click.
function maybeAutoplay() {
  if (userJoined) return;
  if (!lastState || !lastState.playing) return;
  if (!v.paused) return;
  if (v.readyState < 2 /* HAVE_CURRENT_DATA */) return;
  v.play().then(() => {
    if (!userJoined) dismissJoin();
  }).catch(() => {/* autoplay denied — overlay stays for manual click */});
}
v.addEventListener('canplay', maybeAutoplay);

let hlsInstance = null;

function attach() {
  const src = '/hls/index.m3u8?st=' + lastSessionToken;

  // Quick capability probe — surface HEVC support up front so a Linux/Firefox
  // user sees a useful message instead of an empty player.
  const hevcMaybe = MediaSource.isTypeSupported
    && MediaSource.isTypeSupported('video/mp4; codecs="hvc1.2.4.L150.B0"');
  console.log('player: native HLS =', v.canPlayType('application/vnd.apple.mpegurl'),
              ' MSE HEVC =', hevcMaybe);

  // Prefer hls.js over the browser's native HLS player whenever it's
  // supported. Only fall back to native HLS if hls.js isn't supported
  // (very old browsers, or iOS Safari where MSE is restricted).
  if (window.Hls && Hls.isSupported()) {
    const hls = new Hls();
    const seekableStr = () => {
      const r = [];
      for (let i = 0; i < v.seekable.length; i++) {
        r.push(v.seekable.start(i).toFixed(1) + '..' + v.seekable.end(i).toFixed(1));
      }
      return '[' + r.join(', ') + ']';
    };
    hls.on(Hls.Events.MANIFEST_PARSED, (_, data) => {
      console.log('hls manifest parsed: levels=', data.levels.length,
                  ' v.duration=', v.duration,
                  ' v.seekable=', seekableStr());
    });
    hls.on(Hls.Events.LEVEL_LOADED, (_, data) => {
      const d = data.details || {};
      console.log('hls level loaded:',
                  ' live=', d.live,
                  ' type=', d.type,
                  ' totalDur=', d.totalduration ? d.totalduration.toFixed(1) : '?',
                  ' fragments=', d.fragments ? d.fragments.length : '?',
                  ' endList=', d.endList,
                  ' v.duration=', v.duration,
                  ' v.seekable=', seekableStr());
    });
    hls.on(Hls.Events.ERROR, (_, data) => {
      if (!data.fatal) return;
      console.error('hls.js FATAL', data);
      setStatus('player error: ' + (data.details || data.type));
    });
    hls.loadSource(src);
    hls.attachMedia(v);
    hlsInstance = hls;

    return;
  }
  v.src = src;
}

v.addEventListener('error', () => {
  const err = v.error;
  console.error('video element error', err);
  if (!err) return;
  setStatus('video error: ' + err.code + ' ' + (err.message || ''));
});



function post(action, extra) {
  fetch('/control', {
    method:'POST', headers:{'Content-Type':'application/json'},
    body: JSON.stringify(Object.assign({action}, extra||{}))
  });
}

// --- apply authoritative server state ---------------------------------------
// Latest known state from the server. We hold onto it so a periodic
// drift-correction tick (below) can extrapolate where playback SHOULD
// be at any moment, not just on the moments SSE pushes a change.
let lastState = null;

function targetFromState(s) {
  if (!s) return null;
  let t = s.positionSec;
  if (s.playing) t += (Date.now() - s.updatedAtMs) / 1000;
  return t;
}

// Host's player is the source of truth for its own playback. We only
// auto-seek it ONCE (the very first state we receive — useful when
// the host refreshes their tab mid-movie and we want to resume them
// at the right position). After that, host's v.currentTime is law,
// and we instead push the host's actual position back to the server
// (see broadcastHostPosition below) so viewers can sync to reality.
let firstStateApplied = false;

function applyState(s, reason) {
  if (lastLocalActionMs && s.updatedAtMs && s.updatedAtMs < lastLocalActionMs - 500) {
    console.log('ignoring stale state @', s.updatedAtMs, '(local action @', lastLocalActionMs, ')');
    return;
  }
  lastState = s;
  if (!s.ratingKey) {
    // Session ended (idle shutdown or host cleared). The server will
    // now serve waiting.html at /watch — reload so we land there.
    window.location.reload();
    return;
  }
  titleEl.textContent = s.title;
  titleEl.classList.remove('idle');
  if (typeof s.durationSec === 'number' && s.durationSec > 0) {
    serverDuration = s.durationSec;
    updateScrub();
  }
  renderViewers(s.viewers);
  refreshHostUI(s);

  // Status lights track live playback: Run glows mint while playing,
  // Pause glows amber while paused (CSS keys off #wrap.playing).
  wrapEl.classList.toggle('playing', !!s.playing);

  const ratingKeyChanged   = s.ratingKey !== loadedKey;
  const sessionTokenChanged = s.sessionToken && s.sessionToken !== lastSessionToken;
  if (ratingKeyChanged || sessionTokenChanged) {
    loadedKey = s.ratingKey;
    if (s.sessionToken) lastSessionToken = s.sessionToken;
    // Suppress echo BEFORE destroying — destroy() synchronously calls
    // v.pause() which fires the 'pause' event listener. Without this
    // guard the listener would POST an unsolicited pause to /control
    // and the host's player would lock up at whatever position the
    // reattach happened.
    reattaching = true;
    setTimeout(() => { reattaching = false; }, 8000);
    if (hlsInstance) {
      try { hlsInstance.destroy(); } catch (e) {}
      hlsInstance = null;
    }
    if (s.ratingKey) {
      attach();
      // Only show "preparing the new scene" overlay when this is a restart
      // of an existing session, not the initial load (the initial-load case
      // already has the join overlay visible).
      if (sessionTokenChanged && !ratingKeyChanged) {
        showRestartOverlay();
      }
    }
  }

  const target = targetFromState(s);
  applying = true;
  const shouldCorrect = !isHost || !firstStateApplied;
  if (shouldCorrect && Math.abs(v.currentTime - target) > DRIFT && isFinite(target)) {
    if (reason) console.log('apply', reason, '→ seek', target.toFixed(2));
    v.currentTime = target;
  }
  firstStateApplied = true;
  // Two paths, deliberately exclusive:
  //   Pre-join (overlay still up): maybeAutoplay() is the only path
  //     that calls v.play() — it bundles dismissJoin() on success so
  //     the overlay actually goes away when autoplay is allowed.
  //     Calling v.play() outside of that path would race with the
  //     canplay-triggered maybeAutoplay and leave the overlay up
  //     over playing video.
  //   Post-join: keep v ↔ server state aligned.
  if (!userJoined) {
    maybeAutoplay();
  } else {
    if (s.playing && v.paused) v.play().catch(() => {});
    if (!s.playing && !v.paused) v.pause();
  }
  setTimeout(() => { applying = false; }, 250);

  const drift = v.currentTime - target;
  if (isHost) {
    // The host IS the source of truth — measuring drift against the
    // server's extrapolation of the host's own position is circular,
    // and the small value (sub-second) just reflects browser media-
    // pipeline latency. Show role label instead.
    syncEl.textContent = 'host';
    syncEl.classList.remove('drifting');
    syncEl.classList.add('locked');
  } else {
    syncEl.textContent = 'drift ' + drift.toFixed(2) + 's';
    syncEl.classList.toggle('drifting', Math.abs(drift) > DRIFT / 2);
    syncEl.classList.toggle('locked',   Math.abs(drift) <= DRIFT / 2);
  }
}

// openLiveEvents wires its own pagehide close — critical here because
// bfcache-held EventSource connections balloon the server's viewer
// count from a single Back/Forward navigation.
//
// The server sends a one-shot {clientId} envelope as the FIRST SSE
// message (per connection). Stash it so /api/heartbeat can identify
// us; downstream messages are normal State broadcasts and flow into
// applyState as usual.
let clientId = null;
openLiveEvents((msg) => {
  if (msg && typeof msg.clientId === 'string' && !msg.ratingKey && !('positionSec' in msg)) {
    clientId = msg.clientId;
    return;
  }
  applyState(msg, 'sse');
});

// Heartbeat: tell the server what we're actually seeing every 5 s.
// Admin panel uses this to render "viewer N is at HH:MM:SS, paused/
// playing." Gated on visibility so a hidden tab stops chattering.
setInterval(() => {
  if (!clientId) return;
  if (document.visibilityState === 'hidden') return;
  fetch('/api/heartbeat', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      clientId: clientId,
      positionSec: v.currentTime || 0,
      paused: !!v.paused,
    }),
  }).catch(() => {/* network blip, retry next tick */});
}, 5000);

// Periodic drift correction. SSE only fires on state changes, so a
// viewer who joins mid-playback and gets stuck on the autoplay
// overlay can fall arbitrarily far behind by the time they click.
// This tick re-applies the extrapolated state every 2 s while the
// host is playing, hard-seeking when drift exceeds DRIFT.
setInterval(() => {
  // Skip when the tab isn't visible — nothing on screen needs the
  // indicator updated, and viewers in a hidden tab don't need their
  // playhead aggressively re-aligned to a server we can't render
  // for. Tab visible again = next tick catches up.
  if (document.visibilityState === 'hidden') return;
  if (!lastState || !lastState.ratingKey) return;
  if (!lastState.playing) {
    // Paused or idle. Still update the indicator so the user sees
    // the current intent, just don't try to correct anything.
    syncEl.textContent = 'paused';
    syncEl.classList.remove('drifting', 'locked');
    return;
  }
  const target = targetFromState(lastState);
  if (!isFinite(target)) return;
  const drift = v.currentTime - target;

  // Always update the visible indicator. For the host this stays at
  // "host" (drift against own extrapolation is meaningless); for
  // viewers it's the live numeric value with locked/drifting state.
  if (isHost) {
    syncEl.textContent = 'host';
    syncEl.classList.remove('drifting');
    syncEl.classList.add('locked');
  } else {
    syncEl.textContent = 'drift ' + drift.toFixed(2) + 's';
    syncEl.classList.toggle('drifting', Math.abs(drift) > DRIFT / 2);
    syncEl.classList.toggle('locked',   Math.abs(drift) <= DRIFT / 2);
  }

  // Host: never auto-correct. Host's v.currentTime is the source of
  // truth — if their 4K HEVC decoder is lagging, we DO NOT want to
  // hard-seek forward (it'd cause visible skips).
  if (isHost) return;

  if (applying || v.paused) return;
  if (Math.abs(drift) > DRIFT) {
    console.log('drift tick: correcting', drift.toFixed(2), 's → seek', target.toFixed(2));
    applying = true;
    v.currentTime = target;
    setTimeout(() => { applying = false; }, 250);
  }
}, 2000);

// NOTE: previously we ran a "host position broadcast" tick that
// periodically pushed the host's v.currentTime to the server so
// viewers would track the host's actual playback (in case decode
// lag made server's 1× extrapolation diverge). We removed it
// because hls.js occasionally resets v.currentTime to 0 during
// internal playlist/buffer maintenance, and even with stability
// checks the tick eventually trusted a bogus reading and
// broadcast it as a seek — producing the visible "seek storm"
// (state jumping 50 → 0 → 51 → 0 → 54 multiple times per second).
//
// Server state is now set by explicit host actions only:
//   * play / pause  → button presses
//   * seek          → scrub-bar drag (commitScrubSeek)
// Between actions, server extrapolates at 1×; viewers follow that
// extrapolation. If host's decoder runs at 0.97× the divergence
// after an hour is ~2 min — still vastly better than the chaos.
// A user-triggered scrub / pause re-anchors the state precisely.

// --- local controls broadcast to everyone -----------------------------------
// Viewers can't drive playback; their event echoes don't post.
//
// We intentionally do NOT echo `seeked` events. hls.js fires `seeked`
// on its own internal seeks (playlist updates, buffer rewinds during
// recovery, etc.) and treating each one as "the user wants to seek"
// floods the server with bogus state updates. The scrub-bar path
// (commitScrubSeek) is the only entry point that posts a seek; the
// host-position-broadcast tick keeps the server's view aligned with
// natural playback drift.
document.getElementById('play').onclick  = () => { if (isHost) post('play'); };
document.getElementById('pause').onclick = () => { if (isHost) post('pause'); };
// 'play' echoes are NOT suppressed during reattaching. The
// reattaching flag exists to swallow the synchronous v.pause() that
// hls.destroy() fires; nothing in the attach path emits a synthetic
// 'play' event. Suppressing legitimate plays (e.g. the user clicking
// the join overlay right after a fresh load, while the 8s reattaching
// timer is still active) leaves the server thinking Playing=false
// and the next broadcast pauses the player.
v.addEventListener('play',  () => { if (isHost && !applying) post('play');  });
v.addEventListener('pause', () => { if (isHost && !applying && !reattaching) post('pause'); });

// Click anywhere on the video to toggle play/pause (host only) — the
// join overlay catches the first click pre-join, so this only fires
// once playback is actually rolling. Viewers can't drive playback,
// so the click is a no-op for them.
v.addEventListener('click', () => {
  if (!isHost) return;
  if (v.paused) post('play');
  else          post('pause');
});

// Library link: hosts get a synchronous /control stop first so the
// Plex transcoder shuts down before the page changes — otherwise it
// would keep running until the idle timer fires (or someone reloads
// /watch on an empty session). Viewers fall through to the plain
// navigation; the session belongs to the host.
document.getElementById('library-link').addEventListener('click', (e) => {
  if (!isHost) return;
  e.preventDefault();
  fetch('/control', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({action: 'stop'}),
  }).finally(() => { window.location.href = '/'; });
});

// Fullscreen toggle. Fullscreens #wrap (not the bare <video>) so the
// top bar + scrub bar stay visible — same chrome as windowed mode,
// just bigger. The Fullscreen API is per-tab and entirely client-
// side; nothing about it touches the host/viewer sync. (wrapEl is
// declared with the other element refs at the top of this file.)
document.getElementById('fullscreen').onclick = () => {
  if (document.fullscreenElement) {
    document.exitFullscreen();
  } else {
    wrapEl.requestFullscreen().catch(err => console.warn('fullscreen failed:', err));
  }
};
// Light the fullscreen button mint while active (mirrors the zoom 'fill'
// indicator). Listen on document so Esc / F11 exits — which bypass our
// click handler — still update the button.
document.addEventListener('fullscreenchange', () => {
  wrapEl.classList.toggle('fs', document.fullscreenElement === wrapEl);
});

// Volume control — per-client (each viewer has their own), persists
// in localStorage so a refresh doesn't blow it away. Mute remembers
// the last non-zero level so toggling restores where the user was.
const volCell    = document.getElementById('volume');
const volIcon    = volCell.querySelector('.vol-icon');
const volSlider  = document.getElementById('vol-slider');
const volReadout = document.getElementById('vol-readout');
function readVol() {
  try {
    const raw = localStorage.getItem('volume');
    const muted = localStorage.getItem('muted') === '1';
    const lvl = raw === null ? 1 : Math.max(0, Math.min(1, parseFloat(raw)));
    return { lvl: isNaN(lvl) ? 1 : lvl, muted };
  } catch { return { lvl: 1, muted: false }; }
}
function saveVol(lvl, muted) {
  try {
    localStorage.setItem('volume', String(lvl));
    localStorage.setItem('muted', muted ? '1' : '0');
  } catch {}
}
function applyVol(lvl, muted) {
  v.volume = lvl;
  v.muted = muted;
  // Slider always reflects the volume LEVEL, even when muted — that
  // way clicking unmute restores the same height the user set.
  const pct = Math.round(lvl * 100);
  volSlider.value = pct;
  volSlider.style.setProperty('--vol', pct + '%');
  volReadout.textContent = muted ? 'muted' : pct;
  volIcon.classList.remove('vol-state-hi', 'vol-state-mid', 'vol-state-low', 'vol-state-muted');
  volCell.classList.toggle('muted', muted);
  if (muted || pct === 0)      volIcon.classList.add('vol-state-muted');
  else if (pct >= 65)          volIcon.classList.add('vol-state-hi');
  else if (pct >= 30)          volIcon.classList.add('vol-state-mid');
  else                         volIcon.classList.add('vol-state-low');
  saveVol(lvl, muted);
}
(function initVol() {
  const s = readVol();
  applyVol(s.lvl, s.muted);
})();
volSlider.addEventListener('input', () => {
  const lvl = volSlider.value / 100;
  // Dragging the slider implicitly unmutes — bringing it up was the
  // user's intent.
  applyVol(lvl, lvl === 0);
});
// Click anywhere on the icon (but NOT the slider/popover) toggles mute.
volCell.addEventListener('click', (e) => {
  if (e.target.closest('.vol-popover')) return;
  const muted = !v.muted;
  applyVol(v.volume || 0.5, muted); // restore a sensible level if 0
});
// Mouse wheel over the volume cell adjusts in 5% steps.
volCell.addEventListener('wheel', (e) => {
  e.preventDefault();
  const step = e.deltaY < 0 ? 0.05 : -0.05;
  const lvl = Math.max(0, Math.min(1, v.volume + step));
  applyVol(lvl, lvl === 0);
}, { passive: false });

// Zoom toggle: 'fit' (object-fit: contain, preserves aspect, may
// letterbox) ↔ 'fill' (object-fit: cover, zooms to crop the bars off
// at the cost of a sliver of frame edges). Preference persists in
// localStorage so it survives reloads and tab restarts.
const zoomBtn = document.getElementById('zoom');
function applyZoom(mode) {
  if (mode === 'fill') {
    wrapEl.classList.add('fill');
    zoomBtn.textContent = 'fill';
  } else {
    wrapEl.classList.remove('fill');
    zoomBtn.textContent = 'fit';
  }
  try { localStorage.setItem('zoom', mode); } catch {}
}
applyZoom((() => { try { return localStorage.getItem('zoom') || 'fit'; } catch { return 'fit'; } })());
zoomBtn.onclick = () => {
  applyZoom(wrapEl.classList.contains('fill') ? 'fit' : 'fill');
};

// Keyboard shortcuts: space = play/pause (host), 'f' = fullscreen,
// 'z' = zoom toggle, 'm' = mute toggle, ↑/↓ = volume ±5%. All
// ignore input fields so typing in the search box doesn't fire.
window.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') { closeHandoff(); return; }
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
  if (e.key === ' ' || e.code === 'Space') {
    e.preventDefault();
    if (isHost) post(v.paused ? 'play' : 'pause');
    return;
  }
  if (e.key === 'f' || e.key === 'F') { document.getElementById('fullscreen').click(); return; }
  if (e.key === 'z' || e.key === 'Z') { zoomBtn.click(); return; }
  if (e.key === 'm' || e.key === 'M') {
    applyVol(v.volume || 0.5, !v.muted);
    return;
  }
  if (e.key === 'ArrowUp')   { e.preventDefault(); applyVol(Math.min(1, v.volume + 0.05), false); return; }
  if (e.key === 'ArrowDown') { e.preventDefault(); applyVol(Math.max(0, v.volume - 0.05), v.volume - 0.05 <= 0); return; }
});
