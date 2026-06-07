# Changelog

Notable, user-facing changes to plex-watchparty. Newest first.
No version numbers — the app ships continuously as a container.

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
