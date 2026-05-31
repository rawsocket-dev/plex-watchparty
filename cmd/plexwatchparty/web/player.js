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
let myName = '';               // our display name (from whoami) — for the "(you)" label
function applyWhoami(d) {
  if (!d) return;
  isHost = !!d.isActiveHost;
  myName = d.name || '';
  document.body.classList.toggle('viewer', !isHost);
  const al = document.getElementById('admin-link');
  if (al) al.hidden = !d.isAdmin;
}
fetchWhoami().then(applyWhoami);
// Room chip (#room-cell): shows who's driving + the headcount, and its
// dropdown is the audience roster — which doubles as the hand-off picker.
// Anyone can open it to see who's here; only the active host gets tappable
// rows (a viewer row → POST /api/host/handoff to give them the remote).
// renderViewers() fills the value + list from the SSE state's .viewers.
const roomCell = document.getElementById('room-cell');
const roomPop  = document.getElementById('room-pop');
const roomList = document.getElementById('room-list');
const roomName = document.getElementById('room-name');
function openRoom()  { if (roomPop) roomPop.hidden = false; if (roomCell) roomCell.classList.add('open'); }
function closeRoom() { if (roomPop) roomPop.hidden = true;  if (roomCell) roomCell.classList.remove('open'); }
if (roomCell) {
  roomCell.addEventListener('click', (e) => {
    if (e.target.closest('#room-pop')) return; // row clicks handled below
    if (roomPop && roomPop.hidden) openRoom(); else closeRoom();
  });
}
if (roomList) {
  roomList.addEventListener('click', (e) => {
    const btn = e.target.closest('.pop-u[data-id]'); // only hand-off rows carry an id
    if (!btn) return;
    const id = btn.getAttribute('data-id');
    if (id) fetch('/api/host/handoff?id=' + encodeURIComponent(id), { method: 'POST' }).catch(() => {});
    closeRoom();
  });
}
// Outside-click closes the dropdown.
document.addEventListener('click', (e) => {
  if (!roomPop || roomPop.hidden) return;
  if (e.target.closest('#room-cell')) return;
  closeRoom();
});
// Re-fetch whoami (cache-bypassing) whenever the active host changes, so
// control visibility tracks election / hand-off without a reload.
function refreshHostUI(state) {
  const name = state.activeHostName || '';
  if (name !== lastActiveHostName) {
    lastActiveHostName = name;
    fetch('/api/whoami', { cache: 'no-store' }).then(r => r.ok ? r.json() : null).then(applyWhoami).catch(() => {});
  }
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
// Keyboard ±seek (Left/Right). Presses coalesce into one commit. While a
// seek is pending, seekKbTarget holds the destination so updateScrub shows
// it as a preview instead of snapping the thumb back to the live playhead —
// the same idea as the scrubDragging guard below.
let seekKbTarget = null;   // pending keyboard-seek destination (sec), or null
let seekKbTimer  = null;   // debounce timer; fires commitKbSeek once a burst settles
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
    // Hold the preview while dragging OR while a keyboard seek is pending,
    // so the thumb stays on the user's target instead of the live playhead.
    if (!scrubDragging && seekKbTarget == null) paintAt(cur / dur);
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

// --- keyboard ± seek (host only) --------------------------------------------
// Left/Right nudge the playhead by SEEK_STEP. Rapid presses — or a stuck /
// chattering key — accumulate into a single seek committed once the burst
// settles: one /control post, at most one Plex restart, instead of a
// per-press storm. The commit path mirrors commitScrubSeek exactly.
const SEEK_STEP = 10;       // seconds per arrow press
function nudgeSeek(deltaSec) {
  const dur = knownDuration();
  if (dur <= 0) return;
  // Anchor on the live playhead for the first press of a burst, then on the
  // running target so successive taps add up (3× Right → +30s, one seek).
  const base = (seekKbTarget == null) ? (v.currentTime || 0) : seekKbTarget;
  seekKbTarget = Math.max(0, Math.min(dur, base + deltaSec));
  paintAt(seekKbTarget / dur);            // live preview; held by updateScrub's guard
  if (seekKbTimer) clearTimeout(seekKbTimer);
  seekKbTimer = setTimeout(commitKbSeek, 350);
}
function commitKbSeek() {
  seekKbTimer = null;
  if (seekKbTarget == null) return;
  const target = seekKbTarget;
  seekKbTarget = null;
  // Same contract as commitScrubSeek: stamp the local action so an in-flight
  // SSE can't yank us back, seek locally (the browser clamps to seekable; a
  // forward seek past the transcoded edge is corrected by the reattach), and
  // let the server be the authority on whether a restart is needed.
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

// Render the Room chip (driver · count) + its dropdown roster from the SSE
// state's .viewers field. Source of truth is the SSE connection list, not
// bandwidth tracking — names come from the wp_name cookie set at login,
// with "guest" as the fallback. The active host (v.host) is marked
// "driving"; for the active host every other row is a tappable hand-off
// button. escapeHTML provided by /static/common.js
function renderViewers(list) {
  if (!Array.isArray(list) || list.length === 0) {
    if (roomCell) roomCell.hidden = true;
    closeRoom();
    return;
  }
  if (roomCell) roomCell.hidden = false;
  const driver = list.find(v => v.host);
  const driverLabel = driver
    ? (isHost ? ((myName ? myName + ' ' : '') + '(you)') : driver.name)
    : 'nobody';
  if (roomName) roomName.textContent = driverLabel + ' · ' + list.length;
  if (roomList) {
    roomList.innerHTML = list.map(v => {
      const drv = !!v.host;
      const nm = v.name || 'guest';
      const initial = escapeHTML((nm.trim()[0] || '?').toUpperCase());
      const youSuffix = (drv && isHost) ? ' (you)' : '';
      const handoff = isHost && !drv && v.id; // only the active host hands off
      const trailing = drv
        ? '<span class="role">driving</span>'
        : (handoff ? '<span class="arr">→</span>' : '<span class="role">viewer</span>');
      return '<button class="pop-u' + (drv ? ' driver' : '') + '"' +
        (handoff ? ' data-id="' + escapeHTML(v.id) + '"' : ' disabled') + '>' +
        '<span class="av' + (drv ? ' host' : '') + '">' + initial + '</span>' +
        '<span class="nm">' + escapeHTML(nm + youSuffix) + '</span>' +
        trailing + '</button>';
    }).join('');
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

// No autoplay: the join overlay always waits for the user's click (which
// is the gesture that lets v.play() start with sound). The click handler
// above does the rest.

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
    // Aim ~60s of forward buffer (hls.js default is 30). That's how far
    // ahead the browser requests segments — i.e. how far ahead we fetch +
    // cache from Plex. More cushion against network blips; kept modest so
    // hls.js doesn't race past what Plex has transcoded (which would 404
    // and trip a restart). The Plex /:/timeline reports keep Plex
    // transcoding ahead of this.
    const hls = new Hls({ maxBufferLength: 60 });
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
  // Drive the toolbar auto-hide off the shared playing state.
  setChromePlaying(!!s.playing);

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
  // Pre-join (overlay still up): do nothing — the user's click is what
  // starts playback. Post-join: keep v ↔ server state aligned.
  if (userJoined) {
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

// --- Auto-hide chrome + cursor while playing -------------------------
// After IDLE_MS of inactivity during playback the cursor hides and the
// toolbar + scrub slide away for an unobstructed picture. The cursor
// returns on ANY pointer movement; the toolbar returns only when the
// pointer is near the TOP (where the controls live) — so drifting the
// mouse over the video doesn't keep popping the bar up — or whenever
// playback pauses. Never hides while paused or while the hand-off picker
// is open.
const IDLE_MS = 5000;
let chromeTimer = null;   // hides the toolbar; reset by near-top activity
let cursorTimer = null;   // hides the cursor; reset by any activity
let chromePlaying = false;
let chromeZone = 120;     // px from the top that counts as "near the top"

function syncChromeHeight() {
  const bar = document.getElementById('bar');
  const scrub = document.getElementById('scrub');
  const h = (bar ? bar.offsetHeight : 0) + (scrub ? scrub.offsetHeight : 0);
  if (h > 0) {
    wrapEl.style.setProperty('--chrome-h', h + 'px');
    chromeZone = Math.max(h, 80);
  }
}

function hideChrome() {
  chromeTimer = null;
  if (!chromePlaying) return;                   // never hide while paused
  if (roomPop && !roomPop.hidden) return; // don't yank an open room dropdown
  syncChromeHeight();                           // measure before collapsing
  wrapEl.classList.add('chrome-hidden');
}
function showChrome() {
  wrapEl.classList.remove('chrome-hidden');
  if (chromeTimer) clearTimeout(chromeTimer);
  chromeTimer = chromePlaying ? setTimeout(hideChrome, IDLE_MS) : null;
}

function hideCursor() {
  cursorTimer = null;
  if (chromePlaying) wrapEl.classList.add('cursor-hidden');
}
function showCursor() {
  wrapEl.classList.remove('cursor-hidden');
  if (cursorTimer) clearTimeout(cursorTimer);
  cursorTimer = chromePlaying ? setTimeout(hideCursor, IDLE_MS) : null;
}

// Called from applyState whenever the shared playing state flips.
function setChromePlaying(playing) {
  chromePlaying = playing;
  if (!playing) {
    // Paused → controls + cursor stay up; stop the hide countdowns.
    if (chromeTimer) { clearTimeout(chromeTimer); chromeTimer = null; }
    if (cursorTimer) { clearTimeout(cursorTimer); cursorTimer = null; }
    wrapEl.classList.remove('chrome-hidden', 'cursor-hidden');
    return;
  }
  // Playing → start counting down (only if currently visible).
  if (!chromeTimer && !wrapEl.classList.contains('chrome-hidden')) {
    chromeTimer = setTimeout(hideChrome, IDLE_MS);
  }
  if (!cursorTimer && !wrapEl.classList.contains('cursor-hidden')) {
    cursorTimer = setTimeout(hideCursor, IDLE_MS);
  }
}

// Pointer activity: the cursor returns on ANY movement; the toolbar only
// when the pointer is near the top.
function onPointerActivity(e) {
  showCursor();
  if (typeof e.clientY !== 'number' || e.clientY <= chromeZone) {
    showChrome();
  }
}
['pointermove', 'pointerdown'].forEach((evt) =>
  window.addEventListener(evt, onPointerActivity, { passive: true })
);
window.addEventListener('resize', syncChromeHeight);

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
  // The Fit button is icon-only; its state shows via the #wrap.fill #zoom
  // active-glow. Don't touch the button's children — setting textContent
  // here would destroy the inline <svg> icon.
  wrapEl.classList.toggle('fill', mode === 'fill');
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
  if (e.key === 'Escape') { closeRoom(); return; }
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
  // Left/Right: host seeks ∓SEEK_STEP (coalesced — see nudgeSeek). For
  // everyone we preventDefault first: unhandled, these fall through to the
  // browser's default of scrolling the page sideways the instant any
  // horizontal overflow exists, walking the whole player off-axis (the
  // "picture slid left" glitch). Viewers can't drive playback, so for them
  // it's just the no-op scroll-guard. (Volume is on Up/Down above.)
  if (e.key === 'ArrowLeft')  { e.preventDefault(); if (isHost) nudgeSeek(-SEEK_STEP); return; }
  if (e.key === 'ArrowRight') { e.preventDefault(); if (isHost) nudgeSeek(+SEEK_STEP); return; }
});

// --- layout overflow watchdog -----------------------------------------
// The CSS guard (html,body{overflow-x:clip}) makes sure a stray reflow can
// never grow a horizontal scrollbar — but clipping HIDES the cause. This
// watchdog finds it: if any element ever extends past the viewport edge it
// logs the offender ONCE (selector + geometry) so the real trigger is
// captured instead of just manifesting as a slid / half-width picture.
// getBoundingClientRect reports true pre-clip geometry, so this still works
// behind overflow-x:clip. Prime suspect for the end-of-movie case is a
// video intrinsic-size change, so we also probe on the <video> 'resize'.
function checkOverflow(why) {
  const vw = window.innerWidth;
  let worst = null;
  document.querySelectorAll('body *').forEach((el) => {
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) return;
    const over = Math.max(r.right - vw, -r.left);   // px past right edge, or off the left
    if (over > 2 && (!worst || over > worst.over)) worst = { el, over: Math.round(over), r };
  });
  if (!worst || worst.el.dataset.ovLogged) return;  // log each offender once
  worst.el.dataset.ovLogged = '1';
  const e = worst.el;
  const sel = e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') +
    (typeof e.className === 'string' && e.className ? '.' + e.className.trim().split(/\s+/).join('.') : '');
  console.warn('[overflow]', why, '→', sel, 'extends', worst.over, 'px past the viewport',
    { left: Math.round(worst.r.left), right: Math.round(worst.r.right),
      width: Math.round(worst.r.width), viewport: vw });
}
window.addEventListener('resize', () => checkOverflow('window-resize'));
v.addEventListener('resize', () => checkOverflow('video-resize')); // fires when videoWidth/Height change
setInterval(() => { if (document.visibilityState !== 'hidden') checkOverflow('tick'); }, 3000);
