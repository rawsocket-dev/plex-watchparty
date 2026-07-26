package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RecentMovie is one entry in the recently-played list shown on the
// waiting room. Captured at /control load time, just enough metadata
// to render a card and round-trip a re-load.
type RecentMovie struct {
	RatingKey     string              `json:"ratingKey"`
	Title         string              `json:"title"`
	Year          int                 `json:"year"`
	MediaType     string              `json:"mediaType,omitempty"`
	SeriesTitle   string              `json:"seriesTitle,omitempty"`
	SeasonNumber  int                 `json:"seasonNumber,omitempty"`
	EpisodeNumber int                 `json:"episodeNumber,omitempty"`
	ArtworkKey    string              `json:"artworkKey,omitempty"`
	NextEpisode   *NextEpisodeSummary `json:"nextEpisode,omitempty"`
	LastPlayedAt  int64               `json:"lastPlayedAt"` // unix seconds
}

// RecentMovies is a tiny LRU-ish list of recently-played movies,
// persisted to disk so it survives container restarts. The store is
// bounded by recentCap so the JSON file never grows unbounded.
type RecentMovies struct {
	path string
	cap  int

	mu      sync.Mutex
	entries []RecentMovie

	writeMu sync.Mutex // serializes file rewrites (see persist)
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
	r.TouchMedia(RecentMovie{RatingKey: ratingKey, Title: title, Year: year})
}

// TouchMedia is the media-neutral form of Touch. Touch remains as a
// compatibility wrapper for existing movie callers and persisted data.
func (r *RecentMovies) TouchMedia(entry RecentMovie) {
	ratingKey := entry.RatingKey
	if ratingKey == "" {
		return
	}
	r.mu.Lock()
	// Build a fresh slice so we never alias r.entries while iterating
	// — the prior pattern (kept := r.entries[:0]) was correct only
	// because of the surrounding lock, and the foot-gun isn't worth
	// the saved allocation on a list that maxes out at recentCap.
	entry.LastPlayedAt = time.Now().Unix()
	updated := make([]RecentMovie, 0, len(r.entries)+1)
	updated = append(updated, entry)
	for _, e := range r.entries {
		if e.RatingKey != ratingKey {
			updated = append(updated, e)
		}
	}
	r.entries = updated
	if len(r.entries) > r.cap {
		r.entries = r.entries[:r.cap]
	}
	r.mu.Unlock()
	r.persist()
}

// persist atomically rewrites the JSON file. writeMu serializes writers
// and the snapshot is taken AT WRITE TIME (audit.go's pattern), so the
// last write to land always carries the newest committed list — two
// concurrent Touches can't rename an older snapshot over a newer one or
// interleave into a shared temp file.
func (r *RecentMovies) persist() {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	snapshot := make([]RecentMovie, len(r.entries))
	copy(snapshot, r.entries)
	r.mu.Unlock()

	b, err := json.Marshal(snapshot)
	if err != nil {
		log.Printf("recent: marshal: %v", err)
		return
	}
	// Atomic write: stage into a unique temp file and rename. Without
	// this an interrupted write would leave a truncated JSON file on
	// disk and the next Load would discard everything as malformed.
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".recent-*")
	if err != nil {
		log.Printf("recent: temp: %v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		log.Printf("recent: write %s: %v", tmpName, err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		log.Printf("recent: close %s: %v", tmpName, err)
		return
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		log.Printf("recent: rename %s -> %s: %v", tmpName, r.path, err)
		_ = os.Remove(tmpName)
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
