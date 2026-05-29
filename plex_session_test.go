package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPlexSessionStart(t *testing.T) {
	var lastPath string
	var lastQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		lastQuery = r.URL.Query()
		switch r.URL.Path {
		case "/video/:/transcode/universal/decision":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"generalDecisionText":"Direct play not available; Conversion OK."}}`))
		case "/video/:/transcode/universal/start.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	plex := NewPlex(srv.URL, "tok", "", nil)
	ps := NewPlexSession(plex, 12000)
	if err := ps.Start("rk1", 0); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(lastPath, "/video/:/transcode/universal/start") {
		t.Errorf("unexpected path: %q", lastPath)
	}
	if got := lastQuery.Get("X-Plex-Platform"); got != "Generic" {
		t.Errorf("X-Plex-Platform = %q, want Generic", got)
	}
	if got := lastQuery.Get("maxVideoBitrate"); got != "12000" {
		t.Errorf("maxVideoBitrate = %q, want 12000", got)
	}
	if ps.SessionToken() == 0 {
		t.Error("SessionToken not bumped after Start")
	}
}

func TestPlexSessionStartWithOffset(t *testing.T) {
	var lastQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastQuery = r.URL.Query()
		w.Write([]byte("#EXTM3U\n"))
	}))
	defer srv.Close()

	ps := NewPlexSession(NewPlex(srv.URL, "tok", "", nil), 12000)
	if err := ps.Start("rk1", 600); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := lastQuery.Get("offset"); got != "600" {
		t.Errorf("offset = %q, want 600", got)
	}
}

func TestPlexSessionStopCallsPlex(t *testing.T) {
	var stopped bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/transcode/universal/stop") {
			stopped = true
		}
		w.Write([]byte("#EXTM3U\n"))
	}))
	defer srv.Close()

	ps := NewPlexSession(NewPlex(srv.URL, "tok", "", nil), 12000)
	_ = ps.Start("rk1", 0)
	ps.Stop()
	if !stopped {
		t.Error("expected Stop() to call Plex's /transcode/universal/stop endpoint")
	}
	if ps.ratingKey != "" {
		t.Error("expected ratingKey cleared after Stop")
	}
}

func TestPlexSessionRestartBumpsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n"))
	}))
	defer srv.Close()

	ps := NewPlexSession(NewPlex(srv.URL, "tok", "", nil), 12000)
	_ = ps.Start("rk1", 0)
	tokenBefore := ps.SessionToken()
	if err := ps.Restart(120); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if ps.SessionToken() <= tokenBefore {
		t.Errorf("SessionToken did not bump on Restart (before=%d after=%d)",
			tokenBefore, ps.SessionToken())
	}
}

func TestPlexSessionEdgeMsTracks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n"))
	}))
	defer srv.Close()

	ps := NewPlexSession(NewPlex(srv.URL, "tok", "", nil), 12000)
	_ = ps.Start("rk1", 30) // session starts at 30s
	if got := ps.EdgeSec(); got != 30.0 {
		t.Errorf("EdgeSec after start = %v, want 30.0", got)
	}
	ps.UpdateEdge(95000) // playlist now shows segments out to 95s
	if got := ps.EdgeSec(); got != 95.0 {
		t.Errorf("EdgeSec after update = %v, want 95.0", got)
	}
	// Edge never moves backward.
	ps.UpdateEdge(50000)
	if got := ps.EdgeSec(); got != 95.0 {
		t.Errorf("EdgeSec after backward update = %v, want 95.0 (no regression)", got)
	}
}

func TestPlexSessionFetchPlaylist(t *testing.T) {
	const body = "#EXTM3U\n#EXTINF:10,\nhttps://plex/seg-0.ts\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	ps := NewPlexSession(NewPlex(srv.URL, "tok", "", nil), 12000)
	_ = ps.Start("rk1", 0)
	data, baseURL, err := ps.FetchPlaylist()
	if err != nil {
		t.Fatalf("FetchPlaylist: %v", err)
	}
	if string(data) != body {
		t.Errorf("FetchPlaylist body = %q, want %q", string(data), body)
	}
	if baseURL == "" {
		t.Errorf("FetchPlaylist baseURL = %q, want non-empty", baseURL)
	}
}

// TestPlexSessionFetchPlaylistFollowsMaster confirms that a master
// playlist returned by Plex transparently resolves to the variant.
func TestPlexSessionFetchPlaylistFollowsMaster(t *testing.T) {
	const variantBody = "#EXTM3U\n#EXTINF:6,\nseg-0.ts\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "start.m3u8") {
			w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=12000000\nbase/index.m3u8\n"))
			return
		}
		w.Write([]byte(variantBody))
	}))
	defer srv.Close()

	ps := NewPlexSession(NewPlex(srv.URL, "tok", "", nil), 12000)
	_ = ps.Start("rk1", 0)
	data, baseURL, err := ps.FetchPlaylist()
	if err != nil {
		t.Fatalf("FetchPlaylist: %v", err)
	}
	if string(data) != variantBody {
		t.Errorf("FetchPlaylist body = %q, want %q", string(data), variantBody)
	}
	if !strings.HasSuffix(baseURL, "/video/:/transcode/universal/base/index.m3u8") {
		t.Errorf("variant baseURL = %q, expected to end with /base/index.m3u8", baseURL)
	}
}

func TestPlexSessionFetchSegment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("segment-bytes"))
	}))
	defer srv.Close()

	ps := NewPlexSession(NewPlex(srv.URL, "tok", "", nil), 12000)
	_ = ps.Start("rk1", 0)
	rc, err := ps.FetchSegment(srv.URL + "/some-seg.ts")
	if err != nil {
		t.Fatalf("FetchSegment: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "segment-bytes" {
		t.Errorf("FetchSegment data = %q, want %q", string(data), "segment-bytes")
	}
}
