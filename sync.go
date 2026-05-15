package main

import (
	"encoding/json"
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
		si, err := h.plex.Resolve(req.RatingKey)
		if err != nil {
			http.Error(w, "resolve: "+err.Error(), http.StatusBadGateway)
			return
		}
		movies, _ := h.plex.ListMovies()
		title := req.RatingKey
		for _, m := range movies {
			if m.RatingKey == req.RatingKey {
				title = m.Title
				break
			}
		}
		if err := h.rx.Start(req.RatingKey, si); err != nil {
			http.Error(w, "remux: "+err.Error(), http.StatusBadGateway)
			return
		}
		h.mu.Lock()
		h.state = State{
			RatingKey: req.RatingKey, Title: title,
			Playing: false, PositionSec: 0, UpdatedAtMs: nowMs(),
		}
		h.broadcast()
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
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
