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

func TestListMoviesRatings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[
				{"key":"1","type":"movie","title":"Movies"},
				{"key":"2","type":"show","title":"TV"}]}}`))
		case "/library/sections/1/all":
			// Mirrors the real listing: scalar rating/audienceRating, no
			// capital arrays. The first item ALSO carries a capital "Rating"
			// array — the listing endpoint doesn't send one today, but if it
			// ever does the absorber field must keep the decode from
			// colliding (the bug that 502'd every load). One item has only an
			// audience rating; one has neither.
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"ratingKey":"10","title":"The 'Burbs","year":1990,"rating":5.8,"audienceRating":7.1,
				 "Rating":[{"value":5.8,"type":"critic"},{"value":7.1,"type":"audience"}]},
				{"ratingKey":"11","title":"A '90s Christmas","year":2022,"audienceRating":6.0},
				{"ratingKey":"12","title":"Unrated Obscurity","year":1998}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)

	movies, err := p.ListMovies()
	if err != nil {
		t.Fatalf("ListMovies: %v", err)
	}
	if len(movies) != 3 {
		t.Fatalf("got %d movies, want 3 (show section must be skipped)", len(movies))
	}
	if movies[0].Title != "The 'Burbs" || movies[0].Rating != 5.8 || movies[0].AudienceRating != 7.1 {
		t.Errorf("movie[0] = %+v, want The 'Burbs 5.8/7.1", movies[0])
	}
	if movies[1].Rating != 0 || movies[1].AudienceRating != 6.0 {
		t.Errorf("movie[1] ratings = %v/%v, want 0/6.0", movies[1].Rating, movies[1].AudienceRating)
	}
	if movies[2].Rating != 0 || movies[2].AudienceRating != 0 {
		t.Errorf("movie[2] ratings = %v/%v, want 0/0", movies[2].Rating, movies[2].AudienceRating)
	}
}

func TestResolveMovieMeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Real Plex returns BOTH a scalar "guid"/"rating" and a capital-letter
		// "Guid"/"Rating" array — this fixture mirrors that so the case-
		// insensitive-collision regression stays caught.
		w.Write([]byte(`{"MediaContainer":{"Metadata":[{
			"title":"Real Genius","guid":"plex://movie/5d776b","tagline":"He gets creative.","summary":"Plot.",
			"contentRating":"PG","rating":7.7,"audienceRating":8.2,
			"Rating":[{"image":"imdb://image.rating","value":7.7,"type":"critic"},{"image":"themoviedb://image.rating","value":8.2,"type":"audience"}],
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
