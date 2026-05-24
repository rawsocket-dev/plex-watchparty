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

Two flavors, depending on whether you want to pull the CI-built image
or build locally:

**Pull the pre-built image (multi-arch, from this project's registry):**

```sh
# One-time: log in to the registry. See "Registry login" below for token.
docker login registry.example.com:5050 -u <access-token-user> -p <access-token>

export PLEX_TOKEN=xxxxxxxxxxxx
export WATCH_PASSWORD=movienight
docker compose pull
docker compose up -d
```

**Build locally instead:**

```sh
export PLEX_TOKEN=xxxxxxxxxxxx
export WATCH_PASSWORD=movienight
docker compose up --build
```

Then open `http://<your-ip>:8080`, enter the password, pick a movie.

> Edit `PLEX_BASE_URL` in `docker-compose.yml` if Plex isn't reachable at
> `host.docker.internal:32400`.

### Registry login

Each push to `master` publishes a multi-arch image (amd64 + arm64) at
`registry.example.com:5050/example/plex-watchparty:latest`. To pull it you
need a token with `read_registry` scope.

Easiest: create a **deploy token** (project-scoped, no human user
attached, ideal for a deploy host):

1. Go to
   <https://registry.example.com/example/plex-watchparty/-/settings/repository>
   → **Deploy tokens** → fill in a name (e.g. `prod-pull`), scope
   `read_registry`, and create.
2. the registry shows a generated **username** like `the registry+access-token-3`
   and a one-time **token value** — copy both now (the token is not
   shown again).
3. On the deploy host:

   ```sh
   docker login registry.example.com:5050 \
     -u the registry+access-token-3 -p <token-value>
   ```

   The `:5050` port is required — that's where the registry lives.
   Credentials get cached in `~/.docker/config.json`, so this is a
   one-time step per host.

For a personal laptop, a **personal access token**
(<https://registry.example.com/-/user_settings/personal_access_tokens>)
with `read_registry` scope works the same way — use your the registry
username instead of the access-token username.

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
| `HOST_PASSWORD`  | no       | unset (= no host role)   |
| `LISTEN_ADDR`    | no       | `:8080`                  |
| `WORK_DIR`       | no       | `$TMPDIR/plexwatchparty` |

When `HOST_PASSWORD` is set, the person who knows it is the **host** —
the only one who can pick a movie, play, pause, or seek. Everyone else
logs in with `WATCH_PASSWORD` and joins as a *viewer*: they see the
library but can't pick from it, and the player UI hides the playback
controls. If `HOST_PASSWORD` is unset (or equals `WATCH_PASSWORD`),
the original "any friend can drive" behaviour is preserved.

[Finding your Plex token.](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/)

## Known limitations

- Seeking far ahead before that part has been remuxed will stall (the VOD
  playlist fills sequentially).
- Firefox has weak HEVC support; H.265 titles play best in Safari/Chrome.
- No HTTPS — put it behind a reverse proxy if exposing beyond the LAN.
