package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// AliasEntry is one email→alias mapping, as stored on disk and returned
// to the admin panel.
type AliasEntry struct {
	Email string `json:"email"`
	Alias string `json:"alias"`
}

// AliasStore maps a verified email to an admin-assigned display alias
// that replaces the viewer's Google-profile name everywhere the roster
// is shown. Persisted to aliases.json with atomic tmp+rename writes so
// mappings survive restarts — same flat-file pattern as recent.go /
// host.go (no database). Safe for concurrent use.
type AliasStore struct {
	path string
	mu   sync.RWMutex
	m    map[string]string // lowercased email -> alias

	writeMu sync.Mutex // serializes file rewrites (see persist)
}

// NewAliasStore constructs the store and loads any existing mappings.
// A missing or corrupt file just starts empty.
func NewAliasStore(path string) *AliasStore {
	s := &AliasStore{path: path, m: make(map[string]string)}
	s.load()
	return s
}

// load reads aliases.json into memory. Missing file is fine; a corrupt
// file is logged and ignored — an unreadable alias map must never be
// fatal (mirrors recent.go).
func (s *AliasStore) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("alias: load %s: %v", s.path, err)
		}
		return
	}
	var entries []AliasEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		log.Printf("alias: parse %s: %v (starting fresh)", s.path, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		email := strings.ToLower(strings.TrimSpace(e.Email))
		if email != "" && e.Alias != "" {
			s.m[email] = e.Alias
		}
	}
}

// Get returns the alias for email, or "" if none is set.
func (s *AliasStore) Get(email string) string {
	if email == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[strings.ToLower(strings.TrimSpace(email))]
}

// Set stores email→alias and persists. Callers validate/sanitize first
// (see parseAliasArgs); empty inputs are ignored defensively.
func (s *AliasStore) Set(email, alias string) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || alias == "" {
		return
	}
	s.mu.Lock()
	s.m[email] = alias
	s.mu.Unlock()
	s.persist()
}

// Remove deletes email's mapping and persists. No-op if absent.
func (s *AliasStore) Remove(email string) {
	email = strings.ToLower(strings.TrimSpace(email))
	s.mu.Lock()
	delete(s.m, email)
	s.mu.Unlock()
	s.persist()
}

// List returns all mappings sorted by email, as a copy so callers can't
// mutate internal state.
func (s *AliasStore) List() []AliasEntry {
	s.mu.RLock()
	out := make([]AliasEntry, 0, len(s.m))
	for email, alias := range s.m {
		out = append(out, AliasEntry{Email: email, Alias: alias})
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// persist atomically rewrites aliases.json from the in-memory map.
// Best-effort: a write failure is logged but the mapping still lives in
// memory for the session. writeMu serializes writers and the snapshot is
// taken at write time (audit.go's pattern), so concurrent Set/Remove
// can't interleave into a shared temp file or rename an older snapshot
// over a newer one.
func (s *AliasStore) persist() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	entries := s.List() // snapshot at write time
	b, err := json.Marshal(entries)
	if err != nil {
		log.Printf("alias: marshal: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("alias: mkdir: %v", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".aliases-*")
	if err != nil {
		log.Printf("alias: temp: %v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		log.Printf("alias: write %s: %v", tmpName, err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		log.Printf("alias: close %s: %v", tmpName, err)
		return
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		log.Printf("alias: rename %s -> %s: %v", tmpName, s.path, err)
		_ = os.Remove(tmpName)
	}
}

// parseAliasArgs normalizes and validates an alias-set request. Email is
// lowercased/trimmed and must contain '@'. Alias is run through
// sanitizeName (the server-side boundary: standard printable ASCII only,
// ≤ maxViewerName — unicode, emoji, accents, and control chars are
// stripped). Returns ok=false if either is unusable.
func parseAliasArgs(email, alias string) (string, string, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return "", "", false
	}
	alias = sanitizeName(alias)
	if alias == "" {
		return "", "", false
	}
	return email, alias, true
}
