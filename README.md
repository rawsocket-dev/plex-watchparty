# plex-watchparty

Restream a single movie from Plex and watch it **in sync** with friends who
need **only a shared password** — no Plex account, no Plex access.

The Plex token never leaves the server. Plex is pulled **once**; everyone
watches a remuxed HLS stream. Video (H.264 **and** H.265) is stream-copied —
no transcoding — so the server stays light regardless of viewer count.

## How it works

```
Plex ──(one pull, server-side token)──▶ ffmpeg -c:v copy ──▶ fMP4 HLS
                                                                  │
        password gate ──▶ /hls/*  segments  ◀───────────────────┘
        SSE /events  ◀── authoritative play/pause/seek ──▶ all viewers
```

- `plex.go` — Plex API: list movies, resolve a direct Part URL + codecs
- `remux.go` — one ffmpeg HLS session (`-c:v copy`, `hvc1` tag for HEVC)
- `auth.go` — single shared password, HMAC session cookie
- `sync.go` — authoritative state, SSE broadcast, control endpoint
- `web/` — login, movie list, drift-correcting hls.js player

## Run with Docker (recommended — bundles latest static ffmpeg)

```sh
export PLEX_TOKEN=xxxxxxxxxxxx
export WATCH_PASSWORD=movienight
docker compose up --build
```

Then open `http://<your-ip>:8080`, enter the password, pick a movie.

> Edit `PLEX_BASE_URL` in `docker-compose.yml` if Plex isn't reachable at
> `host.docker.internal:32400`.

## Run locally (needs ffmpeg on PATH)

```sh
brew install ffmpeg
PLEX_BASE_URL=http://192.168.1.10:32400 \
PLEX_TOKEN=xxxxxxxxxxxx \
WATCH_PASSWORD=movienight \
go run .
```

## Configuration

| Env var          | Required | Default                  |
|------------------|----------|--------------------------|
| `PLEX_BASE_URL`  | yes      | —                        |
| `PLEX_TOKEN`     | yes      | —                        |
| `WATCH_PASSWORD` | yes      | —                        |
| `LISTEN_ADDR`    | no       | `:8080`                  |
| `WORK_DIR`       | no       | `$TMPDIR/plexwatchparty` |

[Finding your Plex token.](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/)

## Known limitations

- Seeking far ahead before that part has been remuxed will stall (the VOD
  playlist fills sequentially).
- Firefox has weak HEVC support; H.265 titles play best in Safari/Chrome.
- No HTTPS — put it behind a reverse proxy if exposing beyond the LAN.
