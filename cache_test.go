package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSegmentCachePutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := NewSegmentCache(dir, 1<<30)
	key := cacheKey{ratingKey: "rk1", startMs: 0, endMs: 6000}
	path, err := c.Put(key, strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	gotPath, ok := c.Get(key)
	if !ok {
		t.Fatal("Get: not found")
	}
	if gotPath != path {
		t.Fatalf("Get path = %q, want %q", gotPath, path)
	}
	b, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "hello" {
		t.Fatalf("content = %q, want %q", string(b), "hello")
	}
}

func TestSegmentCacheAtomicRename(t *testing.T) {
	dir := t.TempDir()
	c := NewSegmentCache(dir, 1<<30)
	key := cacheKey{ratingKey: "rk1", startMs: 0, endMs: 6000}

	// Write a stale .tmp file that should NOT survive a clean Put.
	movieDir := filepath.Join(dir, "rk1")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tmpPath := filepath.Join(movieDir, "seg_0_6000.ts.tmp")
	if err := os.WriteFile(tmpPath, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	if _, err := c.Put(key, strings.NewReader("real")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("expected tmp gone, stat err = %v", err)
	}
	gotPath, _ := c.Get(key)
	b, _ := os.ReadFile(gotPath)
	if string(b) != "real" {
		t.Fatalf("final content = %q, want %q", string(b), "real")
	}
}

func TestSegmentCacheGetMiss(t *testing.T) {
	c := NewSegmentCache(t.TempDir(), 1<<30)
	if _, ok := c.Get(cacheKey{ratingKey: "rk1", startMs: 0, endMs: 6000}); ok {
		t.Fatal("Get: unexpected hit on empty cache")
	}
}
