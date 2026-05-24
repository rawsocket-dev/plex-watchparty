package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
}

type Hub struct {
	plex    *Plex
	session *PlexSession
	cache   *SegmentCache

	mu sync.Mutex
	state   State
	// clients map[chan]→isHost. Tracking role per-connection so we can
	// pause the room when the last host leaves (viewers should never
	// see the movie keep playing without the host present).
	clients map[chan State]bool

	// Idle shutdown: when the last SSE viewer disconnects, start a
	// grace timer; if no one rejoins by the time it fires, stop ffmpeg
	// so we don't keep transcoding for nobody.
	idleTimer *time.Timer

	// Host-exit pause: when the last host disconnects mid-playback,
	// start a short grace timer. If no host returns by then, broadcast
	// pause so viewers stop where the host did. Reconnects during the
	// grace window (page refresh, network blip) cancel the pause.
	hostExitTimer *time.Timer
}

// idleGrace is how long we keep ffmpeg alive after the last viewer
// leaves. Long enough to forgive a tab refresh or short reconnect,
// short enough that a forgotten session doesn't burn CPU all night.
const idleGrace = 60 * time.Second

// hostExitGrace forgives a quick host reconnect (refresh, brief network
// blip). Shorter than idleGrace because the cost of a wrong pause is
// just "host clicks play again," not "ffmpeg session killed."
const hostExitGrace = 10 * time.Second

func NewHub(plex *Plex, session *PlexSession, cache *SegmentCache) *Hub {
	h := &Hub{plex: plex, session: session, cache: cache, clients: make(map[chan State]bool)}
	// Periodic state re-broadcast. Two goals: (1) Safari's EventSource
	// closes the SSE if it goes ~10–20 s without an actual data event
	// (comment-only heartbeats don't count), and (2) every viewer
	// gets a fresh extrapolated position on a steady cadence so the
	// drift correction in the player has up-to-date state even
	// between play/pause/seek changes.
	go h.broadcastLoop()
	return h
}

func (h *Hub) broadcastLoop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for range t.C {
		h.mu.Lock()
		if len(h.clients) > 0 {
			h.broadcast()
		}
		h.mu.Unlock()
	}
}

// hostCount returns the number of currently-connected hosts. Must be
// called with h.mu held.
func (h *Hub) hostCount() int {
	n := 0
	for _, isHost := range h.clients {
		if isHost {
			n++
		}
	}
	return n
}

// onClientCountChange must be called with h.mu held whenever a client
// is added to or removed from h.clients. Manages two timers:
//   - idle shutdown: ffmpeg stops when ALL viewers leave
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
		log.Printf("idle: no viewers, will stop ffmpeg in %s if nobody returns", idleGrace)
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
	log.Printf("idle: stopping ffmpeg session %q after %s with no viewers",
		h.state.Title, idleGrace)
	h.rx.Stop()
	h.state = State{UpdatedAtMs: nowMs()}
	h.broadcast()
}

func nowMs() int64 { return time.Now().UnixMilli() }

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func containerSuffix(c string) string {
	if c == "" {
		return ""
	}
	return " (" + c + ")"
}

func audioProfileSuffix(p string) string {
	if p == "" {
		return ""
	}
	return " " + p
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
	}
	return s
}

func (h *Hub) broadcast() {
	s := h.snapshot()
	for ch := range h.clients {
		select {
		case ch <- s:
		default: // drop for slow clients; next event re-syncs them
		}
	}
}

// HandleEvents is the SSE stream: initial state on connect, then every change,
// plus a heartbeat so proxies don't kill idle connections.
// isHost is the connection's effective role — needed so the hub can pause
// the room when the last host leaves.
func (h *Hub) HandleEvents(w http.ResponseWriter, r *http.Request, isHost bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan State, 8)
	ip := clientIP(r)
	connectedAt := time.Now()
	h.mu.Lock()
	h.clients[ch] = isHost
	n := len(h.clients)
	hosts := h.hostCount()
	h.onClientCountChange()
	init := h.snapshot()
	h.mu.Unlock()
	role := "viewer"
	if isHost {
		role = "host"
	}
	log.Printf("sse: connect ip=%s role=%s viewers=%d hosts=%d", ip, role, n, hosts)
	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		left := len(h.clients)
		leftHosts := h.hostCount()
		h.onClientCountChange()
		h.mu.Unlock()
		log.Printf("sse: disconnect ip=%s role=%s viewers=%d hosts=%d after=%s",
			ip, role, left, leftHosts, time.Since(connectedAt).Round(time.Second))
	}()

	write := func(s State) {
		b, _ := json.Marshal(s)
		w.Write([]byte("data: "))
		w.Write(b)
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}
	write(init)

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
		case s := <-ch:
			write(s)
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
}

// HandleControl applies an action from any authenticated friend and rebroadcasts.
func (h *Hub) HandleControl(w http.ResponseWriter, r *http.Request) {
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
		si, err := h.plex.Resolve(req.RatingKey)
		if err != nil {
			log.Printf("control: resolve failed ip=%s ratingKey=%s err=%v",
				clientIP(r), req.RatingKey, err)
			http.Error(w, "resolve: "+err.Error(), http.StatusBadGateway)
			return
		}
		resolveMs := time.Since(t0).Milliseconds()

		tList := time.Now()
		movies, _ := h.plex.ListMovies()
		title := req.RatingKey
		for _, m := range movies {
			if m.RatingKey == req.RatingKey {
				title = m.Title
				break
			}
		}
		listMs := time.Since(tList).Milliseconds()

		tRemux := time.Now()
		if err := h.rx.Start(req.RatingKey, si); err != nil {
			http.Error(w, "remux: "+err.Error(), http.StatusBadGateway)
			return
		}
		remuxMs := time.Since(tRemux).Milliseconds()
		totalMs := time.Since(t0).Milliseconds()

		log.Printf("load %q: resolve=%dms list=%dms remux=%dms total=%dms",
			title, resolveMs, listMs, remuxMs, totalMs)
		log.Printf("media %q: %s %s%s · %dx%d @ %s · %s%dch%s · %d kbps total · %s · %s",
			title,
			orDash(si.VideoCodec),
			orDash(si.VideoProfile),
			containerSuffix(si.Container),
			si.Width, si.Height, orDash(si.FrameRate),
			orDash(si.AudioCodec),
			si.AudioChannels,
			audioProfileSuffix(si.AudioProfile),
			si.Bitrate,
			humanBytes(si.Size),
			fmtDurationMs(si.Duration),
		)

		h.mu.Lock()
		h.state = State{
			RatingKey:   req.RatingKey,
			Title:       title,
			Playing:     false,
			PositionSec: 0,
			DurationSec: float64(si.Duration) / 1000.0,
			UpdatedAtMs: nowMs(),
		}
		h.broadcast()
		h.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int64{
			"plexResolveMs": resolveMs,
			"plexListMs":    listMs,
			"remuxStartMs":  remuxMs,
			"totalMs":       totalMs,
		})
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
		cur.PositionSec = req.PositionSec
	}
	cur.UpdatedAtMs = nowMs()
	h.state = cur
	h.broadcast()
	w.WriteHeader(http.StatusNoContent)
}
