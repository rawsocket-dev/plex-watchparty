# Changelog

Notable, user-facing changes to plex-watchparty. Newest first.
No version numbers — the app ships continuously as a container.

## 2026-07-25
- Added: TV libraries alongside Movies. Browse shows, seasons, and episodes,
  search the show grid, and load any library episode through the synchronized
  HLS player.
- Added: A host-only **Next Episode** control. Advancement is always explicit,
  crosses regular-season boundaries, skips empty regular seasons, and keeps
  season 0 specials isolated from the regular sequence.
- Added: Episode context throughout live/resume/recent state, the waiting room,
  admin session summary, audit events, and Discord embeds. Episode embeds omit
  movie-specific Rotten Tomatoes and TMDB links.
- Changed: The persisted library cache now retains shows and lazy-loaded
  season/episode hierarchy data while remaining compatible with movie-only
  cache, state, and recent JSON from earlier versions.
- Fixed: Cached TV hierarchies now refresh after 30 minutes and on an admin
  library refresh, while retaining stale seasons/episodes as an offline
  fallback when Plex is unavailable.
- Fixed: Periodic player-state updates can no longer re-enable **Next Episode**
  while a successor is still being prepared; the server also rejects
  concurrent next requests from multiple host tabs.
- Fixed: Concurrent show/season browsing can no longer corrupt
  `library-cache.json`, and TV hierarchy APIs reject keys outside the known
  show/season catalog.
- Fixed: Re-selecting the current episode now retries a failed successor
  lookup, so a brief Plex hierarchy error no longer hides **Next Episode**
  for the rest of playback.
- Changed: Specials appear after numbered seasons, matching Plex's library
  ordering. Specials remain isolated from regular-season progression.
- Fixed: Rapid TV navigation can no longer let a slower, stale show or season
  response overwrite the detail view selected afterward.

## 2026-07-02
- Fixed: a seek no longer triggers a burst of redundant Plex session
  restarts. In-flight segment requests from the just-replaced transcode
  session used to be mistaken for live failures and "recovered" by
  restarting Plex at their stale positions (~3 restarts per seek); segment
  URLs now carry their session identity and superseded ones are simply
  answered from cache or dropped while the player reattaches.
- Fixed: library poster art now shows without the Discord webhook configured.
  The `/poster` route was only mounted when `DISCORD_WEBHOOK_URL` was set, so
  plain deployments silently got gradient-only cards.
- Fixed: seeking outside the current transcode window now restarts Plex at the
  target. Previously a seek back to before the point a movie was resumed at,
  or forward into ranges cached from an earlier session, skipped the restart —
  and the player would stall or drift apart from the room state, since those
  positions aren't in the playlist the players hold.
- Fixed: a transient segment-fetch failure is no longer sent with cacheable
  headers, so a browser can't pin a one-off error and replay it to the player
  for a day.
- Fixed: the library now falls back to the last cached title list when Plex is
  unreachable at refresh time, instead of failing to load.
- Changed: viewer sync now compensates for a wrong client clock — the server's
  clock rides along on connect, so a machine whose clock is minutes off no
  longer sits exactly that far out of sync.
- Changed: hls.js is bundled with the app (pinned 1.6.16) instead of loaded
  from a CDN at runtime; playback no longer needs internet access.
- Changed: unauthenticated poster requests are only honored for titles in the
  library, and "no art" answers are remembered briefly — outside keys can no
  longer probe or hammer Plex.
- Fixed: assorted races — near-simultaneous movie loads can no longer leak an
  orphaned Plex transcoder session; kicking the same viewer twice at once can
  no longer crash the request; a full admin cache wipe no longer stalls
  viewers' streams while files delete.

## 2026-06-27
- Changed: Library cards now show the **IMDb** score and the **Rotten Tomatoes
  audience** score, each clearly labelled (`IMDb 5.9 · 🍿 7.7`). Previously the
  card showed Plex's unlabelled critic/audience numbers, which on libraries
  sourced from Rotten Tomatoes are the *Tomatometer* — easily mistaken for an
  IMDb score it didn't match (e.g. a film at 5.9 on IMDb showed 8.1). IMDb
  ratings are fetched from Plex per title and cached, so the library still
  opens instantly.

## 2026-06-26
- Changed: New "Velvet" look across every screen — true-black surfaces, an
  electric-violet accent (teal for "in sync"), glassy rounded cards, and a
  Space Grotesk + Hanken Grotesk type pairing. Sign-in, library, player,
  waiting room, and admin are all re-skinned; behavior is unchanged.
- Added: The library is now a poster grid with real Plex artwork. Each card
  shows the movie's poster (a per-title gradient stands in while it loads, or
  when a title has no art), with the ★ critic / 👥 audience rating and the
  year on a line beneath the card.
- Added: Posters load lazily as you scroll — faster scrolling fetches further
  ahead — and are cached on disk and served at card size (downscaled by Plex),
  so even a library of thousands of titles opens instantly instead of stalling
  on a flood of image downloads.
- Added: The admin Session panel shows a LIVE / IDLE state chip.
- Changed: Dismissing the "Resume where you left off" banner now hides that
  movie's prompt for good — including after its position is saved again. A
  different movie still prompts.
- Fixed: The resume banner no longer briefly shows an empty placeholder on load.
- Fixed: A single viewer on two tabs (or mid-reconnect) is no longer counted
  as two — connection logs report distinct people and a single active host.
- Fixed: Live updates send a keepalive every 2 seconds, so a short
  reverse-proxy timeout no longer cuts the stream and causes reconnect loops.

## 2026-06-12
- Added: The player toolbar shows a Quality readout — the source
  resolution + codec as a pill, then the fixed 1080p transcode target
  (e.g. "4K HEVC → 1080p"). Hidden when Plex doesn't report source
  dimensions.

## 2026-06-10
- Fixed: Dismissing the "Resume where you left off" banner now keeps it
  hidden across page reloads — it stays gone until a new resume hint
  (a different movie or a fresh save) appears.

## 2026-06-07
- Added: Library list shows each title's Plex rating — gold ★ critic and
  👥 audience score — between the title and the year, when Plex has them.
- Fixed: Volume slider popover no longer closes while you move the mouse
  from the icon down to the slider.
- Changed: Larger, more legible volume icon.

## 2026-06-05
- Added: Discord webhook — a rich "Now Playing" embed (poster, ratings,
  genres, plot, IMDb/Rotten Tomatoes/TMDB links) on movie start, and a
  notice on end. Optional; off unless DISCORD_WEBHOOK_URL is set.
- Fixed: Library loads no longer 502 — Plex sends both scalar and
  capitalized-array GUID/rating fields and Go's case-insensitive JSON
  matching collided.

## 2026-06-04
- Fixed: Playhead no longer snaps backward when Plex restarts or recovers
  mid-movie.
- Fixed: Joining a paused room no longer starts your own local playback.

## 2026-05-31
- Added: Movie title shows the release year.
- Added: Left/Right arrows seek ±10s (host only).
- Fixed: Arrow keys no longer nudge or scroll the picture.

## 2026-05-30
- Added: Admin email→display-alias mapping — override the Google name shown
  in every roster.

## 2026-05-29
- Added: Google sign-in for everyone, gated by email allowlists (replaces
  the shared watch/host passwords).
- Added: Single active host — exactly one eligible person holds the
  controls, with admin override and "pass the remote"; persists on restart.
- Added: Admin console (/admin) and a persisted audit log of sign-ins,
  admin, playback, and Plex events.
- Changed: Public roster collapses to one row per person, with a live count.
- Changed: Player toolbar auto-hides during playback; reveals on hover/pause.

## Earlier
- Synchronized Plex movie nights over the LAN: Plex's Universal Transcoder
  produces HLS, which the server proxies and rewrites (token-encrypted
  segment contexts) with an on-disk LRU cache for instant backward seeks;
  shared playback state over SSE with drift extrapolation.
