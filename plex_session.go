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

	// Auto-restart bookkeeping. failMu is independent of mu so the
	// segment proxy can record failures while a Restart is mid-
	// flight. consecutiveSegFails increments on Plex 4xx/5xx
	// segment fetches; once it crosses segFailureThreshold the
	// caller is told to kick off an auto-restart at the current
	// play position. lastAutoRestartAt provides a cooldown window
	// so the stale-URL fetches that follow a successful restart
	// don't immediately re-trigger another spin.
	failMu              sync.Mutex
	consecutiveSegFails int
	autoRestartActive   bool
	lastAutoRestartAt   time.Time
}

const segFailureThreshold = 3
const autoRestartCooldown = 10 * time.Second

// plexClientID identifies this app to Plex's session manager. The same
// string is sent as X-Plex-Client-Identifier on every request — Plex
// uses it to group /decision, /start.m3u8, and /stop into a single
// transcode session. Changing this would orphan any in-flight session.
const plexClientID = "plexwatchparty"

func NewPlexSession(p *Plex, transcodeKbps int) *PlexSession {
	return &PlexSession{plex: p, transcodeKbps: transcodeKbps}
}

// newSessionID mints a unique Plex transcode session id.
func newSessionID() string {
	return "watchparty-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// transcodeParams builds the shared query string used by both
// /decision and /start.m3u8. Plex expects identical params across the
// two calls — only the path differs. Returns url.Values so callers
// can mutate (e.g. encode + attach to a different base path).
func (ps *PlexSession) transcodeParams(ratingKey, sessionID string, offsetSec float64) url.Values {
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
	q.Set("X-Plex-Product", plexClientID)
	q.Set("X-Plex-Version", "1.0")
	q.Set("X-Plex-Client-Identifier", plexClientID)
	q.Set("X-Plex-Platform", "Generic")
	q.Set("X-Plex-Device", plexClientID)
	q.Set("X-Plex-Token", ps.plex.Token)
	return q
}

// transcodeURL is the /start.m3u8 form — the URL the playlist proxy fetches.
func (ps *PlexSession) transcodeURL(ratingKey, sessionID string, offsetSec float64) string {
	return ps.plex.BaseURL + "/video/:/transcode/universal/start.m3u8?" +
		ps.transcodeParams(ratingKey, sessionID, offsetSec).Encode()
}

// decisionURL is the /decision form — preflight handshake that primes
// Plex's session bookkeeping for the upcoming /start.m3u8. Plex
// expects the negotiation + actual start to be stitched together by
// session ID, so both calls share the same params.
func (ps *PlexSession) decisionURL(ratingKey, sessionID string, offsetSec float64) string {
	return ps.plex.BaseURL + "/video/:/transcode/universal/decision?" +
		ps.transcodeParams(ratingKey, sessionID, offsetSec).Encode()
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
	hadSession := ps.sessionID != ""
	ps.stopLocked()
	ps.mu.Unlock()
	// If a prior session was just stopped, give Plex's transcoder a
	// beat to tear down before we ask it to spin up a new one under
	// the same client identifier. Done OUTSIDE the lock so readers
	// (RatingKey / SessionToken / PlaylistURL) aren't stalled. When
	// there was no prior session, skip the cooldown entirely.
	if hadSession {
		time.Sleep(400 * time.Millisecond)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()

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
	ps.stopLocked()
	ps.mu.Unlock()
	// Plex's transcoder needs a beat to tear down before it'll accept
	// a new session under the same client identifier; without this
	// pause the next /start.m3u8 returns a bare 400 even with valid
	// params. Done OUTSIDE the lock so reader methods (RatingKey,
	// SessionToken, PlaylistURL) aren't stalled — the session-cleared
	// invariant is already true the moment stopLocked returns.
	time.Sleep(400 * time.Millisecond)
}

// stopLocked clears in-memory session state and asynchronously notifies
// Plex to terminate the transcode session. Must be called with ps.mu
// held. Does NOT sleep — that's left to the caller so the cooldown
// can happen with the lock released.
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
	q.Set("X-Plex-Client-Identifier", plexClientID)
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
}

// Restart stops the current Plex session and starts a new one at the
// given offset. Used when the host seeks forward into untranscoded
// territory. Atomic under ps.mu: concurrent restarts serialize.
func (ps *PlexSession) Restart(offsetSec float64) error {
	ps.mu.Lock()
	ratingKey := ps.ratingKey
	if ratingKey == "" {
		ps.mu.Unlock()
		return fmt.Errorf("Restart with no active session")
	}
	ps.stopLocked()
	ps.mu.Unlock()
	// Same teardown cooldown as Stop() — outside the lock so reader
	// methods aren't blocked while Plex catches up.
	time.Sleep(400 * time.Millisecond)
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

// RecordSegmentFailure increments the consecutive-segment-failure
// counter. Returns true if the caller should kick off an auto-
// restart (counter just crossed segFailureThreshold AND no restart
// is already in flight AND we're past the cooldown from the
// previous one). The cooldown handles stale-URL fetches from hls.js
// that linger briefly after the playlist re-attach.
func (ps *PlexSession) RecordSegmentFailure() bool {
	ps.failMu.Lock()
	defer ps.failMu.Unlock()
	if ps.autoRestartActive {
		return false
	}
	if time.Since(ps.lastAutoRestartAt) < autoRestartCooldown {
		return false
	}
	ps.consecutiveSegFails++
	if ps.consecutiveSegFails < segFailureThreshold {
		return false
	}
	ps.autoRestartActive = true
	return true
}

// AutoRestartShouldProceed is checked by the auto-restart goroutine
// just before it calls Restart. Returns false if a host-initiated
// restart (seek-with-restart, manual reload) raced in via
// SuppressAutoRestart and grabbed the slot first — in that case the
// auto-restart aborts and lets the host's intent win.
func (ps *PlexSession) AutoRestartShouldProceed() bool {
	ps.failMu.Lock()
	defer ps.failMu.Unlock()
	return ps.autoRestartActive
}

// SuppressAutoRestart is called by HandleControl seek-with-restart
// (and any other code path that's about to call session.Restart on
// purpose) so a racing auto-restart goroutine sees autoRestartActive
// flip back to false and aborts before issuing a second /start.
// Also stamps the cooldown so the post-restart re-attach window
// doesn't immediately re-trigger.
func (ps *PlexSession) SuppressAutoRestart() {
	ps.failMu.Lock()
	defer ps.failMu.Unlock()
	ps.autoRestartActive = false
	ps.consecutiveSegFails = 0
	ps.lastAutoRestartAt = time.Now()
}

// RecordSegmentSuccess resets the consecutive-failure counter — any
// successful segment serve (Plex, cache hit, or cache fallback)
// means we're not stuck.
func (ps *PlexSession) RecordSegmentSuccess() {
	ps.failMu.Lock()
	defer ps.failMu.Unlock()
	ps.consecutiveSegFails = 0
}

// ClearAutoRestart is called when the auto-restart goroutine
// finishes (whether the Restart succeeded or failed). Stamps the
// cooldown start NOW so the post-restart re-attach window doesn't
// immediately re-trigger.
func (ps *PlexSession) ClearAutoRestart() {
	ps.failMu.Lock()
	defer ps.failMu.Unlock()
	ps.autoRestartActive = false
	ps.consecutiveSegFails = 0
	ps.lastAutoRestartAt = time.Now()
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
//
// Retries once on a non-2xx status after a 500 ms pause. Plex's
// transcoder produces segments on-the-fly; hls.js can race ahead of
// production and pull a segment number that doesn't exist *yet* — a
// fresh GET a half-second later typically succeeds. Without the
// retry, hls.js sees our 502 and has to recover via its own gap
// handling, which is jankier and sometimes drops audio sync.
func (ps *PlexSession) FetchSegment(segURL string) (io.ReadCloser, error) {
	var lastStatus int
	for attempt := 1; attempt <= 2; attempt++ {
		req, _ := http.NewRequest(http.MethodGet, segURL, nil)
		resp, err := ps.plex.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			if attempt > 1 {
				log.Printf("seg: recovered on attempt %d (%s)", attempt, redactedURL(segURL))
			}
			return resp.Body, nil
		}
		resp.Body.Close()
		lastStatus = resp.StatusCode
		if attempt == 1 {
			log.Printf("seg: attempt 1 returned %d (%s); retrying in 500ms",
				lastStatus, redactedURL(segURL))
			time.Sleep(500 * time.Millisecond)
		}
	}
	return nil, fmt.Errorf("plex segment %s: status %d (after retry)",
		redactedURL(segURL), lastStatus)
}
