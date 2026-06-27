package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newPosterPlex(t *testing.T) *Plex {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/55":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"thumb":"/library/metadata/55/thumb/1"}]}}`))
		case "/photo/:/transcode": // card-sized poster transcode
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("IMG"))
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

func TestPosterHandler(t *testing.T) {
	cache := NewPosterCache(newPosterPlex(t), t.TempDir(), time.Hour)
	h := posterHandler(cache)

	// Valid key → 200 image/jpeg, no token in response.
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/poster/55.jpg", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("valid key code = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q", ct)
	}
	if w.Body.String() != "IMG" {
		t.Errorf("body = %q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tok") {
		t.Errorf("response body contains Plex token")
	}
	for _, hdr := range []string{"X-Plex-Token", "Authorization"} {
		if v := w.Header().Get(hdr); v != "" {
			t.Errorf("response header %s should be absent, got %q", hdr, v)
		}
	}

	// Invalid (non-alphanumeric) key → 400. "a-b" survives the .jpg/prefix
	// trim and then fails validRatingKey's [A-Za-z0-9] filter on the hyphen.
	w = httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/poster/a-b.jpg", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad key code = %d, want 400", w.Code)
	}

	// Missing thumb → 404.
	w = httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/poster/77.jpg", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("no-thumb code = %d, want 404", w.Code)
	}

	// Upstream Plex error (unknown key → mock 404 on the metadata GET) → 404,
	// exercising the non-errNoPoster error branch (log + NotFound).
	w = httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/poster/99.jpg", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("plex-error code = %d, want 404", w.Code)
	}
}
