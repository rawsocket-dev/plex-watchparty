package main

import (
	"path/filepath"
	"testing"
)

func TestAuditLogCapDropsOldest(t *testing.T) {
	a := NewAuditLog(filepath.Join(t.TempDir(), "audit.jsonl"), 3)
	for i := 0; i < 5; i++ {
		a.Record(AuditEvent{Type: "signin", Email: string(rune('a'+i)) + "@x.com"})
	}
	got := a.List()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (capped)", len(got))
	}
	if got[0].Email != "e@x.com" {
		t.Errorf("newest = %q, want e@x.com", got[0].Email)
	}
	if got[2].Email != "c@x.com" {
		t.Errorf("oldest kept = %q, want c@x.com (a,b dropped)", got[2].Email)
	}
}

func TestAuditLogListNewestFirstAndCopy(t *testing.T) {
	a := NewAuditLog(filepath.Join(t.TempDir(), "audit.jsonl"), 10)
	a.Record(AuditEvent{Type: "signin", Email: "first@x.com"})
	a.Record(AuditEvent{Type: "admin", Email: "second@x.com"})
	got := a.List()
	if got[0].Email != "second@x.com" || got[1].Email != "first@x.com" {
		t.Fatalf("order = [%q,%q], want newest first", got[0].Email, got[1].Email)
	}
	got[0].Email = "tampered"
	if a.List()[0].Email != "second@x.com" {
		t.Error("List did not return a copy")
	}
}

func TestAuditLogStampsTime(t *testing.T) {
	a := NewAuditLog(filepath.Join(t.TempDir(), "audit.jsonl"), 10)
	a.Record(AuditEvent{Type: "signin", Email: "x@x.com"})
	if a.List()[0].Unix == 0 {
		t.Error("Record did not stamp Unix")
	}
}

func TestAuditLogPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a := NewAuditLog(path, 500)
	a.Record(AuditEvent{Type: "signin", Email: "one@x.com", Role: "host"})
	a.Record(AuditEvent{Type: "admin", Email: "two@x.com", Role: "admin", Detail: "cleared cache"})

	reloaded := NewAuditLog(path, 500)
	got := reloaded.List()
	if len(got) != 2 {
		t.Fatalf("reloaded len = %d, want 2", len(got))
	}
	if got[0].Email != "two@x.com" || got[0].Detail != "cleared cache" {
		t.Errorf("reloaded newest = %+v, want two@x.com/cleared cache", got[0])
	}
}

func TestAuditLogLoadCapsToLastN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	seed := NewAuditLog(path, 500)
	for i := 0; i < 10; i++ {
		seed.Record(AuditEvent{Type: "signin", Email: string(rune('a'+i)) + "@x.com"})
	}
	reloaded := NewAuditLog(path, 3)
	got := reloaded.List()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Email != "j@x.com" {
		t.Errorf("newest = %q, want j@x.com", got[0].Email)
	}
}

func TestAuditLogMissingFileIsEmpty(t *testing.T) {
	a := NewAuditLog(filepath.Join(t.TempDir(), "nope.jsonl"), 10)
	if len(a.List()) != 0 {
		t.Error("missing file should yield empty log")
	}
}

func TestAuditLogNilSafe(t *testing.T) {
	var a *AuditLog
	a.Record(AuditEvent{Type: "signin"}) // must not panic
	if a.List() != nil {
		t.Error("nil AuditLog.List() should be nil")
	}
}
