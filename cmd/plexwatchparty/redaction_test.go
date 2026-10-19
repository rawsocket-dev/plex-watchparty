package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Plex token must never reach a viewer or the audit log. A transport
// failure (Plex down, DNS blip, timeout) used to surface Go's *url.Error,
// whose text is the full request URL — and the playlist URL carries
// X-Plex-Token — straight into the 502 body every viewer's hls.js polls,
// and into audit.jsonl via the health tracker.
func TestPlexTransportErrorsDoNotExposeToken(t *testing.T) {
	f := newSegTestFixture(t)
	if w := f.post(t, `{"action":"load","ratingKey":"rk1"}`); w.Code != http.StatusOK {
		t.Fatalf("load: %d %s", w.Code, w.Body.String())
	}
	f.mock.Close()

	w := httptest.NewRecorder()
	playlistHandler(f.codec, f.hub.session, f.bw)(w, httptest.NewRequest("GET", "/hls/index.m3u8", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("playlist status = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), "X-Plex-Token=tok") {
		t.Errorf("viewer-visible playlist error carries the token: %q", w.Body.String())
	}

	f.hub.Close() // flush pending audit writes
	b, err := os.ReadFile(filepath.Join(f.dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !strings.Contains(string(b), "plex unreachable") {
		t.Fatalf("expected a 'plex unreachable' audit event; got %s", b)
	}
	if strings.Contains(string(b), "X-Plex-Token=tok") {
		t.Errorf("audit log carries the token: %s", b)
	}
}
