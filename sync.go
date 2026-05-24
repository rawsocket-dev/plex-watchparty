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
	UpdatedAtMs int64   `json:"updatedAtMs"`
}

type Hub struct {
	plex *Plex
	rx   *Remuxer

	mu      sync.Mutex
	state   State
	clients map[chan State]struct{}
}

func NewHub(plex *Plex, rx *Remuxer) *Hub {
	return &Hub{plex: plex, rx: rx, clients: make(map[chan State]struct{})}
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
func (h *Hub) HandleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan State, 8)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	init := h.snapshot()
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}()

	write := func(s State) {
		b, _ := json.Marshal(s)
		w.Write([]byte("data: "))
		w.Write(b)
		w.Write([]byte("\n\n"))
		flusher.Flush()
	}
	write(init)

	heartbeat := time.NewTicker(15 * time.Second)
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
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.Action == "load" {
		t0 := time.Now()
		si, err := h.plex.Resolve(req.RatingKey)
		if err != nil {
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
			RatingKey: req.RatingKey, Title: title,
			Playing: false, PositionSec: 0, UpdatedAtMs: nowMs(),
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
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.snapshot()
	switch req.Action {
	case "play":
		cur.Playing = true
	case "pause":
		cur.Playing = false
	case "seek":
		cur.PositionSec = req.PositionSec
	}
	cur.UpdatedAtMs = nowMs()
	h.state = cur
	h.broadcast()
	w.WriteHeader(http.StatusNoContent)
}
