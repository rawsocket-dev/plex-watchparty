package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// RecentMovie is one entry in the recently-played list shown on the
// waiting room. Captured at /control load time, just enough metadata
// to render a card and round-trip a re-load.
type RecentMovie struct {
	RatingKey    string `json:"ratingKey"`
	Title        string `json:"title"`
	Year         int    `json:"year"`
	LastPlayedAt int64  `json:"lastPlayedAt"` // unix seconds
}

// RecentMovies is a tiny LRU-ish list of recently-played movies,
// persisted to disk so it survives container restarts. The store is
// bounded by recentCap so the JSON file never grows unbounded.
type RecentMovies struct {
	path string
	cap  int

	mu      sync.Mutex
	entries []RecentMovie
}

// recentCap is how many entries we keep. Five fits the waiting room
// layout without scrolling and matches the user's ask.
const recentCap = 5

func NewRecentMovies(path string) *RecentMovies {
	return &RecentMovies{path: path, cap: recentCap}
}

// Load reads the persisted list from disk. Missing file is fine —
// the store just starts empty.
func (r *RecentMovies) Load() {
	b, err := os.ReadFile(r.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("recent: load %s: %v", r.path, err)
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := json.Unmarshal(b, &r.entries); err != nil {
		log.Printf("recent: parse %s: %v (starting fresh)", r.path, err)
		r.entries = nil
		return
	}
	if len(r.entries) > r.cap {
		r.entries = r.entries[:r.cap]
	}
}

// Touch records a load of the given movie. If the ratingKey is
// already in the list, it's promoted to the front; otherwise it's
// inserted at the front and the list is truncated to cap. Persists
// to disk best-effort; a write failure is logged but not fatal.
func (r *RecentMovies) Touch(ratingKey, title string, year int) {
	if ratingKey == "" {
		return
	}
	r.mu.Lock()
	// Remove any existing entry for this ratingKey.
	kept := r.entries[:0]
	for _, e := range r.entries {
		if e.RatingKey != ratingKey {
			kept = append(kept, e)
		}
	}
	// Insert fresh entry at the front.
	entry := RecentMovie{
		RatingKey:    ratingKey,
		Title:        title,
		Year:         year,
		LastPlayedAt: time.Now().Unix(),
	}
	r.entries = append([]RecentMovie{entry}, kept...)
	if len(r.entries) > r.cap {
		r.entries = r.entries[:r.cap]
	}
	snapshot := make([]RecentMovie, len(r.entries))
	copy(snapshot, r.entries)
	r.mu.Unlock()

	b, err := json.Marshal(snapshot)
	if err != nil {
		log.Printf("recent: marshal: %v", err)
		return
	}
	// Atomic write: stage into a sibling .tmp file and rename. Without
	// this an interrupted write would leave a truncated JSON file on
	// disk and the next Load would discard everything as malformed.
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("recent: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("recent: rename %s -> %s: %v", tmp, r.path, err)
		_ = os.Remove(tmp)
	}
}

// List returns a copy of the current recent-played list, newest
// first. Safe for concurrent callers (HTTP handlers).
func (r *RecentMovies) List() []RecentMovie {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecentMovie, len(r.entries))
	copy(out, r.entries)
	return out
}
