package main

import (
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
	return ps.plex.BaseURL + "/video/:/transcode/universal/start?" + q.Encode()
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

// Start begins a Plex transcode session for ratingKey at the given
// movie-time offset (seconds). On success, the playlist URL is captured
// and sessionToken is bumped so clients can detect the new session.
func (ps *PlexSession) Start(ratingKey string, offsetSec float64) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	playlistURL := ps.transcodeURL(ratingKey, offsetSec)
	// Sanity check: a GET should return 200 with an m3u8 body.
	req, _ := http.NewRequest(http.MethodGet, playlistURL, nil)
	req.Header.Set("Accept", "application/vnd.apple.mpegurl")
	resp, err := ps.plex.http.Do(req)
	if err != nil {
		return fmt.Errorf("plex start: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := make([]byte, 1024)
		n, _ := resp.Body.Read(body)
		return fmt.Errorf("plex start: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:n])))
	}
	ps.ratingKey = ratingKey
	ps.offsetMs = int64(offsetSec * 1000)
	ps.edgeMs = ps.offsetMs
	ps.playlistURL = playlistURL
	ps.sessionID = sessionIDFromURL(playlistURL)
	ps.sessionToken++
	return nil
}

// Stop tells Plex to terminate the current transcode session. Safe to
// call when no session is active (no-op).
func (ps *PlexSession) Stop() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.stopLocked()
}

func (ps *PlexSession) stopLocked() {
	if ps.sessionID == "" {
		return
	}
	stopURL := fmt.Sprintf("%s/video/:/transcode/universal/stop?session=%s&X-Plex-Token=%s",
		ps.plex.BaseURL, url.QueryEscape(ps.sessionID), url.QueryEscape(ps.plex.Token))
	req, _ := http.NewRequest(http.MethodGet, stopURL, nil)
	if resp, err := ps.plex.http.Do(req); err == nil {
		resp.Body.Close()
	}
	ps.ratingKey = ""
	ps.sessionID = ""
	ps.playlistURL = ""
	ps.offsetMs = 0
	ps.edgeMs = 0
}

// sessionIDFromURL pulls the session= param out of our transcode URL
// (we generate it on the watchparty side and pass it to Plex).
func sessionIDFromURL(u string) string {
	idx := strings.Index(u, "session=")
	if idx == -1 {
		return ""
	}
	rest := u[idx+len("session="):]
	if amp := strings.IndexByte(rest, '&'); amp >= 0 {
		rest = rest[:amp]
	}
	return rest
}

// Restart stops the current Plex session and starts a new one at the
// given offset. Used when the host seeks forward into untranscoded
// territory. Atomic under ps.mu: concurrent restarts serialize.
func (ps *PlexSession) Restart(offsetSec float64) error {
	ps.mu.Lock()
	ratingKey := ps.ratingKey
	ps.mu.Unlock()
	if ratingKey == "" {
		return fmt.Errorf("Restart with no active session")
	}
	ps.mu.Lock()
	ps.stopLocked()
	ps.mu.Unlock()
	return ps.Start(ratingKey, offsetSec)
}

// EdgeSec is the highest segment-end time observed in Plex's playlist
// so far, in seconds (absolute movie time). Used to decide whether a
// seek target requires a Plex Restart.
func (ps *PlexSession) EdgeSec() float64 {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return float64(ps.edgeMs) / 1000.0
}

// UpdateEdge records a new edge time observed from a playlist parse.
// Never regresses — edges only grow within a session.
func (ps *PlexSession) UpdateEdge(edgeMs int64) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if edgeMs > ps.edgeMs {
		ps.edgeMs = edgeMs
	}
}

// RatingKey reports the active session's movie, or "" if none.
func (ps *PlexSession) RatingKey() string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.ratingKey
}

// PlaylistURL returns the current Plex playlist URL ("" if no session).
func (ps *PlexSession) PlaylistURL() string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.playlistURL
}

// OffsetMs reports the movie time at which the current Plex session
// began. Used by the playlist parser to compute absolute times.
func (ps *PlexSession) OffsetMs() int64 {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.offsetMs
}

// FetchPlaylist GETs the current session's playlist from Plex. Returns
// the raw m3u8 bytes. Caller is responsible for parsing/rewriting.
func (ps *PlexSession) FetchPlaylist() ([]byte, error) {
	ps.mu.Lock()
	plUrl := ps.playlistURL
	ps.mu.Unlock()
	if plUrl == "" {
		return nil, fmt.Errorf("no active Plex session")
	}
	req, _ := http.NewRequest(http.MethodGet, plUrl, nil)
	resp, err := ps.plex.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex playlist: status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// FetchSegment GETs the given Plex segment URL and returns the body as
// a ReadCloser the caller MUST close. The URL already contains the
// X-Plex-Token; Plex's segment endpoint authenticates from that.
func (ps *PlexSession) FetchSegment(segURL string) (io.ReadCloser, error) {
	req, _ := http.NewRequest(http.MethodGet, segURL, nil)
	resp, err := ps.plex.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("plex segment %s: status %d", redactedURL(segURL), resp.StatusCode)
	}
	return resp.Body, nil
}
