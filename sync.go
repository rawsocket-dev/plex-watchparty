package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is the single source of truth for the watch party. Position is only
// authoritative at UpdatedAtMs; while Playing, clients extrapolate from there.
type State struct {
	RatingKey   string  `json:"ratingKey"`
	Title       string  `json:"title"`
	Playing     bool    `json:"playing"`
	PositionSec float64 `json:"positionSec"`
	// DurationSec is the source-of-truth length of the movie, sourced
	// from Plex metadata at load time. We broadcast it so the player
	// can drive its scrub bar without depending on v.duration (which
	// is Infinity with an event-type HLS playlist).
	DurationSec float64 `json:"durationSec"`
	UpdatedAtMs int64   `json:"updatedAtMs"`
	// SessionToken bumps every time the Plex transcode session restarts
	// (load action, or forward-seek-past-edge). Clients compare against
	// the last token they've seen — if changed, they destroy their
	// hls.js instance and attach a fresh one with the new playlist URL.
	SessionToken int64 `json:"sessionToken"`
	// CachedRanges is the union of all currently-cached segment time
	// ranges for the active movie, in seconds. The scrub bar renders
	// these as the "lighter band" so users can see which scrub targets
	// are instant vs which will trigger a Plex restart.
	CachedRanges [][2]float64 `json:"cachedRanges,omitempty"`
	// Viewers is the current connected-viewer list (name + role).
	// Refreshed on every broadcast so the player's "who's here" tooltip
	// can update as people join / leave.
	Viewers []ViewerInfo `json:"viewers,omitempty"`
}

// ViewerInfo is the per-connection identity surfaced to clients in
// the SSE state stream. Name comes from the wp_name cookie set at
// login; Host mirrors the connection's effective role.
type ViewerInfo struct {
	Name string `json:"name"`
	Host bool   `json:"host"`
}

// clientEntry is the per-connection record we keep on Hub.clients.
// The channel carries pre-marshaled JSON so broadcast can serialize
// State once and fan it out byte-identical to every connected viewer
// — at 8 viewers that's ~7 fewer json.Marshal calls per tick.
//
// id is the short random handle the admin panel uses to reference a
// specific connection (kick). ip / connectedAt are stamped at join
// time for the admin roster API. kill is the per-entry abort
// channel; closing it unblocks the HandleEvents select loop and
// terminates that one connection without touching the others.
type clientEntry struct {
	id          string
	host        bool
	name        string
	ip          string
	connectedAt time.Time
	send        chan []byte
	kill        chan struct{}
}

// AdminViewer is the per-connection record surfaced to the admin
// panel. Includes the kick handle + the IP + when the connection
// joined — fields we deliberately don't put on the public ViewerInfo.
// Kbps is each viewer's current rolling-window throughput, joined in
// by IP from bwTracker so the panel can show "who's buffering."
type AdminViewer struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Host         bool   `json:"host"`
	IP           string `json:"ip"`
	ConnectedSec int64  `json:"connectedSec"`
	Kbps         int64  `json:"kbps"`
}

type Hub struct {
	plex    *Plex
	session *PlexSession
	cache   *SegmentCache
	recent  *RecentMovies

	mu    sync.Mutex
	state State
	// clients keyed by the per-connection send channel so disconnect
	// lookups stay O(1). Role tracking pauses the room when the last
	// host leaves; name tracking populates the "who's here" roster.
	clients map[*clientEntry]struct{}

	// broadcastWake fires whenever the broadcast loop should re-tick:
	// first viewer joins, state changes, idle threshold crossed. The
	// loop pauses (blocks on this channel) when there are no clients
	// so an empty room doesn't burn cycles broadcasting to nobody.
	broadcastWake chan struct{}

	// Idle shutdown: when the last SSE viewer disconnects, start a
	// grace timer; if no one rejoins by the time it fires, stop plex session
	// so we don't keep transcoding for nobody.
	idleTimer *time.Timer

	// Host-exit pause: when the last host disconnects mid-playback,
	// start a short grace timer. If no host returns by then, broadcast
	// pause so viewers stop where the host did. Reconnects during the
	// grace window (page refresh, network blip) cancel the pause.
	hostExitTimer *time.Timer
}

// idleGrace is how long we keep the plex session alive after the last viewer
// leaves. Long enough to forgive a tab refresh or short reconnect,
// short enough that a forgotten session doesn't burn CPU all night.
const idleGrace = 60 * time.Second

// hostExitGrace forgives a quick host reconnect (refresh, brief network
// blip). Shorter than idleGrace because the cost of a wrong pause is
// just "host clicks play again," not "plex session killed."
const hostExitGrace = 10 * time.Second

func NewHub(plex *Plex, session *PlexSession, cache *SegmentCache, recent *RecentMovies) *Hub {
	h := &Hub{
		plex:          plex,
		session:       session,
		cache:         cache,
		recent:        recent,
		clients:       make(map[*clientEntry]struct{}),
		broadcastWake: make(chan struct{}, 1),
	}
	// Periodic state re-broadcast. Two goals: (1) Safari's EventSource
	// closes the SSE if it goes ~10–20 s without an actual data event
	// (comment-only heartbeats don't count), and (2) every viewer
	// gets a fresh extrapolated position on a steady cadence so the
	// drift correction in the player has up-to-date state even
	// between play/pause/seek changes.
	go h.broadcastLoop()
	// Plex timeline reporting. Makes us look like a normal Plex
	// client (web app, iOS, etc.) by POSTing our position + state
	// every ~5s. Plex uses these reports to transcode ahead of the
	// playhead and not evict nearby segments — without them we get
	// 404s on segments hls.js races ahead to ask for.
	go h.timelineReportLoop()
	return h
}

func (h *Hub) broadcastLoop() {
	for {
		h.mu.Lock()
		hasClients := len(h.clients) > 0
		h.mu.Unlock()
		if !hasClients {
			// Empty room — block until someone joins. No point
			// re-broadcasting state to nobody every 3s.
			<-h.broadcastWake
			continue
		}
		select {
		case <-time.After(3 * time.Second):
		case <-h.broadcastWake:
		}
		h.mu.Lock()
		// Auto-pause at the end of the movie. Without this, a host
		// who leaves a tab open in 'playing' state past the credits
		// has the server extrapolating forever — overnight idles
		// have produced PositionSec values in the tens of thousands
		// of seconds, then the player can't seek there because it's
		// outside the HLS playlist. Bake the clamp into h.state
		// (not just snapshot()) so subsequent /control actions see
		// the corrected position.
		if h.state.Playing && h.state.DurationSec > 0 {
			extrapolated := h.state.PositionSec + float64(nowMs()-h.state.UpdatedAtMs)/1000.0
			if extrapolated >= h.state.DurationSec {
				log.Printf("auto-pause: %q reached end at %.2fs (was %.2fs, dur %.2fs)",
					h.state.Title, h.state.DurationSec, h.state.PositionSec, h.state.DurationSec)
				h.state.PositionSec = h.state.DurationSec
				h.state.Playing = false
				h.state.UpdatedAtMs = nowMs()
			}
		}
		if len(h.clients) > 0 {
			h.broadcast()
		}
		h.mu.Unlock()
	}
}

// timelineReportInterval matches the cadence Plex's official web /
// mobile clients use. Faster than this is noisy on the Plex side;
// slower invites stale-segment eviction.
const timelineReportInterval = 5 * time.Second

// timelineReportLoop POSTs /:/timeline every timelineReportInterval
// while a Plex session is active. Snapshot extrapolates the position
// so each report carries the actual playhead, not a stale value
// from the last play/pause.
func (h *Hub) timelineReportLoop() {
	t := time.NewTicker(timelineReportInterval)
	defer t.Stop()
	for range t.C {
		h.reportTimelineNow()
	}
}

// reportTimelineNow snapshots state and fires off a single timeline
// report. Called from the periodic ticker AND from state-changing
// /control actions (load, play, pause, seek) so Plex learns about
// changes promptly instead of waiting up to 5 seconds for the next
// tick. Safe to call when no session is active — ReportTimeline
// is a no-op in that case.
func (h *Hub) reportTimelineNow() {
	h.mu.Lock()
	if h.state.RatingKey == "" {
		h.mu.Unlock()
		return
	}
	s := h.snapshot()
	h.mu.Unlock()
	plexState := "paused"
	if s.Playing {
		plexState = "playing"
	}
	if err := h.session.ReportTimeline(
		plexState,
		s.RatingKey,
		int64(s.PositionSec*1000),
		int64(s.DurationSec*1000),
	); err != nil {
		log.Printf("timeline: report failed: %v", err)
	}
}

// wakeBroadcast is a non-blocking nudge to the broadcast loop. Used
// when a client joins (so the loop unblocks from its idle wait) and
// when state changes between ticks. The single-slot buffered channel
// coalesces — if multiple wakes arrive before the loop reads, only
// one fires.
func (h *Hub) wakeBroadcast() {
	select {
	case h.broadcastWake <- struct{}{}:
	default:
	}
}

// Lock ordering — touching this invariant breaks the room.
//
// The three mutexes in play are Hub.mu, PlexSession.mu, and
// SegmentCache.mu. They MUST be acquired in that order if any two
// are held simultaneously:
//
//	Hub.mu → PlexSession.mu (HandleControl seek/restart, idleShutdown)
//	Hub.mu → SegmentCache.mu (broadcast → cache.RangesFor)
//
// Neither PlexSession nor SegmentCache ever calls back into Hub, so
// the reverse direction is impossible by construction. The cache
// methods also never call into PlexSession and vice versa, so those
// two are never held together. If any future refactor introduces a
// cache or session callback that needs hub state, take it OUT of
// the lock and pass values, don't add an acquire.

// hostCount returns the number of currently-connected hosts. Must be
// called with h.mu held.
func (h *Hub) hostCount() int {
	n := 0
	for c := range h.clients {
		if c.host {
			n++
		}
	}
	return n
}

// viewerList returns the current connected-viewer roster sorted host-
// first then alphabetical. Must be called with h.mu held.
func (h *Hub) viewerList() []ViewerInfo {
	out := make([]ViewerInfo, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, ViewerInfo{Name: c.name, Host: c.host})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// onClientCountChange must be called with h.mu held whenever a client
// is added to or removed from h.clients. Manages two timers:
//   - idle shutdown: plex session stops when ALL viewers leave
//   - host-exit pause: room pauses when the last HOST leaves mid-playback
func (h *Hub) onClientCountChange() {
	// --- idle shutdown (any-viewer) ---
	if len(h.clients) > 0 {
		if h.idleTimer != nil {
			h.idleTimer.Stop()
			h.idleTimer = nil
		}
	} else if h.state.RatingKey != "" && h.idleTimer == nil {
		rk := h.state.RatingKey
		h.idleTimer = time.AfterFunc(idleGrace, func() { h.idleShutdown(rk) })
		log.Printf("idle: no viewers, will stop plex session in %s if nobody returns", idleGrace)
	}

	// --- host-exit pause ---
	if h.hostCount() > 0 {
		// At least one host present — cancel any pending pause.
		if h.hostExitTimer != nil {
			h.hostExitTimer.Stop()
			h.hostExitTimer = nil
		}
		return
	}
	// No hosts. Only schedule a pause if a movie is actively playing
	// (no point pausing a paused session) and a pause isn't already
	// scheduled.
	if !h.state.Playing || h.state.RatingKey == "" {
		return
	}
	if h.hostExitTimer != nil {
		return
	}
	rk := h.state.RatingKey
	h.hostExitTimer = time.AfterFunc(hostExitGrace, func() { h.hostExitPause(rk) })
	log.Printf("host-exit: no host present, will pause in %s if none returns", hostExitGrace)
}

// hostExitPause fires when the host-exit grace timer expires. Pauses
// the room so viewers don't keep watching without the host.
func (h *Hub) hostExitPause(forRatingKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hostExitTimer = nil
	if h.hostCount() > 0 {
		return // a host came back
	}
	if h.state.RatingKey != forRatingKey || !h.state.Playing {
		return // session changed or already paused
	}
	cur := h.snapshot() // capture extrapolated position before pausing
	cur.Playing = false
	cur.UpdatedAtMs = nowMs()
	h.state = cur
	log.Printf("host-exit: pausing %q at %.2fs (no host returned in %s)",
		h.state.Title, h.state.PositionSec, hostExitGrace)
	h.broadcast()
}

// idleShutdown fires when the grace timer expires. Guards against the
// race where a viewer rejoins during the grace window.
func (h *Hub) idleShutdown(forRatingKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.idleTimer = nil
	if len(h.clients) > 0 {
		return // someone rejoined
	}
	if h.state.RatingKey != forRatingKey {
		return // session changed during the grace window
	}
	if h.state.RatingKey == "" {
		return // already cleared
	}
	log.Printf("idle: stopping plex session %q after %s with no viewers",
		h.state.Title, idleGrace)
	h.session.Stop()
	h.state = State{UpdatedAtMs: nowMs()}
	h.broadcast()
}

func nowMs() int64 { return time.Now().UnixMilli() }

// nameCookie is the cookie that carries the viewer's chosen display
// name. Separate from sessionCookie so the name can be JS-readable
// without exposing the auth credential.
const nameCookie = "wp_name"

// maxViewerName is the trim length applied to viewer names. Short
// enough to keep the top-bar tooltip readable, long enough for real
// nicknames.
const maxViewerName = 16

// sanitizeName trims, drops anything outside printable ASCII (so
// emoji, control chars, and non-Latin scripts don't sneak in), and
// caps the result at maxViewerName runes. Returns "" if nothing
// usable remains.
func sanitizeName(s string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 0x20 && r <= 0x7e {
			return r
		}
		return -1
	}, s)
	clean = strings.TrimSpace(clean)
	if len(clean) > maxViewerName {
		clean = clean[:maxViewerName]
	}
	return clean
}

// viewerNameFromRequest pulls the display name out of the wp_name
// cookie, percent-decodes it, then runs it through sanitizeName.
// Empty or missing cookies fall back to "guest" so the UI always has
// something to render.
func viewerNameFromRequest(r *http.Request) string {
	c, err := r.Cookie(nameCookie)
	if err != nil {
		return "guest"
	}
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		v = c.Value
	}
	if clean := sanitizeName(v); clean != "" {
		return clean
	}
	return "guest"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fmtDurationMs(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	d := time.Duration(ms) * time.Millisecond
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func (h *Hub) snapshot() State {
	s := h.state
	if s.Playing {
		s.PositionSec += float64(nowMs()-s.UpdatedAtMs) / 1000.0
		s.UpdatedAtMs = nowMs()
		// Clamp at the end of the movie. Without this, a host who
		// leaves a tab open in 'playing' state past the credits keeps
		// the server extrapolating forever — we've seen positions in
		// the tens of thousands of seconds after an overnight idle.
		// Subsequent /control actions then record that nonsense and
		// the player can't seek to a position outside the playlist.
		if s.DurationSec > 0 && s.PositionSec > s.DurationSec {
			s.PositionSec = s.DurationSec
		}
	}
	return s
}

// RestartAtCurrentPosition restarts the active Plex session at the
// host's current play position. Called by the segment proxy's auto-
// restart goroutine (after N consecutive Plex 4xx/5xx fetches) and
// by the admin "Restart Plex session" button. Reuses the same
// Hub.mu → release → session.RestartFor → Hub.mu dance as the seek-
// with-restart path; bumps SessionToken so connected clients destroy
// their hls.js and reattach with the freshly-rewritten playlist.
//
// reason classifies the call site for the admin lifecycle counters.
func (h *Hub) RestartAtCurrentPosition(reason RestartReason) error {
	h.mu.Lock()
	if h.state.RatingKey == "" {
		h.mu.Unlock()
		return fmt.Errorf("no active session")
	}
	cur := h.snapshot()
	h.mu.Unlock()
	log.Printf("restart: %v — restarting Plex at %.2fs", reason, cur.PositionSec)
	if err := h.session.RestartFor(reason, cur.PositionSec); err != nil {
		return fmt.Errorf("plex Restart: %w", err)
	}
	h.mu.Lock()
	h.state.SessionToken = h.session.SessionToken()
	h.state.UpdatedAtMs = nowMs()
	h.broadcast()
	h.mu.Unlock()
	go h.reportTimelineNow()
	return nil
}

// RecoverSegmentForRange asks the session to substitute a segment
// covering the requested movie-time window after Plex 404'd the
// original. Internally the session may restart Plex at that offset
// (bumping SessionToken) — on success we stamp the new token onto
// h.state and broadcast so connected clients reattach with the
// fresh playlist for SUBSEQUENT segments. The bytes returned here
// satisfy the in-flight segment request without the client ever
// seeing the failure.
func (h *Hub) RecoverSegmentForRange(startMs, endMs int64) ([]byte, error) {
	data, err := h.session.RecoverSegmentBytes(startMs, endMs)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.state.SessionToken = h.session.SessionToken()
	h.state.UpdatedAtMs = nowMs()
	h.broadcast()
	h.mu.Unlock()
	go h.reportTimelineNow()
	return data, nil
}

// Snapshot is the locked, public counterpart of snapshot(). Used by
// HTTP handlers that need a one-shot view of current state (e.g. the
// /api/state endpoint that drives the library's "Resume?" prompt).
// Includes the current viewer roster so callers don't see a stale
// list between broadcast ticks.
func (h *Hub) Snapshot() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.snapshot()
	s.Viewers = h.viewerList()
	return s
}

// broadcast marshals the current snapshot once and fans the bytes out
// to every connected viewer. Stays out of h.state (CachedRanges and
// Viewers are computed per-broadcast and stamped onto the snapshot
// only — the authoritative h.state is unchanged).
func (h *Hub) broadcast() {
	s := h.snapshot()
	if s.RatingKey != "" && h.cache != nil {
		s.CachedRanges = h.cache.RangesFor(s.RatingKey)
	}
	s.Viewers = h.viewerList()
	b, err := json.Marshal(s)
	if err != nil {
		log.Printf("broadcast: marshal: %v", err)
		return
	}
	for c := range h.clients {
		select {
		case c.send <- b:
		default: // drop for slow clients; next event re-syncs them
		}
	}
}

// HandleEvents is the SSE stream: initial state on connect, then every change,
// plus a heartbeat so proxies don't kill idle connections.
// isHost is the connection's effective role — needed so the hub can pause
// the room when the last host leaves.
func (h *Hub) HandleEvents(w http.ResponseWriter, r *http.Request, isHost bool) {
	name := viewerNameFromRequest(r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default and that breaks SSE
	// (events get held back instead of flushed event-by-event). This
	// header tells nginx + GCP HTTPS LB to bypass response buffering
	// for this stream without requiring per-location config on the
	// proxy side. Other proxies (Caddy, Traefik) ignore it harmlessly.
	w.Header().Set("X-Accel-Buffering", "no")

	ip := clientIP(r)
	connectedAt := time.Now()
	entry := &clientEntry{
		id:          randomHex(8),
		host:        isHost,
		name:        name,
		ip:          ip,
		connectedAt: connectedAt,
		send:        make(chan []byte, 8),
		kill:        make(chan struct{}),
	}
	h.mu.Lock()
	wasEmpty := len(h.clients) == 0
	h.clients[entry] = struct{}{}
	n := len(h.clients)
	hosts := h.hostCount()
	h.onClientCountChange()
	if wasEmpty {
		// First viewer in an idle room — unblock the broadcast loop.
		h.wakeBroadcast()
	}
	init := h.snapshot()
	if init.RatingKey != "" && h.cache != nil {
		init.CachedRanges = h.cache.RangesFor(init.RatingKey)
	}
	// snapshot() doesn't refresh Viewers — that only happens in
	// broadcast(). For a freshly-connected client this matters:
	// without this, the new viewer would see a stale (or empty)
	// roster on join and have to wait up to 3 s for the next
	// broadcastLoop tick. Stamp the current roster onto the init.
	init.Viewers = h.viewerList()
	initBytes, _ := json.Marshal(init)
	h.mu.Unlock()
	role := "viewer"
	if isHost {
		role = "host"
	}
	log.Printf("sse: connect ip=%s role=%s viewers=%d hosts=%d", ip, role, n, hosts)
	defer func() {
		h.mu.Lock()
		delete(h.clients, entry)
		left := len(h.clients)
		leftHosts := h.hostCount()
		h.onClientCountChange()
		h.mu.Unlock()
		log.Printf("sse: disconnect ip=%s role=%s viewers=%d hosts=%d after=%s",
			ip, role, left, leftHosts, time.Since(connectedAt).Round(time.Second))
	}()

	writeBytes := func(b []byte) {
		w.Write([]byte("data: "))
		w.Write(b)
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}
	writeBytes(initBytes)

	// 5 s heartbeat: also doubles as our "is the client still there?"
	// probe. The write fails on a closed TCP connection, which is how
	// Go's HTTP server cancels r.Context() and lets the defer GC the
	// dead client. A long heartbeat = long ghost-viewer window.
	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-entry.kill:
			log.Printf("sse: kicked ip=%s role=%s after=%s",
				ip, role, time.Since(connectedAt).Round(time.Second))
			return
		case b := <-entry.send:
			writeBytes(b)
		case <-heartbeat.C:
			w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

type controlReq struct {
	Action      string  `json:"action"` // load | play | pause | seek
	RatingKey   string  `json:"ratingKey"`
	PositionSec float64 `json:"positionSec"`
	// Restart, when set on a 'load', forces a fresh Plex Start even
	// when the requested movie is already the active session. Used by
	// the library's "Start over" prompt to discard the current
	// position and begin from offset 0.
	Restart bool `json:"restart"`
}

// HandleControl applies an action from any authenticated friend and rebroadcasts.
func (h *Hub) HandleControl(w http.ResponseWriter, r *http.Request) {
	// Cap the request body — /control is the host's command channel,
	// not a file upload. 4 KiB is generous for the JSON we accept.
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req controlReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("control: bad request ip=%s err=%v", clientIP(r), err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	log.Printf("control: %s ip=%s action=%s ratingKey=%s pos=%.2f",
		"received", clientIP(r), req.Action, req.RatingKey, req.PositionSec)

	if req.Action == "load" {
		t0 := time.Now()
		// If the same movie is already loaded and Plex still has a live
		// session for it, skip the Stop+Start dance entirely. Plex's
		// universal-transcoder /stop is unreliable: it often returns
		// 200 without actually freeing the slot, and the next /start
		// then 400s with a bare HTML body. Reusing the existing session
		// avoids that whole class of failure when the user clicks the
		// same movie twice (back + forward, refresh, etc.). The
		// library prompts "Resume / Start over" for this case; Start
		// over sends restart=true to force the full path.
		if h.session.RatingKey() == req.RatingKey && !req.Restart {
			log.Printf("load %q: same movie already loaded, reusing session", req.RatingKey)
			h.mu.Lock()
			cur := h.state
			h.broadcast()
			h.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"reused":      true,
				"durationSec": cur.DurationSec,
			})
			return
		}

		si, err := h.plex.Resolve(req.RatingKey)
		if err != nil {
			log.Printf("control: resolve failed ip=%s ratingKey=%s err=%v",
				clientIP(r), req.RatingKey, err)
			http.Error(w, "resolve: "+err.Error(), http.StatusBadGateway)
			return
		}
		resolveMs := time.Since(t0).Milliseconds()

		tList := time.Now()
		// Ensure the library cache is warm so MovieByKey can resolve
		// the title without a Plex round-trip. ListMovies is cheap on
		// a warm cache; on cold start it populates the index for us.
		_, _ = h.plex.ListMovies()
		title := req.RatingKey
		year := 0
		if m, ok := h.plex.MovieByKey(req.RatingKey); ok {
			title = m.Title
			year = m.Year
		}
		listMs := time.Since(tList).Milliseconds()

		tStart := time.Now()
		// A host-initiated load wins over any in-flight auto-restart —
		// signal the auto-restart goroutine to abort before we call
		// Start. Without this both could call /stop+/start in quick
		// succession and Plex would see overlapping sessions.
		h.session.SuppressAutoRestart()
		if err := h.session.Start(req.RatingKey, 0); err != nil {
			log.Printf("control: plex Start failed ip=%s ratingKey=%s err=%v",
				clientIP(r), req.RatingKey, err)
			http.Error(w, "plex start: "+err.Error(), http.StatusBadGateway)
			return
		}
		startMs := time.Since(tStart).Milliseconds()
		totalMs := time.Since(t0).Milliseconds()

		log.Printf("load %q: resolve=%dms list=%dms plexStart=%dms total=%dms",
			title, resolveMs, listMs, startMs, totalMs)
		log.Printf("media %q: %s %s · %dx%d @ %s · %d kbps · %s",
			title, orDash(si.VideoCodec), orDash(si.VideoProfile),
			si.Width, si.Height, orDash(si.FrameRate), si.Bitrate,
			fmtDurationMs(si.Duration),
		)

		h.mu.Lock()
		h.state = State{
			RatingKey:    req.RatingKey,
			Title:        title,
			Playing:      false,
			PositionSec:  0,
			DurationSec:  float64(si.Duration) / 1000.0,
			SessionToken: h.session.SessionToken(),
			UpdatedAtMs:  nowMs(),
		}
		h.broadcast()
		h.mu.Unlock()
		// Tell Plex about the new session immediately so it knows our
		// starting position. The 5s ticker would catch it eventually
		// but the first segments hls.js requests would race ahead of
		// Plex's transcoder otherwise.
		go h.reportTimelineNow()
		if h.recent != nil {
			h.recent.Touch(req.RatingKey, title, year)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int64{
			"plexResolveMs": resolveMs,
			"plexListMs":    listMs,
			"plexStartMs":   startMs,
			"totalMs":       totalMs,
		})
		return
	}

	if req.Action == "stop" {
		// Host requested an explicit end of the watch session. Tear
		// down the Plex transcoder, clear our state, and broadcast
		// the empty State so every connected client navigates to the
		// waiting room or the library. Used by the player's "←
		// library" link so navigating away kills playback instead of
		// leaving the transcoder running until the idle timer fires.
		log.Printf("control: stop ip=%s title=%q", clientIP(r), h.state.Title)
		// Send a final "stopped" timeline before tearing down the
		// session — after session.Stop() the sessionID is cleared and
		// ReportTimeline becomes a no-op.
		h.mu.Lock()
		rk := h.state.RatingKey
		posMs := int64(h.snapshot().PositionSec * 1000)
		durMs := int64(h.state.DurationSec * 1000)
		h.mu.Unlock()
		if rk != "" {
			if err := h.session.ReportTimeline("stopped", rk, posMs, durMs); err != nil {
				log.Printf("timeline: stop report failed: %v", err)
			}
		}
		h.session.Stop()
		h.mu.Lock()
		h.state = State{UpdatedAtMs: nowMs()}
		h.broadcast()
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if req.Action != "play" && req.Action != "pause" && req.Action != "seek" {
		log.Printf("control: unknown action ip=%s action=%q", clientIP(r), req.Action)
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.snapshot()
	switch req.Action {
	case "play":
		cur.Playing = true
		log.Printf("state: play  ip=%s title=%q at=%.2f", clientIP(r), cur.Title, cur.PositionSec)
	case "pause":
		cur.Playing = false
		log.Printf("state: pause ip=%s title=%q at=%.2f", clientIP(r), cur.Title, cur.PositionSec)
	case "seek":
		log.Printf("state: seek  ip=%s title=%q from=%.2f to=%.2f",
			clientIP(r), cur.Title, cur.PositionSec, req.PositionSec)
		// Decide whether the seek target is reachable from cache alone
		// (no Plex restart) or whether we need to restart Plex at the
		// new offset. Cached ranges + the current session's edge define
		// what's instantly seekable.
		target := req.PositionSec
		needRestart := target > h.session.EdgeSec()+0.5
		if needRestart {
			// Also check cache — backward seeks into a previously-watched
			// range (perhaps before a prior Restart) don't need a restart.
			for _, rng := range h.cache.RangesFor(cur.RatingKey) {
				if target >= rng[0] && target <= rng[1] {
					needRestart = false
					break
				}
			}
		}
		if needRestart {
			// We must drop h.mu before calling session.Restart — it
			// holds PlexSession.mu internally for the duration of the
			// HTTP round-trip to Plex (decision + start, ~hundreds of
			// ms). Holding Hub.mu through that would stall every
			// connected viewer's SSE writes. Re-snapshot state after
			// reacquiring since other handlers (play/pause echoes)
			// can have mutated h.state during the gap; the seek
			// target overrides whatever PositionSec they wrote, but
			// the rest of the state must be current.
			h.mu.Unlock()
			log.Printf("seek: restart needed (target=%.2f > edge=%.2f, no cache hit)",
				target, h.session.EdgeSec())
			// Host action wins over any in-flight auto-restart.
			h.session.SuppressAutoRestart()
			if err := h.session.RestartFor(RestartBySeek, target); err != nil {
				log.Printf("seek: plex Restart failed: %v", err)
				h.mu.Lock()
				http.Error(w, "plex restart: "+err.Error(), http.StatusBadGateway)
				return
			}
			h.mu.Lock()
			cur = h.snapshot()
			cur.SessionToken = h.session.SessionToken()
		}
		cur.PositionSec = target
	}
	cur.UpdatedAtMs = nowMs()
	h.state = cur
	h.broadcast()
	// Promptly tell Plex about play/pause/seek so it can adjust its
	// transcode lookahead and segment retention without waiting for
	// the next 5s tick.
	go h.reportTimelineNow()
	w.WriteHeader(http.StatusNoContent)
}

// SendEveryoneToLobby tears down the active Plex session and clears
// hub state. Every connected /watch page sees the empty State on the
// next SSE tick and reloads to land on waiting.html. Same code path
// as a host clicking "← library", just admin-triggered.
func (h *Hub) SendEveryoneToLobby() {
	h.mu.Lock()
	rk := h.state.RatingKey
	posMs := int64(h.snapshot().PositionSec * 1000)
	durMs := int64(h.state.DurationSec * 1000)
	title := h.state.Title
	h.mu.Unlock()
	if rk != "" {
		if err := h.session.ReportTimeline("stopped", rk, posMs, durMs); err != nil {
			log.Printf("timeline: stop report failed: %v", err)
		}
	}
	h.session.Stop()
	h.mu.Lock()
	h.state = State{UpdatedAtMs: nowMs()}
	h.broadcast()
	h.mu.Unlock()
	log.Printf("admin lobby: tore down session %q", title)
}

// AdminRoster returns a snapshot of currently connected SSE clients
// with the fields the admin panel needs (id for kick, ip, name, role,
// connected-since seconds, current kbps). Sorted host-first then by
// IP for a stable display order. bw is used to join in each viewer's
// rolling-window throughput; pass nil if bandwidth is unavailable.
func (h *Hub) AdminRoster(bw *bwTracker) []AdminViewer {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	out := make([]AdminViewer, 0, len(h.clients))
	for c := range h.clients {
		var kbps int64
		if bw != nil {
			kbps = bw.KbpsForIP(c.ip)
		}
		out = append(out, AdminViewer{
			ID:           c.id,
			Name:         c.name,
			Host:         c.host,
			IP:           c.ip,
			ConnectedSec: int64(now.Sub(c.connectedAt).Seconds()),
			Kbps:         kbps,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host
		}
		return out[i].IP < out[j].IP
	})
	return out
}

// KickClient closes the kill channel of the matching connection so its
// HandleEvents writer-loop returns. Returns true if a connection was
// found and signalled. The viewer's browser will reconnect almost
// immediately via EventSource auto-retry — kick is "drop this socket,"
// not "ban this user."
func (h *Hub) KickClient(id string) bool {
	h.mu.Lock()
	var target *clientEntry
	for c := range h.clients {
		if c.id == id {
			target = c
			break
		}
	}
	h.mu.Unlock()
	if target == nil {
		return false
	}
	select {
	case <-target.kill:
		// already closed
	default:
		close(target.kill)
	}
	return true
}
