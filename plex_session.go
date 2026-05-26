package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
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

// newSessionID mints a unique Plex transcode session id.
func newSessionID() string {
	return "watchparty-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// transcodeURL builds the Plex Universal Transcoder URL at the given
// movie-time offset (in seconds). Targets HLS output (mpegts/h264/aac)
// so we can proxy + cache Plex's playlist + segments directly. The
// caller passes a sessionID so /decision and /start.m3u8 can share
// the same one — Plex expects the negotiation + actual start to be
// stitched together by session.
func (ps *PlexSession) transcodeURL(ratingKey, sessionID string, offsetSec float64) string {
	// Profile-Extra teaches Plex's transcoder what we can play. For HLS
	// browser playback that's h264 video + aac audio inside mpegts
	// segments — matches what hls.js + MSE can decode without any
	// further muxing on our side. The add-limitation clause caps the
	// transcoded bitrate at our target.
	profileExtra := strings.Join([]string{
		fmt.Sprintf("add-limitation(scope=videoCodec&scopeName=*&type=upperBound&name=video.bitrate&value=%d&replace=true)",
			ps.transcodeKbps),
		"add-transcode-target(type=videoProfile&context=streaming&protocol=hls&container=mpegts" +
			"&videoCodec=h264&audioCodec=aac)",
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
	q.Set("session", sessionID)
	q.Set("subtitles", "none")
	q.Set("copyts", "1")
	q.Set("offset", strconv.FormatInt(int64(offsetSec), 10))
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

// decisionURL is the same as transcodeURL but hits Plex's /decision
// endpoint, which returns a JSON body explaining WHY a transcode was
// rejected (codec not supported, no matching target, etc.). Real Plex
// clients negotiate via /decision before issuing /start.m3u8 and
// share the same session ID across both calls — Plex's session
// bookkeeping expects that order. Used both as a preflight and (with
// the old fall-through path) as a post-mortem.
func (ps *PlexSession) decisionURL(ratingKey, sessionID string, offsetSec float64) string {
	return strings.Replace(ps.transcodeURL(ratingKey, sessionID, offsetSec),
		"/transcode/universal/start.m3u8?",
		"/transcode/universal/decision?", 1)
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
// movie-time offset (seconds). Real Plex clients negotiate via
// /decision before issuing /start.m3u8 and use the same session id
// across both — without the decision call, Plex sometimes returns a
// bare 400 from /start.m3u8 (even though the params would succeed
// after a brief warm-up). We mirror that pattern AND retry once
// after a short delay if the first /start still 400s, since Plex's
// /stop endpoint is unreliable and a freshly-stopped prior session
// can stay tracked for a few seconds.
func (ps *PlexSession) Start(ratingKey string, offsetSec float64) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.stopLocked()

	sessionID := newSessionID()

	// /decision first. Plex returns 200 + a JSON body describing
	// "Direct play not available; Conversion OK." when our transcode
	// target is acceptable — and that response also primes Plex's
	// session bookkeeping for the upcoming /start.m3u8 call. Failures
	// here are surfaced but not retried; if Plex can't even decide,
	// /start won't fare better.
	if err := ps.callDecision(ratingKey, sessionID, offsetSec); err != nil {
		return fmt.Errorf("plex decision: %w", err)
	}

	// /start.m3u8. Retry once after a 1.5s pause if Plex returns 4xx
	// — empirically that covers the "/stop ack but slot still held"
	// race that's plagued us in testing.
	var (
		playlistURL = ps.transcodeURL(ratingKey, sessionID, offsetSec)
		lastErr     error
	)
	for attempt := 1; attempt <= 2; attempt++ {
		req, _ := http.NewRequest(http.MethodGet, playlistURL, nil)
		req.Header.Set("Accept", "application/vnd.apple.mpegurl")
		resp, err := ps.plex.Do(req)
		if err != nil {
			return fmt.Errorf("plex start (%s): %w", redactedURL(playlistURL), err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body.Close()
			ps.ratingKey = ratingKey
			ps.offsetMs = int64(offsetSec * 1000)
			ps.edgeMs = ps.offsetMs
			ps.playlistURL = playlistURL
			ps.sessionID = sessionID
			ps.sessionToken++
			if attempt > 1 {
				log.Printf("plex start: succeeded on attempt %d", attempt)
			}
			return nil
		}
		body := make([]byte, 1024)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		lastErr = fmt.Errorf("status %d (%s)", resp.StatusCode, strings.TrimSpace(string(body[:n])))
		if attempt == 1 {
			log.Printf("plex start: attempt 1 failed (%v); retrying after 1.5s", lastErr)
			time.Sleep(1500 * time.Millisecond)
		}
	}
	return fmt.Errorf("plex start (%s): %v", redactedURL(playlistURL), lastErr)
}

// callDecision GETs /transcode/universal/decision and returns nil if
// Plex acknowledges the request (any 2xx). The body isn't read — we
// only care that Plex registered the negotiation. Errors are kept
// because a failed decision predicts a failed start.
func (ps *PlexSession) callDecision(ratingKey, sessionID string, offsetSec float64) error {
	u := ps.decisionURL(ratingKey, sessionID, offsetSec)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := ps.plex.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
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
	// /stop needs the same client identifier as the request that
	// started the session — without it Plex's session manager doesn't
	// recognize who's asking and the stop is a no-op (which is why a
	// subsequent /start.m3u8 then 400s with a stale session held).
	q := url.Values{}
	q.Set("session", ps.sessionID)
	q.Set("X-Plex-Client-Identifier", "plexwatchparty")
	q.Set("X-Plex-Token", ps.plex.Token)
	stopURL := ps.plex.BaseURL + "/video/:/transcode/universal/stop?" + q.Encode()
	req, _ := http.NewRequest(http.MethodGet, stopURL, nil)
	if resp, err := ps.plex.Do(req); err == nil {
		resp.Body.Close()
	}
	ps.ratingKey = ""
	ps.sessionID = ""
	ps.playlistURL = ""
	ps.offsetMs = 0
	ps.edgeMs = 0
	// Plex's transcoder needs a beat to tear down before it'll accept a
	// new session under the same client identifier; without this pause
	// the next /start.m3u8 returns a bare 400 even with valid params.
	time.Sleep(400 * time.Millisecond)
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

// FetchPlaylist GETs the current session's playlist from Plex. If Plex
// returns a master playlist (the usual case for protocol=hls — single
// variant wrapped in a master), follow the variant URI transparently
// and return the media playlist body instead. The returned baseURL is
// the URL the returned body was fetched from — used to resolve any
// relative segment URIs in the body.
func (ps *PlexSession) FetchPlaylist() (body []byte, baseURL string, err error) {
	ps.mu.Lock()
	plUrl := ps.playlistURL
	ps.mu.Unlock()
	if plUrl == "" {
		return nil, "", fmt.Errorf("no active Plex session")
	}
	body, err = ps.fetchPlaylistBody(plUrl)
	if err != nil {
		return nil, "", err
	}
	baseURL = plUrl
	if !bytes.Contains(body, []byte("#EXT-X-STREAM-INF")) {
		return body, baseURL, nil
	}
	variantURL, err := resolveFirstVariantURL(body, plUrl)
	if err != nil {
		return nil, "", fmt.Errorf("resolve variant playlist: %w", err)
	}
	body, err = ps.fetchPlaylistBody(variantURL)
	if err != nil {
		return nil, "", err
	}
	return body, variantURL, nil
}

func (ps *PlexSession) fetchPlaylistBody(u string) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	resp, err := ps.plex.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex playlist: status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// resolveFirstVariantURL scans a master playlist for the first
// #EXT-X-STREAM-INF line and returns its variant URI resolved against
// the master URL (so relative URIs become absolute).
func resolveFirstVariantURL(master []byte, masterURL string) (string, error) {
	base, err := url.Parse(masterURL)
	if err != nil {
		return "", err
	}
	lines := bytes.Split(master, []byte{'\n'})
	streamInf := false
	for _, raw := range lines {
		line := strings.TrimRight(string(raw), "\r")
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			streamInf = true
			continue
		}
		if !streamInf {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ref, err := url.Parse(trimmed)
		if err != nil {
			return "", err
		}
		return base.ResolveReference(ref).String(), nil
	}
	return "", fmt.Errorf("master playlist has no variant URI")
}

// FetchSegment GETs the given Plex segment URL and returns the body as
// a ReadCloser the caller MUST close. The URL already contains the
// X-Plex-Token; Plex's segment endpoint authenticates from that.
func (ps *PlexSession) FetchSegment(segURL string) (io.ReadCloser, error) {
	req, _ := http.NewRequest(http.MethodGet, segURL, nil)
	resp, err := ps.plex.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("plex segment %s: status %d", redactedURL(segURL), resp.StatusCode)
	}
	return resp.Body, nil
}
