# Changelog

Notable, user-facing changes to plex-watchparty. Newest first.
No version numbers — the app ships continuously as a container.

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
