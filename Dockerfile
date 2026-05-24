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
FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 app
COPY --from=ffmpeg  /ffmpeg                 /usr/local/bin/ffmpeg
COPY --from=ffmpeg  /ffprobe                /usr/local/bin/ffprobe
COPY --from=build   /out/plexwatchparty     /usr/local/bin/plexwatchparty
USER app
ENV LISTEN_ADDR=:8080 \
    WORK_DIR=/tmp/plexwatchparty
EXPOSE 8080
ENTRYPOINT ["plexwatchparty"]
