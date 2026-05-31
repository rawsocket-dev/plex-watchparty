package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// segCtx is the per-segment context we carry inside our rewritten URLs.
// Short JSON field names keep the encrypted payload small.
type segCtx struct {
	PlexURL string `json:"u"` // original Plex segment URL (with token)
	StartMs int64  `json:"s"` // absolute movie time at segment start
	EndMs   int64  `json:"e"` // absolute movie time at segment end
	Rating  string `json:"k"` // ratingKey of the movie
}

// segCodec encrypts the per-segment context into the opaque blob that
// appears in client-visible /hls/seg/<blob>.ts URLs.
//
// It exists because the context contains the upstream Plex segment URL —
// and that URL carries the X-Plex-Token. Base64 (the previous encoding)
// is reversible, so any authenticated viewer could decode the playlist
// URL and recover the Plex token (contradicting "the token never leaves
// the server"), and could just as easily MINT a context with an
// arbitrary PlexURL (SSRF) or ratingKey (cache-path traversal).
//
// AES-256-GCM fixes both: the token is no longer readable (confidentiality)
// and a forged or tampered blob fails the GCM tag check on decode
// (authenticity), so PlexURL and Rating coming out of decode are values
// the server itself produced.
type segCodec struct {
	aead cipher.AEAD
	rnd  io.Reader // nonce source; crypto/rand.Reader in production
}

// newSegCodec derives a 256-bit key from seed (the Plex token in
// production — stable across restarts, already secret, never sent to
// clients). Rotating the Plex token invalidates outstanding segment
// URLs, which is harmless: the player reattaches on the next playlist.
func newSegCodec(seed string) (*segCodec, error) {
	key := sha256.Sum256([]byte("plexwatchparty-segctx-v1\x00" + seed))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &segCodec{aead: gcm, rnd: rand.Reader}, nil
}

// encode seals the context and returns nonce||ciphertext, base64url'd.
func (sc *segCodec) encode(c segCtx) string {
	pt, _ := json.Marshal(c)
	nonce := make([]byte, sc.aead.NonceSize())
	if _, err := io.ReadFull(sc.rnd, nonce); err != nil {
		return "" // crypto/rand failure is fatal-ish; caller treats "" as no URL
	}
	sealed := sc.aead.Seal(nonce, nonce, pt, nil)
	return base64.RawURLEncoding.EncodeToString(sealed)
}

// decode authenticates and opens a blob. Any base64, length, GCM-tag, or
// JSON failure returns an error — the caller maps that to 404.
func (sc *segCodec) decode(enc string) (*segCtx, error) {
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	ns := sc.aead.NonceSize()
	if len(raw) < ns {
		return nil, fmt.Errorf("seg ctx too short")
	}
	pt, err := sc.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return nil, err
	}
	var c segCtx
	if err := json.Unmarshal(pt, &c); err != nil {
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
// URIs with our /hls/seg/<blob>.ts form. baseURL is the URL the playlist
// was fetched from — segment URIs are resolved against it so
// segCtx.PlexURL is always absolute. sessionOffsetMs is the absolute
// movie time at which Plex's session started, so the returned StartMs
// values are absolute movie times suitable for cache indexing.
func rewritePlaylist(codec *segCodec, data []byte, baseURL string, sessionOffsetMs int64, ratingKey string) ([]byte, []playlistSeg, error) {
	return walkPlaylist(data, baseURL, sessionOffsetMs, func(seg playlistSeg) string {
		return "/hls/seg/" + codec.encode(segCtx{
			PlexURL: seg.OrigURL, StartMs: seg.StartMs, EndMs: seg.EndMs, Rating: ratingKey,
		}) + ".ts"
	})
}

// parsePlaylistSegments walks an HLS media playlist and returns its
// segments as absolute-time ranges WITHOUT rewriting — used by the
// recovery path, which only needs segment metadata to pick a substitute
// and so doesn't need (and shouldn't carry) the encryption codec.
func parsePlaylistSegments(data []byte, baseURL string, sessionOffsetMs int64) ([]playlistSeg, error) {
	_, segs, err := walkPlaylist(data, baseURL, sessionOffsetMs, nil)
	return segs, err
}

// walkPlaylist is the shared parse loop. It computes each segment's
// absolute [StartMs, EndMs] window. When rewrite != nil, each segment
// URI line is replaced with rewrite(seg) and the joined playlist is
// returned; when nil, the returned bytes are nil (parse-only).
func walkPlaylist(data []byte, baseURL string, sessionOffsetMs int64, rewrite func(playlistSeg) string) ([]byte, []playlistSeg, error) {
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
			if rewrite != nil {
				lines[i] = []byte(rewrite(seg))
			}
			cumMs = endMs
			pendingDur = 0
		}
	}
	if rewrite == nil {
		return nil, segs, nil
	}
	return bytes.Join(lines, []byte{'\n'}), segs, nil
}
