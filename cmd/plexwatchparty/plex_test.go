package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestPosterStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/metadata/55":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"title":"M","thumb":"/library/metadata/55/thumb/123","Media":[{"Part":[{"key":"/p"}]}]}]}}`))
		case r.URL.Path == "/library/metadata/55/thumb/123":
			if r.URL.Query().Get("X-Plex-Token") != "tok" {
				t.Errorf("token missing on thumb fetch")
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("JPEGBYTES"))
		case r.URL.Path == "/library/metadata/77":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"title":"NoThumb"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)

	body, ct, err := p.PosterStream("55")
	if err != nil {
		t.Fatalf("PosterStream: %v", err)
	}
	defer body.Close()
	if ct != "image/jpeg" {
		t.Errorf("content-type = %q", ct)
	}
	b, _ := io.ReadAll(body)
	if string(b) != "JPEGBYTES" {
		t.Errorf("body = %q", b)
	}

	if _, _, err := p.PosterStream("77"); err != errNoPoster {
		t.Errorf("no-thumb err = %v, want errNoPoster", err)
	}
}

func TestPosterStreamThumbStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/99":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"thumb":"/library/metadata/99/thumb/1"}]}}`))
		case "/library/metadata/99/thumb/1":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)
	body, _, err := p.PosterStream("99")
	if err == nil {
		if body != nil {
			body.Close()
		}
		t.Fatal("expected error on non-200 thumb fetch, got nil")
	}
}

func TestResolveMovieMeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"MediaContainer":{"Metadata":[{
			"title":"Real Genius","tagline":"He gets creative.","summary":"Plot.",
			"contentRating":"PG","rating":7.7,"audienceRating":8.2,
			"Genre":[{"tag":"Comedy"},{"tag":"Sci-Fi"}],
			"Guid":[{"id":"imdb://tt0089886"},{"id":"tmdb://14370"},{"id":"tvdb://4068"}],
			"Media":[{"videoCodec":"hevc","width":3840,"height":2160,"Part":[{"key":"/p"}]}]
		}]}}`))
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)

	_, meta, err := p.Resolve("123")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if meta.Tagline != "He gets creative." || meta.Summary != "Plot." {
		t.Errorf("tagline/summary = %q / %q", meta.Tagline, meta.Summary)
	}
	if meta.ContentRating != "PG" || meta.CriticRating != 7.7 || meta.AudienceRating != 8.2 {
		t.Errorf("ratings = %q %v %v", meta.ContentRating, meta.CriticRating, meta.AudienceRating)
	}
	if len(meta.Genres) != 2 || meta.Genres[0] != "Comedy" || meta.Genres[1] != "Sci-Fi" {
		t.Errorf("genres = %v", meta.Genres)
	}
	// imdb:// id keeps the "tt" prefix; tvdb is ignored.
	if meta.IMDbID != "tt0089886" || meta.TMDBID != "14370" {
		t.Errorf("ids = %q / %q", meta.IMDbID, meta.TMDBID)
	}
}
