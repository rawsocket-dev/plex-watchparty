# syntax=docker/dockerfile:1

# --- build the Go binary (pure Go, no cgo) ----------------------------------
# Mirrored library/golang:1.26-alpine into our the registry registry so CI builds
# don't hit Docker Hub's unauthenticated pull rate limit. Rehydrate with
# crane from buildhost when bumping versions.
FROM registry.example.com:5050/example/plex-watchparty/golang:1.26-alpine AS build
WORKDIR /src
# Commit the build is from, stamped into the binary (main.version) and
# logged at startup. CI passes --build-arg VERSION=$CI_COMMIT_SHORT_SHA;
# local builds default to "dev".
ARG VERSION=dev
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/plexwatchparty .

# --- minimal runtime --------------------------------------------------------
# Mirrored library/alpine:3.20. No ffmpeg here — playback goes through
# Plex's Universal Transcoder and we just proxy + cache the HLS output.
FROM registry.example.com:5050/example/plex-watchparty/alpine:3.20
# su-exec lets the entry-point chown the WORK_DIR as root, then drop
# to the unprivileged `app` user before exec-ing the server.
RUN apk add --no-cache ca-certificates su-exec \
 && adduser -D -u 10001 app
COPY --from=build   /out/plexwatchparty     /usr/local/bin/plexwatchparty
COPY entrypoint.sh                          /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENV LISTEN_ADDR=:8080 \
    WORK_DIR=/tmp/plexwatchparty
EXPOSE 8080
# NB: no USER directive — the entrypoint runs as root long enough to
# chown WORK_DIR, then exec's the server as `app`.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
