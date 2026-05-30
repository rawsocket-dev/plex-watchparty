package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// HostStore persists the active host's email across restarts. On boot the
// Hub loads it so the same person keeps the remote: their browser
// reconnects within the host-reassign grace window and reclaims the slot
// (and if they never return, it's handed on as usual). Atomic tmp+rename
// writes — a failure just means the host isn't restored next boot.
type HostStore struct {
	path string
	mu   sync.Mutex
	// wg tracks in-flight SaveAsync goroutines so Close can drain pending
	// writes before the data dir is torn down (graceful shutdown + tests).
	wg sync.WaitGroup
}

func NewHostStore(path string) *HostStore { return &HostStore{path: path} }

// Save writes the email atomically. An empty email clears the file.
func (s *HostStore) Save(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if email == "" {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			log.Printf("host: clear: %v", err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("host: mkdir: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(email), 0o644); err != nil {
		log.Printf("host: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("host: rename: %v", err)
		_ = os.Remove(tmp)
	}
}

// SaveAsync persists off the broadcast hot path so the room lock is never
// held across a disk write. Tracked by wg so Close can drain it.
func (s *HostStore) SaveAsync(email string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.Save(email)
	}()
}

// Wait blocks until all in-flight SaveAsync writes have completed.
func (s *HostStore) Wait() { s.wg.Wait() }

// Load returns the persisted active host email (lowercased + trimmed), or
// "" if the file is missing/empty/unreadable.
func (s *HostStore) Load() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("host: load %s: %v", s.path, err)
		}
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(b)))
}
