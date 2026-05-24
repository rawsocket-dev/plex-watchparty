# syntax=docker/dockerfile:1

# --- ffmpeg: pinned static build, full codec support (x265/HEVC, aac, ...) ---
# mwader/static-ffmpeg tracks upstream FFmpeg releases. Pinned for
# reproducible builds; bump intentionally when a new upstream release lands.
FROM mwader/static-ffmpeg:8.1.1 AS ffmpeg

# --- build the Go binary (pure Go, no cgo) ----------------------------------
FROM golang:1.26-alpine AS build
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
