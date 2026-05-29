package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// AuditEvent is one entry in the admin audit trail: a sign-in, a denied
// sign-in attempt, or an admin maintenance action.
type AuditEvent struct {
	Unix   int64  `json:"unix"`   // event time (set by Record if 0)
	Type   string `json:"type"`   // "signin" | "signin-denied" | "admin"
	Email  string `json:"email"`  // verified email ("" if unknown)
	Role   string `json:"role"`   // "host" | "viewer" | "admin" | ""
	IP     string `json:"ip"`     // client IP at the time
	Detail string `json:"detail"` // action text, or "admin" marker on a signin
}

// auditCap is how many events we retain. 500 is plenty for a small LAN
// deployment and keeps the JSONL file tiny.
const auditCap = 500

// AuditLog is a bounded, disk-backed event log. The newest cap events
// are kept in memory AND mirrored to a JSONL file (one event per line)
// so they survive restarts. Its mutex is independent of the Hub lock
// hierarchy — AuditLog never calls into Hub/PlexSession/SegmentCache.
type AuditLog struct {
	path string
	cap  int

	mu     sync.Mutex
	events []AuditEvent // oldest-first

	writeMu sync.Mutex // serializes file rewrites so concurrent Records can't clobber the .tmp
}

// NewAuditLog builds the store and loads up to the last cap events from
// path. A missing file is fine (empty log); a malformed line is skipped
// rather than discarding the whole file.
func NewAuditLog(path string, maxEvents int) *AuditLog {
	a := &AuditLog{path: path, cap: maxEvents}
	a.load()
	return a
}

func (a *AuditLog) load() {
	f, err := os.Open(a.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("audit: open %s: %v", a.path, err)
		}
		return
	}
	defer f.Close()
	var events []AuditEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			log.Printf("audit: skip malformed line in %s: %v", a.path, err)
			continue
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		log.Printf("audit: read %s: %v", a.path, err)
	}
	if len(events) > a.cap {
		events = events[len(events)-a.cap:]
	}
	a.mu.Lock()
	a.events = events
	a.mu.Unlock()
}

// Record stamps the event time (if unset), appends it, trims to cap,
// and atomically rewrites the JSONL file. Best-effort: a write error is
// logged, never returned — auditing must not fail the request. Safe to
// call on a nil receiver (no-op).
func (a *AuditLog) Record(ev AuditEvent) {
	if a == nil {
		return
	}
	if ev.Unix == 0 {
		ev.Unix = time.Now().Unix()
	}
	a.mu.Lock()
	a.events = append(a.events, ev)
	if len(a.events) > a.cap {
		a.events = a.events[len(a.events)-a.cap:]
	}
	snapshot := make([]AuditEvent, len(a.events))
	copy(snapshot, a.events)
	a.mu.Unlock()
	a.persist(snapshot)
}

func (a *AuditLog) persist(events []AuditEvent) {
	if a.path == "" {
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // Encode appends a newline → JSONL
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			log.Printf("audit: marshal: %v", err)
			return
		}
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		log.Printf("audit: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, a.path); err != nil {
		log.Printf("audit: rename %s -> %s: %v", tmp, a.path, err)
		_ = os.Remove(tmp)
	}
}

// List returns a copy of the retained events, NEWEST FIRST. Safe to
// call on a nil receiver (returns nil).
func (a *AuditLog) List() []AuditEvent {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuditEvent, len(a.events))
	for i, ev := range a.events { // reverse: internal slice is oldest-first
		out[len(a.events)-1-i] = ev
	}
	return out
}
