# plex-watchparty

Restream a single movie from Plex and watch it **in sync** with friends who
need **only a shared password** — no Plex account, no Plex access.

The Plex token never leaves the server. The watchparty proxy acts as a thin
caching layer over Plex's Universal Transcoder, fetching HLS segments on demand
and caching them to disk so backward seek is instant. Friends stay in sync via
Server-Sent Events — the host's play/pause/seek actions are broadcast to all
viewers.

## How it works

The watchparty server acts as a thin proxy + cache over Plex's Universal
Transcoder HLS output. When the host picks a movie, watchparty asks Plex to
start a transcode session at the requested offset. Plex produces HLS segments;
watchparty fetches the playlist on demand, rewrites segment URLs to route
through us, and caches fetched segments to disk so backward seek into a
previously-watched range is instant.

On forward seek past Plex's current transcoded position, watchparty restarts
Plex's transcoder at the new offset. Clients see a brief ~5-second pause (Plex
transcoder spin-up) and then playback resumes at the new position.

Friends watching together stay in sync via Server-Sent Events: the host's play
/ pause / seek actions are broadcast to all viewers, who track the host's
authoritative position with sub-second tolerance.

**Code structure:**

- `plex.go` — Plex API: list movies, start transcoder sessions, fetch + rewrite HLS playlists
- `cache.go` — LRU disk cache for HLS segments (survives restarts)
- `auth.go` — password gate, HMAC session cookies
- `sync.go` — authoritative playback state, SSE broadcast to viewers, host control endpoint
- `web/` — login, movie list, drift-correcting hls.js player

## Run with Docker (recommended)

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

## Run locally

```sh
PLEX_BASE_URL=http://192.168.1.10:32400 \
PLEX_TOKEN=xxxxxxxxxxxx \
WATCH_PASSWORD=movienight \
go run .
```

## Configuration

| Env var                           | Required | Default                  | Notes |
|-----------------------------------|----------|--------------------------|-------|
| `PLEX_BASE_URL`                   | yes      | —                        | Your Plex server URL, e.g. `http://192.168.1.10:32400` |
| `PLEX_TOKEN`                      | yes      | —                        | Plex auth token (stays server-side, never sent to clients) |
| `WATCH_PASSWORD`                  | yes      | —                        | Shared password for viewers (can be anyone) |
| `HOST_PASSWORD`                   | no       | unset (= any friend can drive) | Optional: password granting play/pause/seek privileges |
| `PLEX_TRANSCODE_BITRATE_KBPS`     | no       | unset (no transcode)     | Request Plex transcode at this bitrate (e.g., `12000` = 12 Mbps). Plex handles codec conversion (HEVC→H.264, HDR→SDR). Leave unset for direct-stream only. |
| `CACHE_MAX_GB`                    | no       | `20`                     | Disk cap for HLS segment cache in GB. Cached segments survive restarts; LRU eviction kicks in when cap is hit. Estimate ~10 GB per typical 2hr movie at 12 Mbps. |
| `LISTEN_ADDR`                     | no       | `:8080`                  | Listen address (e.g., `:8080` or `0.0.0.0:8080`) |
| `WORK_DIR`                        | no       | `$TMPDIR/plexwatchparty` | Root data directory for cache and work files |

**HOST_PASSWORD behavior:**

When `HOST_PASSWORD` is set, the person who knows it is the **host** — the only
one who can pick a movie, play, pause, or seek. Everyone else logs in with
`WATCH_PASSWORD` and joins as a *viewer*: they see the library but can't pick
from it, and the player UI hides the playback controls. If `HOST_PASSWORD` is
unset (or equals `WATCH_PASSWORD`), the original "any friend can drive"
behaviour is preserved.

[Finding your Plex token.](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/)

## Reverse proxy

The app speaks plain HTTP and is designed to sit behind your existing TLS
terminator. It honors `X-Forwarded-For` / `X-Real-IP` for bandwidth + viewer
attribution, and the SSE handler ships `X-Accel-Buffering: no` so nginx-family
proxies don't buffer events. Mount it at the root of a hostname — paths
like `/hls/`, `/events`, `/control` are absolute, so a `/watchparty/` subpath
won't work without a path-stripping rewrite.

Two knobs matter on every proxy: keep `proxy_read_timeout` generous (the first
HLS segment can take 1–6 s while Plex spins up its transcoder), and use
HTTP/1.1 with an empty `Connection` header for the SSE stream.

### nginx

```nginx
server {
    listen 443 ssl http2;
    server_name watchparty.example.com;
    # ssl_certificate / ssl_certificate_key ...

    location / {
        proxy_pass http://buildhost.example.com:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection        "";

        # First-segment Plex transcodes can take several seconds.
        proxy_read_timeout 300s;
        # Belt-and-suspenders with X-Accel-Buffering: no on /events.
        proxy_buffering off;
    }
}
```

### Caddy

```caddy
watchparty.example.com {
    reverse_proxy buildhost.example.com:8080 {
        transport http {
            read_timeout 5m
        }
    }
}
```

Caddy auto-provisions TLS, sets the X-Forwarded-* headers, and handles HTTP/1.1
+ `Connection` correctly out of the box — no extra configuration needed for
SSE or HLS streaming.

### Traefik (Docker labels)

Add these labels to the `watchparty` service in `docker-compose.yml`:

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.watchparty.rule=Host(`watchparty.example.com`)"
  - "traefik.http.routers.watchparty.entrypoints=websecure"
  - "traefik.http.routers.watchparty.tls.certresolver=letsencrypt"
  - "traefik.http.services.watchparty.loadbalancer.server.port=8080"
  # Long-running responses (HLS + SSE) need generous timeouts.
  - "traefik.http.services.watchparty.loadbalancer.responseforwarding.flushinterval=100ms"
```

And in your Traefik static config (`traefik.yml`):

```yaml
serversTransport:
  forwardingTimeouts:
    responseHeaderTimeout: 300s
    idleConnTimeout: 300s
```

## Known limitations

- Seeking far ahead before Plex has transcoded to that position will pause for
  ~5 seconds while Plex spins up a new transcoder at the target offset.
- Firefox has weak HEVC support; H.265 titles play best in Safari/Chrome.
- No HTTPS — put it behind a reverse proxy if exposing beyond the LAN.
