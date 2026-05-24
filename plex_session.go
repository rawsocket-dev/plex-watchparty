package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PlexSession owns one active Plex Universal Transcoder session. Replaces
// the Remuxer's role of producing HLS — instead, Plex produces the HLS
// directly and we proxy. Sessions are restarted (Stop + Start at new
// offset) when the host seeks forward into untranscoded territory.
type PlexSession struct {
	plex          *Plex
	transcodeKbps int

	mu           sync.Mutex
	ratingKey    string
	sessionID    string // Plex's UUID session ID (currently unused for control; kept for logging)
	sessionToken int64  // bumps on every successful Start/Restart
	playlistURL  string // Plex /transcode/universal/start URL with all params
	offsetMs     int64  // movie time at which Plex's current session began
	edgeMs       int64  // max segment-end time observed in Plex's playlist
}

func NewPlexSession(p *Plex, transcodeKbps int) *PlexSession {
	return &PlexSession{plex: p, transcodeKbps: transcodeKbps}
}

// transcodeURL builds the Plex Universal Transcoder URL at the given
// movie-time offset (in seconds). Mirrors the param shape we validated
// against plezy in commit 011d48c.
func (ps *PlexSession) transcodeURL(ratingKey string, offsetSec float64) string {
	sessionID := "watchparty-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	profileExtra := strings.Join([]string{
		"add-settings(DirectPlayStreamSelection=true)",
		fmt.Sprintf("add-limitation(scope=videoCodec&scopeName=*&type=upperBound&name=video.bitrate&value=%d&replace=true)",
			ps.transcodeKbps),
		"add-transcode-target(type=videoProfile&context=streaming&protocol=http&container=mkv" +
			"&videoCodec=h264%2Chevc%2C*&audioCodec=opus%2Cvorbis%2Cflac%2C*" +
			"&subtitleCodec=ass%2Cpgs%2Cvobsub%2C*)",
		"add-transcode-target-settings(type=videoProfile&context=streaming" +
			"&protocol=http&CopyMatroskaAttachments=true)",
	}, "+")

	q := url.Values{}
	q.Set("hasMDE", "1")
	q.Set("path", "/library/metadata/"+ratingKey)
	q.Set("mediaIndex", "0")
	q.Set("partIndex", "0")
	q.Set("protocol", "hls")
	q.Set("fastSeek", "1")
	q.Set("directPlay", "0")
	q.Set("directStream", "0")
	q.Set("subtitleSize", "100")
	q.Set("audioBoost", "100")
	q.Set("location", "lan")
	q.Set("maxVideoBitrate", strconv.Itoa(ps.transcodeKbps))
	q.Set("addDebugOverlay", "0")
	q.Set("autoAdjustQuality", "0")
	q.Set("directStreamAudio", "0")
	q.Set("mediaBufferSize", "102400")
	q.Set("session", sessionID)
	q.Set("subtitles", "none")
	q.Set("copyts", "1")
	if offsetSec > 0 {
		q.Set("offset", strconv.FormatInt(int64(offsetSec), 10))
	}
	q.Set("Accept-Language", "en")
	q.Set("X-Plex-Session-Identifier", sessionID)
	q.Set("X-Plex-Client-Profile-Extra", profileExtra)
	q.Set("X-Plex-Chunked", "1")
	q.Set("X-Plex-Features", "external-media,indirect-media")
	q.Set("X-Plex-Model", "standalone")
	q.Set("X-Plex-Language", "en")
	q.Set("X-Plex-Product", "plexwatchparty")
	q.Set("X-Plex-Version", "1.0")
	q.Set("X-Plex-Client-Identifier", "plexwatchparty")
	q.Set("X-Plex-Platform", "Generic")
	q.Set("X-Plex-Device", "plexwatchparty")
	q.Set("X-Plex-Token", ps.plex.Token)
	return ps.plex.BaseURL + "/video/:/transcode/universal/start.m3u8?" + q.Encode()
}

// redactedURL hides the X-Plex-Token query param so the URL is safe to log.
func redactedURL(u string) string {
	idx := strings.Index(u, "X-Plex-Token=")
	if idx == -1 {
		return u
	}
	end := strings.Index(u[idx:], "&")
	if end == -1 {
		return u[:idx] + "X-Plex-Token=<redacted>"
	}
	return u[:idx] + "X-Plex-Token=<redacted>" + u[idx+end:]
}

// SessionToken returns the current session token. Bumps on every Start/Restart.
func (ps *PlexSession) SessionToken() int64 {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.sessionToken
}

// Placeholder references to imports — Start/Stop/FetchPlaylist etc. (added in
// later tasks) will use these. Without these the imports are flagged as unused.
var _ = json.Marshal
var _ = http.MethodGet
var _ io.Reader = nil
