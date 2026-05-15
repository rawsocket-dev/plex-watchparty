# PLAN.md — plex-watchparty

Working document to resume this project on another machine. Captures the
goal, the design decisions (and the alternatives we rejected and *why*),
current state, and what's left.

---

## 1. Goal

Restream **one movie from Plex** to a group of friends, **synced**, where
friends authenticate with **only a shared password** — no Plex account, no
Plex access, and they should never be able to grab the source file or token.

Concretely, the required properties:

1. **Restream once** — server pulls Plex a single time and fans out (viewer
   count must not multiply Plex load).
2. **Synced playback** — host/any friend play/pause/seek applies to everyone.
3. **Password-only access** — no per-user Plex accounts.
4. **Source hidden** — clients never see the Plex URL or token.

Reference inspiration: `pion/rtwatch` (WebRTC, server-authoritative,
stream-only). We are *not* using it — see decisions below.

---

## 2. Key decisions (and rejected alternatives)

### Why not the off-the-shelf tools

| Option | Rejected because |
|---|---|
| Plex "Watch Together" | Discontinued by Plex (late 2026). |
| SyncLounge | Each viewer needs Plex access — violates password-only. |
| Jellyfin SyncPlay | Same: each client streams independently w/ server access. Also not Plex. |
| Neko (virtual browser) | Matches restream model but heavy (server renders a browser); overkill. |
| Fork rtwatch (WebRTC) | Viable, but WebRTC has **no browser HEVC support** — kills H.265 passthrough. Audio also always needs Opus transcode. |

### Why Path B: remux to HLS + SSE sync (CHOSEN)

The library is a **mix of H.264 and H.265**. WebRTC can't passthrough HEVC in
browsers; HLS/fMP4 + Media Source Extensions **can** play both H.264 and HEVC
with the container only stream-copied (`ffmpeg -c:v copy`). So:

- **Transport:** ffmpeg copy-remux Plex → fragmented-MP4 HLS (VOD playlist).
  Video is *never* transcoded (the expensive part). Audio is copied if AAC,
  else re-encoded to AAC (cheap, needed for broad browser support).
- **Sync:** no WebRTC. A server-authoritative state + **Server-Sent Events**
  push to clients + plain `POST /control` for actions. Chosen over websockets
  specifically to keep **zero external Go dependencies** (pure stdlib) and
  because server→client push is exactly SSE's shape.
- **Drift model:** state is authoritative position at `updatedAtMs`; while
  playing, clients extrapolate and hard-seek if drift > 0.75s.

Trade-off accepted: sync is ~sub-second (not frame-perfect like WebRTC), and
we own a small sync layer — but we get HEVC passthrough and a near-idle
server (IO-bound copy, no encode).

---

## 3. Architecture

```
Plex ──(one pull, server-side X-Plex-Token)──▶ ffmpeg -c:v copy ─▶ fMP4 HLS dir
                                                                       │
 browser ──password──▶ cookie ──▶ GET /hls/*  (segments served static) ┘
 browser ◀── SSE /events  (authoritative State on every change)
 browser ──▶ POST /control {load|play|pause|seek}  (any friend can drive)
```

The Plex token only ever appears in the ffmpeg input URL, server-side. The
browser only ever touches `/hls/*`, `/events`, `/control` — all behind the
password cookie.

### File map

| File | Responsibility |
|---|---|
| `main.go` | Env config, HTTP routes, static HLS serving, auth wiring |
| `plex.go` | Plex API: `ListMovies()`, `Resolve(ratingKey)` → Part URL + codecs |
| `auth.go` | Single shared password, HMAC session cookie, `Guard` middleware |
| `remux.go` | One ffmpeg HLS session at a time; `hvc1` tag for HEVC; waits for playlist |
| `sync.go` | Authoritative `State`, SSE hub, `/control` handler |
| `web.go` | `go:embed` of the 3 HTML files |
| `web/login.html` | Password form |
| `web/index.html` | Movie list (fetches `/api/movies`, posts `load`) |
| `web/player.html` | hls.js player + SSE drift-correction + control buttons |
| `Dockerfile` | 3-stage: latest static ffmpeg + Go build + Alpine runtime |
| `docker-compose.yml` | `docker compose up --build` with env vars |

### Config (env vars)

| Var | Required | Default |
|---|---|---|
| `PLEX_BASE_URL` | yes | — |
| `PLEX_TOKEN` | yes | — |
| `WATCH_PASSWORD` | yes | — |
| `LISTEN_ADDR` | no | `:8080` |
| `WORK_DIR` | no | `$TMPDIR/plexwatchparty` |

---

## 4. Current status

- ✅ All Go code written; `go build ./...`, `go vet`, `gofmt` clean.
- ✅ No external Go dependencies (stdlib only; `go.mod` has no requires).
- ✅ Dockerfile + compose + README + git repo with initial commit.
- ❌ **Not runtime-tested** — this machine had no ffmpeg and no Plex creds.
- ❌ **Docker image not built** — needs network to pull base images.

The Plex JSON struct shapes (`plex.go`) and the ffmpeg flag set (`remux.go`)
are the parts most likely to need adjustment against a real Plex server.

---

## 5. Known limitations / risks

1. **Seek-ahead stall:** VOD HLS playlist fills sequentially as ffmpeg copies;
   seeking past the remuxed point stalls until segments exist.
2. **Firefox + HEVC:** weak/no HEVC; H.265 titles play best Safari/Chrome.
3. **No HTTPS:** run behind a reverse proxy if exposed beyond LAN.
4. **Single session:** one movie streaming at a time (by design — it's a
   shared party). Changing movie restarts ffmpeg.
5. **No auth on Plex reachability errors surfaced minimally.**
6. **hls.js from CDN** in `player.html` — needs internet at view time, or
   vendor it locally for fully offline LAN use.

---

## 6. Next steps (priority order)

1. **Runtime test** against a real Plex: verify `plex.go` JSON shapes and the
   `ffmpeg` HLS flags actually produce a playable stream for both an H.264 and
   an H.265 title.
2. **Pin ffmpeg** in `Dockerfile` to a specific `mwader/static-ffmpeg:<ver>`
   tag for reproducible builds (currently `:latest`).
3. **`docker compose up --build`** end-to-end smoke test.
4. **Harden seeking:** options — switch to `hls_playlist_type event` +
   client-side seek clamping, or pre-segment, or fall back to a longer
   `-hls_time` / report "buffering past remux point" in the UI.
5. **Vendor hls.js** locally (drop the CDN dependency).
6. **TLS / reverse-proxy** guidance or built-in (Caddy sidecar in compose?).
7. Optional: host-only control (lock play/pause to one user) vs current
   "any friend can drive."
8. Optional: progress/seek bar reflecting remux availability.

---

## 7. Resume on another machine

```sh
git clone <remote-url> plex-watchparty && cd plex-watchparty

# Local dev (needs ffmpeg on PATH):
brew install ffmpeg                       # or apt: ffmpeg
PLEX_BASE_URL=http://<plex-ip>:32400 \
PLEX_TOKEN=<token> WATCH_PASSWORD=<pw> go run .

# Or Docker (bundles latest static ffmpeg, no local install):
export PLEX_TOKEN=<token> WATCH_PASSWORD=<pw>
docker compose up --build
```

Go 1.26+. Plex token: Plex web → any item → "Get Info" → View XML → copy
`X-Plex-Token` from the URL.
