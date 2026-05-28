package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ResumeHint is the snapshot of a prior watch session, surfaced
// through State.Resume when no live session is active. The waiting
// room and library use it to offer "Resume X at Y?" after a
// container restart or idle shutdown.
type ResumeHint struct {
	RatingKey   string  `json:"ratingKey"`
	Title       string  `json:"title"`
	PositionSec float64 `json:"positionSec"`
	DurationSec float64 `json:"durationSec"`
	SavedAtUnix int64   `json:"savedAtUnix"`
}

// StateStore persists / loads the watchparty's last-known playback
// state to a JSON file (state.json next to recent.json). Atomic
// tmp+rename writes so a torn write can't corrupt the file. Safe
// to call from any goroutine — the internal mutex serializes writes.
type StateStore struct {
	path string
	mu   sync.Mutex
	// wg tracks in-flight SaveAsync goroutines so callers can drain
	// pending writes before tearing down the directory we write into
	// (graceful shutdown, and crucially tests using t.TempDir whose
	// RemoveAll otherwise races a late write recreating the dir).
	wg sync.WaitGroup
}

func NewStateStore(path string) *StateStore {
	return &StateStore{path: path}
}

// Save writes the hint atomically. Stamps SavedAtUnix here so callers
// don't have to. Errors are logged-and-eaten — a write failure means
// resume-after-restart degrades to "no hint," not a fatal condition.
func (s *StateStore) Save(state ResumeHint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state.SavedAtUnix = time.Now().Unix()
	b, err := json.Marshal(state)
	if err != nil {
		log.Printf("state: marshal: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("state: mkdir: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("state: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("state: rename: %v", err)
		_ = os.Remove(tmp)
	}
}

// SaveAsync persists the hint on a background goroutine so the caller
// (the broadcast hot path) never blocks on a slow filesystem. The
// write is tracked by an internal WaitGroup; call Wait to drain
// in-flight saves before removing the directory the store writes into.
func (s *StateStore) SaveAsync(state ResumeHint) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Save(state)
	}()
}

// Wait blocks until all in-flight SaveAsync writes have completed.
func (s *StateStore) Wait() {
	s.wg.Wait()
}

// Load reads the persisted hint from disk. Returns nil if the file
// doesn't exist (clean install / never persisted), is unreadable, or
// is empty/zero-valued. Called at startup before NewHub.
func (s *StateStore) Load() *ResumeHint {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("state: load %s: %v", s.path, err)
		}
		return nil
	}
	var ps ResumeHint
	if err := json.Unmarshal(b, &ps); err != nil {
		log.Printf("state: parse %s: %v (ignoring)", s.path, err)
		return nil
	}
	if ps.RatingKey == "" {
		return nil
	}
	return &ps
}

// Clear removes the persisted hint. Called when the admin explicitly
// ends a watch session ("Send everyone to lobby") — the user has
// signaled "we're done with that movie," so don't keep offering it.
func (s *StateStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		log.Printf("state: clear: %v", err)
	}
}
