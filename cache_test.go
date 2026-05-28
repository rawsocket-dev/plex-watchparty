package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestSegmentCacheRangesForEmpty(t *testing.T) {
	c := NewSegmentCache(t.TempDir(), 1<<30)
	got := c.RangesFor("rk1")
	if len(got) != 0 {
		t.Fatalf("RangesFor empty cache: got %v, want []", got)
	}
}

func TestSegmentCacheRangesForMergesAdjacent(t *testing.T) {
	c := NewSegmentCache(t.TempDir(), 1<<30)
	// Three contiguous segments and one separate.
	for _, k := range []cacheKey{
		{ratingKey: "rk1", startMs: 0, endMs: 6000},
		{ratingKey: "rk1", startMs: 6000, endMs: 12000},
		{ratingKey: "rk1", startMs: 12000, endMs: 18000},
		{ratingKey: "rk1", startMs: 30000, endMs: 36000},
	} {
		if _, err := c.Put(k, strings.NewReader("x")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	got := c.RangesFor("rk1")
	want := [][2]float64{{0, 18.0}, {30.0, 36.0}}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (got = %v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestSegmentCacheRangesForSeparatesMovies(t *testing.T) {
	c := NewSegmentCache(t.TempDir(), 1<<30)
	_, _ = c.Put(cacheKey{ratingKey: "rk1", startMs: 0, endMs: 6000}, strings.NewReader("x"))
	_, _ = c.Put(cacheKey{ratingKey: "rk2", startMs: 0, endMs: 6000}, strings.NewReader("x"))
	r1 := c.RangesFor("rk1")
	r2 := c.RangesFor("rk2")
	if len(r1) != 1 || len(r2) != 1 {
		t.Fatalf("expected 1 range per movie, got rk1=%v rk2=%v", r1, r2)
	}
}

func TestSegmentCacheLoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	// First instance: populate.
	c1 := NewSegmentCache(dir, 1<<30)
	for _, k := range []cacheKey{
		{ratingKey: "rk1", startMs: 0, endMs: 6000},
		{ratingKey: "rk1", startMs: 6000, endMs: 12000},
		{ratingKey: "rk2", startMs: 0, endMs: 6000},
	} {
		if _, err := c1.Put(k, strings.NewReader("xxxxx")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Plant a garbage filename + a stale .tmp; both should be ignored / cleaned.
	if err := os.WriteFile(filepath.Join(dir, "rk1", "junk.ts"), []byte("nope"), 0o644); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rk1", "seg_0_6000.ts.tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	// Second instance: load and verify.
	c2 := NewSegmentCache(dir, 1<<30)
	if err := c2.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	for _, k := range []cacheKey{
		{ratingKey: "rk1", startMs: 0, endMs: 6000},
		{ratingKey: "rk1", startMs: 6000, endMs: 12000},
		{ratingKey: "rk2", startMs: 0, endMs: 6000},
	} {
		if _, ok := c2.Get(k); !ok {
			t.Errorf("expected %v in loaded cache", k)
		}
	}
	// .tmp file should be cleaned up.
	if _, err := os.Stat(filepath.Join(dir, "rk1", "seg_0_6000.ts.tmp")); !os.IsNotExist(err) {
		t.Errorf("expected .tmp cleaned, stat err = %v", err)
	}
}

func TestCacheClearAll(t *testing.T) {
	dir := t.TempDir()
	c := NewSegmentCache(dir, 10<<20)
	for i := int64(0); i < 5; i++ {
		_, err := c.Put(cacheKey{ratingKey: "m1", startMs: i * 1000, endMs: (i + 1) * 1000},
			bytes.NewReader([]byte("xxxxxxxx")))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if c.totalBytes == 0 {
		t.Fatal("expected cache to have bytes before Clear")
	}
	entries, n := c.Clear()
	if entries != 5 {
		t.Errorf("Clear returned entries=%d, want 5", entries)
	}
	if n == 0 {
		t.Error("Clear returned bytes=0, want >0")
	}
	if c.totalBytes != 0 || len(c.entries) != 0 {
		t.Errorf("after Clear: totalBytes=%d entries=%d, want both 0",
			c.totalBytes, len(c.entries))
	}
}

func TestCacheClearMovie(t *testing.T) {
	dir := t.TempDir()
	c := NewSegmentCache(dir, 10<<20)
	for _, rk := range []string{"a", "b"} {
		for i := int64(0); i < 3; i++ {
			_, err := c.Put(cacheKey{ratingKey: rk, startMs: i * 1000, endMs: (i + 1) * 1000},
				bytes.NewReader([]byte("yyyyy")))
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
	}
	entries, _ := c.ClearMovie("a")
	if entries != 3 {
		t.Errorf("ClearMovie(a) entries = %d, want 3", entries)
	}
	if got := len(c.entries); got != 3 {
		t.Errorf("remaining entries = %d, want 3 (movie b only)", got)
	}
	for k := range c.entries {
		if k.ratingKey != "b" {
			t.Errorf("after ClearMovie(a), found entry for %q", k.ratingKey)
		}
	}
}

func TestCachePruneByAge(t *testing.T) {
	dir := t.TempDir()
	c := NewSegmentCache(dir, 10<<20)
	// Two entries written now.
	for i := int64(0); i < 2; i++ {
		_, err := c.Put(cacheKey{ratingKey: "m1", startMs: i * 1000, endMs: (i + 1) * 1000},
			bytes.NewReader([]byte("zzzz")))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Reach into one of them and backdate its file mtime.
	for k, e := range c.entries {
		if k.startMs == 0 {
			old := time.Now().Add(-72 * time.Hour)
			if err := os.Chtimes(e.path, old, old); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}
			break
		}
	}
	entries, _ := c.Prune(24 * time.Hour)
	if entries != 1 {
		t.Errorf("Prune(24h) entries = %d, want 1", entries)
	}
	if got := len(c.entries); got != 1 {
		t.Errorf("after Prune, remaining = %d, want 1", got)
	}
}

func TestCacheStats(t *testing.T) {
	dir := t.TempDir()
	c := NewSegmentCache(dir, 10<<20)
	_, _ = c.Put(cacheKey{ratingKey: "a", startMs: 0, endMs: 1000},
		bytes.NewReader([]byte("hello")))
	_, _ = c.Put(cacheKey{ratingKey: "b", startMs: 0, endMs: 1000},
		bytes.NewReader([]byte("hello world")))
	st := c.Stats()
	if st.Entries != 2 {
		t.Errorf("entries = %d, want 2", st.Entries)
	}
	if len(st.PerMovie) != 2 {
		t.Fatalf("perMovie = %d, want 2", len(st.PerMovie))
	}
	// PerMovie sorts by bytes desc, so "b" (11 bytes) should come first.
	if st.PerMovie[0].RatingKey != "b" {
		t.Errorf("top movie = %s, want b (largest by bytes)", st.PerMovie[0].RatingKey)
	}
}
