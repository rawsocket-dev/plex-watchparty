# syntax=docker/dockerfile:1

# --- ffmpeg: pinned static build, full codec support (x265/HEVC, aac, ...) ---
# Mirrored from mwader/static-ffmpeg:8.1.1 into our the registry registry so CI
# builds don't hit Docker Hub's unauthenticated pull rate limit. Rehydrate
# with crane from buildhost when bumping versions.
FROM registry.example.com:5050/example/plex-watchparty/static-ffmpeg:8.1.1 AS ffmpeg

# --- build the Go binary (pure Go, no cgo) ----------------------------------
# Mirrored from library/golang:1.26-alpine — same reasoning as above.
FROM registry.example.com:5050/example/plex-watchparty/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/plexwatchparty .

# --- minimal runtime --------------------------------------------------------
# Mirrored from library/alpine:3.20 — same reasoning as the other FROMs.
FROM registry.example.com:5050/example/plex-watchparty/alpine:3.20
# su-exec lets the entry-point chown the WORK_DIR as root, then drop
# to the unprivileged `app` user before exec-ing the server.
RUN apk add --no-cache ca-certificates su-exec \
 && adduser -D -u 10001 app
COPY --from=ffmpeg  /ffmpeg                 /usr/local/bin/ffmpeg
COPY --from=ffmpeg  /ffprobe                /usr/local/bin/ffprobe
COPY --from=build   /out/plexwatchparty     /usr/local/bin/plexwatchparty
COPY entrypoint.sh                          /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENV LISTEN_ADDR=:8080 \
    WORK_DIR=/tmp/plexwatchparty
EXPOSE 8080
# NB: no USER directive — the entrypoint runs as root long enough to
# chown WORK_DIR, then exec's the server as `app`.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
