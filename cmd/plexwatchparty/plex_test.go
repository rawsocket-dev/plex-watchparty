package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPosterStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/metadata/55":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"title":"M","thumb":"/library/metadata/55/thumb/123","Media":[{"Part":[{"key":"/p"}]}]}]}}`))
		case r.URL.Path == "/photo/:/transcode":
			if r.URL.Query().Get("X-Plex-Token") != "tok" {
				t.Errorf("token missing on poster transcode")
			}
			if got := r.URL.Query().Get("url"); got != "/library/metadata/55/thumb/123" {
				t.Errorf("transcode url = %q, want the thumb path", got)
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
	// Both keys are library titles (posters are only served for those);
	// their empty Thumb forces the per-key metadata fallback.
	p.moviesMu.Lock()
	p.moviesByKey = buildMoviesIndex([]Movie{{RatingKey: "55"}, {RatingKey: "77"}})
	p.moviesMu.Unlock()

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
		case "/photo/:/transcode":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)
	p.moviesMu.Lock()
	p.moviesByKey = buildMoviesIndex([]Movie{{RatingKey: "99"}})
	p.moviesMu.Unlock()
	body, _, err := p.PosterStream("99")
	if err == nil {
		if body != nil {
			body.Close()
		}
		t.Fatal("expected error on non-200 thumb fetch, got nil")
	}
}

// imdbFixtures maps a ratingKey to the IMDb score the per-movie /library/
// metadata enrichment should surface. Key "12" is deliberately absent (no
// imdb:// entry in its Rating array) to prove a missing IMDb stays 0.
var imdbFixtures = map[string]float64{"10": 5.9, "11": 6.3}

// metadataBatchHandler serves the comma-batched /library/metadata/<keys>
// enrichment call, recording which keys were requested so carry-forward can
// be asserted. Each movie carries BOTH a scalar "rating" and a capital
// "Rating" array (and a scalar "guid") — mirroring real Plex so the
// case-insensitive-collision guard stays exercised.
func metadataBatchHandler(t *testing.T, requested *[]string, mu *sync.Mutex) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		keys := strings.Split(strings.TrimPrefix(r.URL.Path, "/library/metadata/"), ",")
		mu.Lock()
		*requested = append(*requested, keys...)
		mu.Unlock()
		var items []string
		for _, k := range keys {
			ratings := `{"image":"rottentomatoes://image.rating.ripe","value":8.1,"type":"critic"},{"image":"themoviedb://image.rating","value":6.8,"type":"audience"}`
			if v, ok := imdbFixtures[k]; ok {
				ratings = `{"image":"imdb://image.rating","value":` + strconv.FormatFloat(v, 'f', -1, 64) + `,"type":"audience"},` + ratings
			}
			items = append(items, `{"ratingKey":"`+k+`","rating":8.1,"audienceRating":7.7,"guid":"plex://movie/`+k+`","Rating":[`+ratings+`]}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"MediaContainer":{"Metadata":[` + strings.Join(items, ",") + `]}}`))
	}
}

func TestListMoviesRatings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/library/metadata/") {
			metadataBatchHandler(t, &[]string{}, &sync.Mutex{})(w, r)
			return
		}
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
	// RT audience (scalar audienceRating from the listing) is kept verbatim.
	if movies[0].Title != "The 'Burbs" || movies[0].AudienceRating != 7.1 {
		t.Errorf("movie[0] = %+v, want The 'Burbs audience 7.1", movies[0])
	}
	if movies[1].AudienceRating != 6.0 {
		t.Errorf("movie[1] audienceRating = %v, want 6.0", movies[1].AudienceRating)
	}
	// IMDb is enriched from the per-movie Rating array; a movie with no
	// imdb:// entry stays at 0.
	if movies[0].IMDbRating != 5.9 {
		t.Errorf("movie[0] IMDbRating = %v, want 5.9", movies[0].IMDbRating)
	}
	if movies[1].IMDbRating != 6.3 {
		t.Errorf("movie[1] IMDbRating = %v, want 6.3", movies[1].IMDbRating)
	}
	if movies[2].IMDbRating != 0 {
		t.Errorf("movie[2] IMDbRating = %v, want 0 (no imdb:// entry)", movies[2].IMDbRating)
	}
}

// TestListMoviesIMDbCarryForward proves the second refresh reuses IMDb scores
// already in the cache and only re-fetches the title that still lacks one.
func TestListMoviesIMDbCarryForward(t *testing.T) {
	var mu sync.Mutex
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/library/metadata/") {
			metadataBatchHandler(t, &requested, &mu)(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie","title":"Movies"}]}}`))
		case "/library/sections/1/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"ratingKey":"10","title":"Has IMDb","year":1990},
				{"ratingKey":"12","title":"No IMDb","year":1998}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)

	if _, err := p.ListMovies(); err != nil {
		t.Fatalf("first ListMovies: %v", err)
	}
	mu.Lock()
	requested = nil // reset; the first pass legitimately fetched both
	mu.Unlock()

	p.RefreshLibrary() // rewind TTL so the next call re-fetches the listing
	movies, err := p.ListMovies()
	if err != nil {
		t.Fatalf("second ListMovies: %v", err)
	}
	if movies[0].IMDbRating != 5.9 {
		t.Errorf("carried-forward IMDb = %v, want 5.9", movies[0].IMDbRating)
	}
	mu.Lock()
	defer mu.Unlock()
	// Key 10 already had an IMDb score → must NOT be re-fetched. Key 12 never
	// had one → it's the only key worth a second look.
	for _, k := range requested {
		if k == "10" {
			t.Errorf("key 10 was re-fetched on refresh; should have been carried forward (requested=%v)", requested)
		}
	}
	if len(requested) != 1 || requested[0] != "12" {
		t.Errorf("second-refresh enrichment requested %v, want only [12]", requested)
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

// listingHandler is a minimal healthy Plex listing (one movie section, two
// titles, batch-enrichment endpoint) whose failure can be toggled: while
// failing, every request returns 502.
func listingHandler(failing *atomic.Bool, sectionsCalls *atomic.Int64, delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if failing != nil && failing.Load() {
			http.Error(w, "plex down", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/library/sections":
			if sectionsCalls != nil {
				sectionsCalls.Add(1)
			}
			time.Sleep(delay)
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie","title":"Movies"}]}}`))
		case r.URL.Path == "/library/sections/1/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"ratingKey":"10","title":"A","year":1990},
				{"ratingKey":"11","title":"B","year":1991}]}}`))
		case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
		default:
			http.NotFound(w, r)
		}
	}
}

// A refresh that fails (Plex down, TTL expired) must fall back to the
// previously-cached list instead of breaking the library page.
func TestListMoviesServesStaleCacheWhenPlexDown(t *testing.T) {
	var failing atomic.Bool
	srv := httptest.NewServer(listingHandler(&failing, nil, 0))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)

	first, err := p.ListMovies()
	if err != nil || len(first) != 2 {
		t.Fatalf("prime: %v (%d movies)", err, len(first))
	}
	p.RefreshLibrary() // expire the TTL
	failing.Store(true)

	got, err := p.ListMovies()
	if err != nil {
		t.Fatalf("ListMovies with Plex down = error %v, want stale fallback", err)
	}
	if len(got) != 2 {
		t.Errorf("stale fallback returned %d movies, want 2", len(got))
	}
}

// Concurrent callers hitting an expired cache must collapse to ONE
// upstream refresh, not N full library walks.
func TestListMoviesSingleflightCollapsesConcurrentRefreshes(t *testing.T) {
	var sectionsCalls atomic.Int64
	srv := httptest.NewServer(listingHandler(nil, &sectionsCalls, 50*time.Millisecond))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.ListMovies(); err != nil {
				t.Errorf("ListMovies: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := sectionsCalls.Load(); got != 1 {
		t.Errorf("%d concurrent ListMovies performed %d upstream refreshes, want 1", 8, got)
	}
}

// The poster endpoint is unauthenticated (Discord fetches embed images
// from the public internet), so a key the library cache doesn't know must
// be refused WITHOUT a Plex round-trip — otherwise alphanumeric key scans
// enumerate the whole Plex server and hammer it with metadata fetches.
func TestPosterStreamRefusesKeysOutsideLibrary(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"MediaContainer":{"Metadata":[{"thumb":"/library/metadata/999/thumb/1"}]}}`))
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)
	// Library cache knows only key 55.
	p.moviesMu.Lock()
	p.moviesByKey = buildMoviesIndex([]Movie{{RatingKey: "55", Title: "Known"}})
	p.moviesMu.Unlock()

	if _, _, err := p.PosterStream("999"); err != errNoPoster {
		t.Errorf("unknown key err = %v, want errNoPoster", err)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("unknown key reached Plex %d times, want 0", got)
	}
}

// A title Plex has no IMDb rating for must not be re-fetched on every
// TTL-expiry refresh — "known unrated" carries forward like a score does.
// An explicit admin refresh (RefreshLibrary) clears that memory so newly
// rated titles can be picked up on demand.
func TestListMoviesUnratedNotRefetchedOnTTLRefresh(t *testing.T) {
	var mu sync.Mutex
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/library/metadata/") {
			metadataBatchHandler(t, &requested, &mu)(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie","title":"Movies"}]}}`))
		case "/library/sections/1/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"ratingKey":"10","title":"Has IMDb","year":1990},
				{"ratingKey":"12","title":"No IMDb","year":1998}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)

	if _, err := p.ListMovies(); err != nil {
		t.Fatalf("first ListMovies: %v", err)
	}
	mu.Lock()
	requested = nil
	mu.Unlock()

	// TTL-expiry refresh (no admin action): nothing should be re-fetched —
	// 10's score carries forward, 12 is known unrated.
	p.moviesMu.Lock()
	p.moviesAt = time.Now().Add(-2 * moviesCacheTTL)
	p.moviesMu.Unlock()
	if _, err := p.ListMovies(); err != nil {
		t.Fatalf("TTL refresh: %v", err)
	}
	mu.Lock()
	if len(requested) != 0 {
		t.Errorf("TTL refresh re-fetched %v, want none (unrated title should carry forward)", requested)
	}
	requested = nil
	mu.Unlock()

	// Admin refresh: the unrated title gets one fresh look.
	p.RefreshLibrary()
	if _, err := p.ListMovies(); err != nil {
		t.Fatalf("admin refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requested) != 1 || requested[0] != "12" {
		t.Errorf("admin refresh requested %v, want only [12]", requested)
	}
}
