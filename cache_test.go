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

func TestSegmentCacheLRUEviction(t *testing.T) {
	// Cap at 20 bytes; "0123456789" is 10 bytes per segment.
	c := NewSegmentCache(t.TempDir(), 20)
	keys := []cacheKey{
		{ratingKey: "rk1", startMs: 0, endMs: 1000},
		{ratingKey: "rk1", startMs: 1000, endMs: 2000},
		{ratingKey: "rk1", startMs: 2000, endMs: 3000},
	}
	for _, k := range keys {
		if _, err := c.Put(k, strings.NewReader("0123456789")); err != nil {
			t.Fatalf("Put %v: %v", k, err)
		}
	}
	// First key should be evicted; second + third remain.
	if _, ok := c.Get(keys[0]); ok {
		t.Fatal("expected keys[0] evicted")
	}
	if _, ok := c.Get(keys[1]); !ok {
		t.Fatal("expected keys[1] present")
	}
	if _, ok := c.Get(keys[2]); !ok {
		t.Fatal("expected keys[2] present")
	}
}

func TestSegmentCacheLRUTouchOnGet(t *testing.T) {
	c := NewSegmentCache(t.TempDir(), 20)
	k1 := cacheKey{ratingKey: "rk1", startMs: 0, endMs: 1000}
	k2 := cacheKey{ratingKey: "rk1", startMs: 1000, endMs: 2000}
	k3 := cacheKey{ratingKey: "rk1", startMs: 2000, endMs: 3000}
	_, _ = c.Put(k1, strings.NewReader("0123456789"))
	_, _ = c.Put(k2, strings.NewReader("0123456789"))
	// Touch k1 so k2 becomes the LRU.
	_, _ = c.Get(k1)
	_, _ = c.Put(k3, strings.NewReader("0123456789"))
	if _, ok := c.Get(k2); ok {
		t.Fatal("expected k2 evicted (was LRU after touching k1)")
	}
	if _, ok := c.Get(k1); !ok {
		t.Fatal("expected k1 present")
	}
}
