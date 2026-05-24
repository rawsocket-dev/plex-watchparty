package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// segCtx is the per-segment context we encode into our rewritten URLs.
// Short JSON field names keep the base64 payload small.
type segCtx struct {
	PlexURL string `json:"u"` // original Plex segment URL (with token)
	StartMs int64  `json:"s"` // absolute movie time at segment start
	EndMs   int64  `json:"e"` // absolute movie time at segment end
	Rating  string `json:"k"` // ratingKey of the movie
}

func encodeSegCtx(c segCtx) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeSegCtx(enc string) (*segCtx, error) {
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	var c segCtx
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

type playlistSeg struct {
	OrigURL  string
	Duration float64
	StartMs  int64
	EndMs    int64
}

// rewritePlaylist parses an HLS media playlist and replaces segment
// URIs with our /hls/seg/<encoded>.ts form. baseURL is the URL the
// playlist was fetched from — segment URIs are resolved against it so
// segCtx.PlexURL is always absolute. sessionOffsetMs is the absolute
// movie time at which Plex's session started, so the returned StartMs
// values are absolute movie times suitable for cache indexing.
func rewritePlaylist(data []byte, baseURL string, sessionOffsetMs int64, ratingKey string) ([]byte, []playlistSeg, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse base URL: %w", err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	var segs []playlistSeg
	cumMs := sessionOffsetMs
	var pendingDur float64
	for i, raw := range lines {
		line := strings.TrimRight(string(raw), "\r")
		if strings.HasPrefix(line, "#EXTINF:") {
			rest := strings.TrimPrefix(line, "#EXTINF:")
			if comma := strings.IndexByte(rest, ','); comma >= 0 {
				rest = rest[:comma]
			}
			d, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("bad EXTINF duration %q: %w", rest, err)
			}
			pendingDur = d
			continue
		}
		if pendingDur > 0 && !strings.HasPrefix(line, "#") && line != "" {
			ref, err := url.Parse(line)
			if err != nil {
				return nil, nil, fmt.Errorf("parse segment URI %q: %w", line, err)
			}
			absURL := base.ResolveReference(ref).String()
			startMs := cumMs
			endMs := cumMs + int64(pendingDur*1000)
			seg := playlistSeg{
				OrigURL:  absURL,
				Duration: pendingDur,
				StartMs:  startMs,
				EndMs:    endMs,
			}
			segs = append(segs, seg)
			enc := encodeSegCtx(segCtx{
				PlexURL: absURL, StartMs: startMs, EndMs: endMs, Rating: ratingKey,
			})
			lines[i] = []byte("/hls/seg/" + enc + ".ts")
			cumMs = endMs
			pendingDur = 0
		}
	}
	return bytes.Join(lines, []byte{'\n'}), segs, nil
}
