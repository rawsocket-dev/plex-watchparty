package main

import (
	"path/filepath"
	"testing"
)

func TestHostStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.json")
	s := NewHostStore(path)
	if got := s.Load(); got != "" {
		t.Errorf("fresh store Load = %q, want empty", got)
	}
	s.Save("  Alice@X.com  ") // stored as-is; normalized on Load
	if got := s.Load(); got != "alice@x.com" {
		t.Errorf("Load after Save = %q, want lowercased/trimmed alice@x.com", got)
	}
	s.Save("") // empty clears the file
	if got := s.Load(); got != "" {
		t.Errorf("Load after clear = %q, want empty", got)
	}
}
