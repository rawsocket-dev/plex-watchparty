package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// newCountingPosterPlex is like newPosterPlex but counts metadata fetches in
// *hits, so a test can assert when a request actually reached Plex (a cache
// miss) versus was served from disk (a hit).
func newCountingPosterPlex(t *testing.T, hits *int64) *Plex {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/55":
			atomic.AddInt64(hits, 1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"thumb":"/library/metadata/55/thumb/1"}]}}`))
		case "/library/metadata/55/thumb/1":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("IMGDATA"))
		case "/library/metadata/77":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)
}

func readClose(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read poster: %v", err)
	}
	return string(b)
}

func TestPosterCacheServesFromDisk(t *testing.T) {
	var hits int64
	dir := t.TempDir()
	c := NewPosterCache(newCountingPosterPlex(t, &hits), dir, time.Hour)

	// First call: miss → one Plex round trip, bytes returned + written to disk.
	body, ct, err := c.Stream("55")
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	if got := readClose(t, body); got != "IMGDATA" {
		t.Fatalf("first body = %q, want IMGDATA", got)
	}
	if ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	if hits != 1 {
		t.Fatalf("after miss hits = %d, want 1", hits)
	}
	if _, err := os.Stat(filepath.Join(dir, "55.jpg")); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	// Second call: hit → served from disk, no additional Plex round trip.
	body, _, err = c.Stream("55")
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	if got := readClose(t, body); got != "IMGDATA" {
		t.Fatalf("disk body = %q, want IMGDATA", got)
	}
	if hits != 1 {
		t.Errorf("after disk hit hits = %d, want 1 (no refetch)", hits)
	}
}

func TestPosterCacheTTLRefetch(t *testing.T) {
	var hits int64
	dir := t.TempDir()
	c := NewPosterCache(newCountingPosterPlex(t, &hits), dir, time.Hour)

	if _, _, err := c.Stream("55"); err != nil {
		t.Fatalf("warm Stream: %v", err)
	}
	if hits != 1 {
		t.Fatalf("after warm hits = %d, want 1", hits)
	}

	// Age the cached file past the TTL; the next read must refetch from Plex.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "55.jpg"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, _, err := c.Stream("55"); err != nil {
		t.Fatalf("post-expiry Stream: %v", err)
	}
	if hits != 2 {
		t.Errorf("after TTL expiry hits = %d, want 2 (refetched)", hits)
	}
}

func TestPosterCacheNeverExpiresWhenTTLZero(t *testing.T) {
	var hits int64
	dir := t.TempDir()
	c := NewPosterCache(newCountingPosterPlex(t, &hits), dir, 0)

	if _, _, err := c.Stream("55"); err != nil {
		t.Fatalf("warm Stream: %v", err)
	}
	// Even an ancient file is served from disk when ttl <= 0.
	old := time.Now().Add(-1000 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "55.jpg"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, _, err := c.Stream("55"); err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	if hits != 1 {
		t.Errorf("ttl=0 hits = %d, want 1 (never expires)", hits)
	}
}

func TestPosterCacheNoThumbNotCached(t *testing.T) {
	var hits int64
	dir := t.TempDir()
	c := NewPosterCache(newCountingPosterPlex(t, &hits), dir, time.Hour)

	if _, _, err := c.Stream("77"); err != errNoPoster {
		t.Fatalf("err = %v, want errNoPoster", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "77.jpg")); !os.IsNotExist(err) {
		t.Errorf("errNoPoster should not write a cache file (stat err = %v)", err)
	}
}
