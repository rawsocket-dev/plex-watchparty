package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlexTVCatalogHierarchyNextAndDiskFallback(t *testing.T) {
	var seasonCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[
				{"key":"m1","type":"movie"},{"key":"m2","type":"movie"},
				{"key":"t1","type":"show"},{"key":"t2","type":"show"}]}}`))
		case "/library/sections/m1/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"10","title":"Movie A","type":"movie"}]}}`))
		case "/library/sections/m2/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"10","title":"Movie A duplicate","type":"movie"}]}}`))
		case "/library/sections/t1/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"show","title":"Series","type":"show","thumb":"/show.jpg"}]}}`))
		case "/library/sections/t2/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"other","title":"Other Series","type":"show"}]}}`))
		case "/library/metadata/10":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
		case "/library/metadata/show/children":
			seasonCalls.Add(1)
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"type":"season","ratingKey":"s3","title":"Season 3","index":3},
				{"type":"season","ratingKey":"sp","title":"Specials","index":0},
				{"type":"season","ratingKey":"s2","title":"Season 2","index":2},
				{"type":"season","ratingKey":"s1","title":"Season 1","index":1}]}}`))
		case "/library/metadata/s1/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"type":"episode","ratingKey":"e2","title":"Second","index":2,"parentIndex":1,"grandparentRatingKey":"show","grandparentTitle":"Series"},
				{"type":"episode","ratingKey":"e1","title":"First","index":1,"parentIndex":1,"grandparentRatingKey":"show","grandparentTitle":"Series"}]}}`))
		case "/library/metadata/s2/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
		case "/library/metadata/s3/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"type":"episode","ratingKey":"e3","title":"Third-season premiere","index":1,"parentIndex":3,"grandparentRatingKey":"show","grandparentTitle":"Series"}]}}`))
		case "/library/metadata/sp/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"type":"episode","ratingKey":"x1","title":"Special 1","index":1,"parentIndex":0,"grandparentRatingKey":"show","grandparentTitle":"Series"},
				{"type":"episode","ratingKey":"x2","title":"Special 2","index":2,"parentIndex":0,"grandparentRatingKey":"show","grandparentTitle":"Series"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	cacheFile := filepath.Join(t.TempDir(), "library.json")
	p := NewPlex(srv.URL, "tok", cacheFile, nil)

	library, err := p.ListLibrary()
	if err != nil {
		t.Fatalf("ListLibrary: %v", err)
	}
	if len(library.Movies) != 1 || len(library.Shows) != 2 {
		t.Fatalf("library = %d movies/%d shows, want 1/2", len(library.Movies), len(library.Shows))
	}
	seasons, err := p.ListSeasons("show")
	if err != nil || len(seasons) != 4 || seasons[0].Index != 1 || seasons[2].Index != 3 || seasons[3].Index != 0 {
		t.Fatalf("seasons = %+v, err=%v", seasons, err)
	}
	episodes, err := p.ListEpisodes("s1")
	if err != nil || len(episodes) != 2 || episodes[0].RatingKey != "e1" {
		t.Fatalf("episodes = %+v, err=%v", episodes, err)
	}
	next, err := p.NextEpisode(episodes[1])
	if err != nil || next == nil || next.RatingKey != "e3" {
		t.Fatalf("next across empty season = %+v, err=%v", next, err)
	}
	specials, _ := p.ListEpisodes("sp")
	next, err = p.NextEpisode(specials[1])
	if err != nil || next != nil {
		t.Fatalf("final special next = %+v, err=%v; specials must not enter regular seasons", next, err)
	}
	if seasonCalls.Load() != 1 {
		t.Fatalf("season hierarchy fetched %d times, want 1", seasonCalls.Load())
	}

	srv.Close()
	restored := NewPlex(srv.URL, "tok", cacheFile, nil)
	if got, err := restored.ListEpisodes("s1"); err != nil || len(got) != 2 {
		t.Fatalf("disk fallback episodes = %+v, err=%v", got, err)
	}
}

func TestPlexTVHierarchySingleflight(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", "", nil)
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	ready.Add(8)
	begin := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-begin
			if _, err := p.ListSeasons("show"); err != nil {
				t.Errorf("ListSeasons: %v", err)
			}
		}()
	}
	ready.Wait()
	close(begin)
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent hierarchy calls = %d, want 1", calls.Load())
	}
}

func TestPlexTVHierarchyRefreshAndStaleFallback(t *testing.T) {
	var version atomic.Int64
	version.Store(1)
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "plex down", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/metadata/show/children":
			body := `{"MediaContainer":{"Metadata":[{"type":"season","ratingKey":"s1","title":"Season 1","index":1}`
			if version.Load() > 1 {
				body += `,{"type":"season","ratingKey":"s2","title":"Season 2","index":2}`
			}
			w.Write([]byte(body + `]}}`))
		case "/library/metadata/s1/children":
			body := `{"MediaContainer":{"Metadata":[{"type":"episode","ratingKey":"e1","title":"Pilot","index":1,"parentIndex":1}`
			if version.Load() > 1 {
				body += `,{"type":"episode","ratingKey":"e2","title":"Weekly","index":2,"parentIndex":1}`
			}
			w.Write([]byte(body + `]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", filepath.Join(t.TempDir(), "library.json"), nil)

	if seasons, err := p.ListSeasons("show"); err != nil || len(seasons) != 1 {
		t.Fatalf("prime seasons = %+v, err=%v", seasons, err)
	}
	if episodes, err := p.ListEpisodes("s1"); err != nil || len(episodes) != 1 {
		t.Fatalf("prime episodes = %+v, err=%v", episodes, err)
	}
	version.Store(2)
	if episodes, _ := p.ListEpisodes("s1"); len(episodes) != 1 {
		t.Fatalf("fresh hierarchy cache unexpectedly refreshed: %+v", episodes)
	}

	p.RefreshLibrary()
	if seasons, err := p.ListSeasons("show"); err != nil || len(seasons) != 2 {
		t.Fatalf("refreshed seasons = %+v, err=%v", seasons, err)
	}
	if episodes, err := p.ListEpisodes("s1"); err != nil || len(episodes) != 2 {
		t.Fatalf("refreshed episodes = %+v, err=%v", episodes, err)
	}

	failing.Store(true)
	p.RefreshLibrary()
	if seasons, err := p.ListSeasons("show"); err != nil || len(seasons) != 2 {
		t.Fatalf("stale season fallback = %+v, err=%v", seasons, err)
	}
	if episodes, err := p.ListEpisodes("s1"); err != nil || len(episodes) != 2 {
		t.Fatalf("stale episode fallback = %+v, err=%v", episodes, err)
	}
}

func TestPlexConcurrentCacheSavesRemainValid(t *testing.T) {
	cacheFile := filepath.Join(t.TempDir(), "library.json")
	p := NewPlex("http://127.0.0.1:1", "tok", cacheFile, nil)
	p.moviesMu.Lock()
	p.moviesAt = time.Now()
	p.moviesVal = []Movie{{RatingKey: "m1", Title: "Movie"}}
	p.showsVal = []Show{{RatingKey: "show", Title: "Series"}}
	p.showsLoaded = true
	p.seasonsVal = map[string][]Season{"show": {{RatingKey: "s1", ParentRatingKey: "show", Index: 1}}}
	p.episodesVal = map[string][]Episode{"s1": {{RatingKey: "e1", ParentRatingKey: "s1", EpisodeNumber: 1}}}
	p.seasonsAt = map[string]time.Time{"show": time.Now()}
	p.episodesAt = map[string]time.Time{"s1": time.Now()}
	p.moviesMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.saveCacheToDisk()
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var disk cachedLibrary
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatalf("concurrent cache save produced invalid JSON: %v\n%s", err, data)
	}
	if len(disk.Seasons["show"]) != 1 || len(disk.Episodes["s1"]) != 1 {
		t.Fatalf("concurrent cache save lost hierarchy: %+v", disk)
	}
}

func TestPlexLoadsV1MovieCacheThenFetchesFullLibrary(t *testing.T) {
	var sectionCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			sectionCalls.Add(1)
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"movies","type":"movie"},{"key":"tv","type":"show"}]}}`))
		case "/library/sections/movies/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"new","title":"New Movie"}]}}`))
		case "/library/sections/tv/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"show","title":"Series"}]}}`))
		case "/library/metadata/new":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cacheFile := filepath.Join(t.TempDir(), "library-cache.json")
	at := time.Now().UTC().Truncate(time.Second)
	data, err := json.Marshal(map[string]any{
		"at": at, "movies": []Movie{{RatingKey: "old", Title: "Cached Movie"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewPlex(srv.URL, "tok", cacheFile, nil)
	movies, err := p.ListMovies()
	if err != nil || len(movies) != 1 || movies[0].RatingKey != "old" {
		t.Fatalf("v1 cached movies = %+v, err=%v", movies, err)
	}
	p.moviesMu.Lock()
	showsLoaded := p.showsLoaded
	p.moviesMu.Unlock()
	if showsLoaded {
		t.Fatal("v1 cache incorrectly marked shows loaded")
	}
	library, err := p.ListLibrary()
	if err != nil || len(library.Shows) != 1 || library.Movies[0].RatingKey != "new" {
		t.Fatalf("full library after v1 cache = %+v, err=%v", library, err)
	}
	if sectionCalls.Load() != 1 {
		t.Fatalf("full catalog fetches = %d, want 1", sectionCalls.Load())
	}
}

func TestPlexLoadsLegacyTVCacheWithoutHierarchyTimestamps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "offline", http.StatusBadGateway)
	}))
	defer srv.Close()
	cacheFile := filepath.Join(t.TempDir(), "library-cache.json")
	at := time.Now().Add(-hierarchyCacheTTL - time.Minute).UTC().Truncate(time.Second)
	entry := cachedLibrary{
		Version: 2, At: at,
		Shows:    []Show{{RatingKey: "show", Title: "Series"}},
		Seasons:  map[string][]Season{"show": {{RatingKey: "s1", ParentRatingKey: "show", Index: 1}}},
		Episodes: map[string][]Episode{"s1": {{RatingKey: "e1", ParentRatingKey: "s1", GrandparentRatingKey: "show", EpisodeNumber: 1}}},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewPlex(srv.URL, "tok", cacheFile, nil)
	p.moviesMu.Lock()
	seasonAt, episodeAt := p.seasonsAt["show"], p.episodesAt["s1"]
	p.moviesMu.Unlock()
	if !seasonAt.Equal(at) || !episodeAt.Equal(at) {
		t.Fatalf("legacy timestamps = %s/%s, want %s", seasonAt, episodeAt, at)
	}
	if seasons, err := p.ListSeasons("show"); err != nil || len(seasons) != 1 {
		t.Fatalf("offline legacy seasons = %+v, err=%v", seasons, err)
	}
	if episodes, err := p.ListEpisodes("s1"); err != nil || len(episodes) != 1 {
		t.Fatalf("offline legacy episodes = %+v, err=%v", episodes, err)
	}
}

func TestPlexCatalogRefreshPrunesRemovedHierarchyAndTempFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"tv","type":"show"}]}}`))
		case "/library/sections/tv/all":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"keep","title":"Kept"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "library-cache.json")
	entry := cachedLibrary{
		Version: 2, At: time.Now().Add(-moviesCacheTTL - time.Minute),
		Shows: []Show{{RatingKey: "keep"}, {RatingKey: "gone"}},
		Seasons: map[string][]Season{
			"keep": {{RatingKey: "ks", ParentRatingKey: "keep"}},
			"gone": {{RatingKey: "gs", ParentRatingKey: "gone"}},
		},
		Episodes: map[string][]Episode{
			"ks": {{RatingKey: "ke", ParentRatingKey: "ks", GrandparentRatingKey: "keep"}},
			"gs": {{RatingKey: "ge", ParentRatingKey: "gs", GrandparentRatingKey: "gone"}},
		},
		SeasonsAt:  map[string]time.Time{"keep": time.Now(), "gone": time.Now()},
		EpisodesAt: map[string]time.Time{"ks": time.Now(), "gs": time.Now()},
	}
	data, _ := json.Marshal(entry)
	if err := os.WriteFile(cacheFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	debris := filepath.Join(dir, ".library-tmp-crash-debris")
	if err := os.WriteFile(debris, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	operatorBackup := filepath.Join(dir, ".library-manual.bak")
	if err := os.WriteFile(operatorBackup, []byte("operator backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewPlex(srv.URL, "tok", cacheFile, nil)
	if _, err := os.Stat(debris); !os.IsNotExist(err) {
		t.Fatalf("stale cache temp still exists: %v", err)
	}
	if data, err := os.ReadFile(operatorBackup); err != nil || string(data) != "operator backup" {
		t.Fatalf("operator-owned .library-* file was changed: %q, %v", data, err)
	}
	if _, err := p.ListLibrary(); err != nil {
		t.Fatalf("ListLibrary: %v", err)
	}
	p.moviesMu.Lock()
	_, goneSeasons := p.seasonsVal["gone"]
	_, goneEpisodes := p.episodesVal["gs"]
	_, keptSeasons := p.seasonsVal["keep"]
	_, keptEpisodes := p.episodesVal["ks"]
	p.moviesMu.Unlock()
	if goneSeasons || goneEpisodes || !keptSeasons || !keptEpisodes {
		t.Fatalf("pruned hierarchy wrong: gone=%v/%v kept=%v/%v",
			goneSeasons, goneEpisodes, keptSeasons, keptEpisodes)
	}
}

func TestPlexCatalogMembershipChecks(t *testing.T) {
	p := NewPlex("http://127.0.0.1:1", "tok", "", nil)
	p.moviesMu.Lock()
	p.showsByKey = buildShowsIndex([]Show{{RatingKey: "show"}})
	p.seasonsVal = map[string][]Season{"show": {{RatingKey: "s1", ParentRatingKey: "show"}}}
	p.rebuildHierarchyIndexesLocked()
	p.moviesMu.Unlock()
	if !p.HasShow("show") || p.HasShow("movie") {
		t.Fatal("show catalog membership check is incorrect")
	}
	if !p.HasSeason("s1") || p.HasSeason("album") {
		t.Fatal("season catalog membership check is incorrect")
	}
}

func TestResolveEpisodeMetadataAndRejectDirectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/show") {
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"type":"show","title":"Series","Media":[{"Part":[{"key":"/bad"}]}]}]}}`))
			return
		}
		w.Write([]byte(`{"MediaContainer":{"Metadata":[{
			"type":"episode","ratingKey":"e2","title":"The Episode","year":2026,
			"parentRatingKey":"s1","grandparentRatingKey":"show","grandparentTitle":"Series",
			"parentIndex":1,"index":2,"thumb":"/episode.jpg","duration":2700000,
			"Media":[{"videoCodec":"hevc"},{"videoCodec":"h264","optimizedForStreaming":1,
				"width":1920,"height":1080,"Part":[{"key":"/episode.mp4"}]}]
		}]}}`))
	}))
	defer srv.Close()
	p := NewPlex(srv.URL, "tok", "", nil)
	si, meta, err := p.Resolve("episode")
	if err != nil {
		t.Fatalf("Resolve episode: %v", err)
	}
	if meta.MediaType != "episode" || meta.SeriesTitle != "Series" ||
		meta.SeasonNumber != 1 || meta.EpisodeNumber != 2 || si.VideoCodec != "h264" {
		t.Fatalf("resolved episode si=%+v meta=%+v", si, meta)
	}
	if _, _, err := p.Resolve("show"); err == nil {
		t.Fatal("Resolve accepted a non-playable show")
	}
}

func newTVHubFixture(t *testing.T) *hubTestFixture {
	t.Helper()
	return newTVHubFixtureWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/video/:/transcode/universal/decision":
			w.Write([]byte(`{"MediaContainer":{}}`))
		case "/video/:/transcode/universal/start.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte("#EXTM3U\n#EXTINF:6,\nseg-0.ts\n"))
		case "/video/:/transcode/universal/stop":
			w.WriteHeader(http.StatusOK)
		case "/library/metadata/e1":
			w.Write([]byte(episodeMetadataJSON("e1", "Pilot", 1)))
		case "/library/metadata/e2":
			w.Write([]byte(episodeMetadataJSON("e2", "Second", 2)))
		case "/library/metadata/s1/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"type":"episode","ratingKey":"e1","title":"Pilot","index":1,"parentIndex":1,"grandparentRatingKey":"show","grandparentTitle":"Series"},
				{"type":"episode","ratingKey":"e2","title":"Second","index":2,"parentIndex":1,"grandparentRatingKey":"show","grandparentTitle":"Series"}]}}`))
		case "/library/metadata/show/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"type":"season","ratingKey":"s1","title":"Season 1","index":1}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func newTVHubFixtureWithHandler(t *testing.T, handler http.Handler) *hubTestFixture {
	t.Helper()
	mock := httptest.NewServer(handler)
	t.Cleanup(mock.Close)
	dir := t.TempDir()
	audit := NewAuditLog(filepath.Join(dir, "audit.jsonl"), auditCap)
	plex := NewPlex(mock.URL, "tok", filepath.Join(dir, "library.json"), audit)
	cache := NewSegmentCache(filepath.Join(dir, "cache"), 1<<30)
	recent := NewRecentMovies(filepath.Join(dir, "recent.json"))
	store := NewStateStore(filepath.Join(dir, "state.json"))
	hostStore := NewHostStore(filepath.Join(dir, "host.json"))
	hub := NewHub(plex, NewPlexSession(plex, 12000), cache, recent, store, hostStore,
		audit, NewAliasStore(filepath.Join(dir, "aliases.json")), nil)
	hub.mu.Lock()
	hub.clients[&clientEntry{id: "host", email: testHostEmail, name: "Tester", send: make(chan []byte, 8), kill: make(chan struct{})}] = struct{}{}
	hub.activeHost = testHostEmail
	hub.lastSavedHost = testHostEmail
	hub.mu.Unlock()
	t.Cleanup(hub.Close)
	return &hubTestFixture{hub: hub, mock: mock, cache: cache, dir: dir, hostStore: hostStore}
}

func episodeMetadataJSON(key, title string, episode int) string {
	return `{"MediaContainer":{"Metadata":[{
		"type":"episode","ratingKey":"` + key + `","title":"` + title + `",
		"parentRatingKey":"s1","grandparentRatingKey":"show","grandparentTitle":"Series",
		"parentIndex":1,"index":` + strconv.Itoa(episode) + `,"duration":1800000,"thumb":"/ep.jpg",
		"Media":[{"videoCodec":"h264","width":1920,"height":1080,"Part":[{"key":"/p"}]}]
	}]}}`
}

func TestHubExplicitNextEpisode(t *testing.T) {
	f := newTVHubFixture(t)
	if w := f.post(t, `{"action":"load","ratingKey":"e1"}`); w.Code != http.StatusOK {
		t.Fatalf("load episode: %d %s", w.Code, w.Body.String())
	}
	first := f.hub.Snapshot()
	if first.MediaType != "episode" || first.NextEpisode == nil || first.NextEpisode.RatingKey != "e2" {
		t.Fatalf("first episode state = %+v", first)
	}
	if first.Playing {
		t.Fatal("ordinary episode selection must start paused")
	}
	f.hub.store.Wait()
	if w := f.post(t, `{"action":"next"}`); w.Code != http.StatusOK {
		t.Fatalf("next: %d %s", w.Code, w.Body.String())
	}
	second := f.hub.Snapshot()
	if second.RatingKey != "e2" || !second.Playing || second.NextEpisode != nil {
		t.Fatalf("next episode state = %+v", second)
	}
	recent := f.hub.recent.List()
	if len(recent) == 0 || recent[0].MediaType != "episode" || recent[0].EpisodeNumber != 2 {
		t.Fatalf("recent episode = %+v", recent)
	}
	f.hub.store.Wait()
	hint := f.hub.store.Load()
	if hint == nil || hint.MediaType != "episode" || hint.EpisodeNumber != 2 {
		t.Fatalf("resume hint = %+v", hint)
	}
	if w := f.post(t, `{"action":"next"}`); w.Code != http.StatusConflict {
		t.Fatalf("next on final episode = %d, want 409", w.Code)
	}
}

func TestEpisodeReuseRetriesMissingNextSummary(t *testing.T) {
	var hierarchyDown atomic.Bool
	hierarchyDown.Store(true)
	f := newTVHubFixtureWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/video/:/transcode/universal/decision":
			w.Write([]byte(`{"MediaContainer":{}}`))
		case "/video/:/transcode/universal/start.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte("#EXTM3U\n#EXTINF:6,\nseg-0.ts\n"))
		case "/video/:/transcode/universal/stop":
			w.WriteHeader(http.StatusOK)
		case "/library/metadata/e1":
			w.Write([]byte(episodeMetadataJSON("e1", "Pilot", 1)))
		case "/library/metadata/s1/children":
			if hierarchyDown.Load() {
				http.Error(w, "temporary hierarchy failure", http.StatusBadGateway)
				return
			}
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"type":"episode","ratingKey":"e1","title":"Pilot","index":1,"parentIndex":1,"grandparentRatingKey":"show","grandparentTitle":"Series"},
				{"type":"episode","ratingKey":"e2","title":"Second","index":2,"parentIndex":1,"grandparentRatingKey":"show","grandparentTitle":"Series"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	if w := f.post(t, `{"action":"load","ratingKey":"e1"}`); w.Code != http.StatusOK {
		t.Fatalf("initial load: %d %s", w.Code, w.Body.String())
	}
	first := f.hub.Snapshot()
	if first.NextEpisode != nil {
		t.Fatalf("next summary survived failed hierarchy lookup: %+v", first.NextEpisode)
	}
	token := first.SessionToken
	hierarchyDown.Store(false)
	if w := f.post(t, `{"action":"load","ratingKey":"e1"}`); w.Code != http.StatusOK {
		t.Fatalf("reuse load: %d %s", w.Code, w.Body.String())
	}
	retried := f.hub.Snapshot()
	if retried.NextEpisode == nil || retried.NextEpisode.RatingKey != "e2" {
		t.Fatalf("next summary was not recovered on reuse: %+v", retried.NextEpisode)
	}
	if retried.SessionToken != token {
		t.Fatalf("reuse restarted transcode session: token %d -> %d", token, retried.SessionToken)
	}
}

func TestNextEpisodeRequiresActiveHost(t *testing.T) {
	f := newTVHubFixture(t)
	f.post(t, `{"action":"load","ratingKey":"e1"}`)
	r := httptest.NewRequest(http.MethodPost, "/control", bytes.NewBufferString(`{"action":"next"}`))
	r = r.WithContext(context.WithValue(r.Context(), actorCtxKey{}, "viewer@example.com"))
	w := httptest.NewRecorder()
	f.hub.HandleControl(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer next status = %d, want 403", w.Code)
	}
}

func TestNextEpisodeRejectsConcurrentAdvance(t *testing.T) {
	f := newTVHubFixture(t)
	f.post(t, `{"action":"load","ratingKey":"e1"}`)
	f.hub.nextMu.Lock()
	f.hub.nextInFlight = true
	f.hub.nextMu.Unlock()
	defer func() {
		f.hub.nextMu.Lock()
		f.hub.nextInFlight = false
		f.hub.nextMu.Unlock()
	}()
	if w := f.post(t, `{"action":"next"}`); w.Code != http.StatusConflict {
		t.Fatalf("concurrent next status = %d, want 409", w.Code)
	}
	if got := f.hub.Snapshot().RatingKey; got != "e1" {
		t.Fatalf("concurrent next changed current episode to %q", got)
	}
}

func TestNextEpisodeRejectsOverlappingRequestEndToEnd(t *testing.T) {
	var starts, stops atomic.Int64
	nextResolveStarted := make(chan struct{})
	releaseNextResolve := make(chan struct{})
	var signalOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseNextResolve) }) }
	defer release()
	f := newTVHubFixtureWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/video/:/transcode/universal/decision":
			w.Write([]byte(`{"MediaContainer":{}}`))
		case "/video/:/transcode/universal/start.m3u8":
			starts.Add(1)
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte("#EXTM3U\n#EXTINF:6,\nseg-0.ts\n"))
		case "/video/:/transcode/universal/stop":
			stops.Add(1)
			w.WriteHeader(http.StatusOK)
		case "/library/metadata/e1":
			w.Write([]byte(episodeMetadataJSON("e1", "Pilot", 1)))
		case "/library/metadata/e2":
			signalOnce.Do(func() { close(nextResolveStarted) })
			<-releaseNextResolve
			w.Write([]byte(episodeMetadataJSON("e2", "Second", 2)))
		case "/library/metadata/s1/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"type":"episode","ratingKey":"e1","title":"Pilot","index":1,"parentIndex":1,"grandparentRatingKey":"show","grandparentTitle":"Series"},
				{"type":"episode","ratingKey":"e2","title":"Second","index":2,"parentIndex":1,"grandparentRatingKey":"show","grandparentTitle":"Series"}]}}`))
		case "/library/metadata/show/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"type":"season","ratingKey":"s1","title":"Season 1","index":1}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	if w := f.post(t, `{"action":"load","ratingKey":"e1"}`); w.Code != http.StatusOK {
		t.Fatalf("initial load: %d %s", w.Code, w.Body.String())
	}
	firstDone := make(chan int, 1)
	go func() {
		firstDone <- f.post(t, `{"action":"next","ratingKey":"e1"}`).Code
	}()
	select {
	case <-nextResolveStarted:
	case <-time.After(time.Second):
		t.Fatal("first next request did not reach slow Plex resolve")
	}
	secondDone := make(chan int, 1)
	go func() {
		secondDone <- f.post(t, `{"action":"next","ratingKey":"e1"}`).Code
	}()
	select {
	case status := <-secondDone:
		if status != http.StatusConflict {
			t.Fatalf("overlapping next status = %d, want 409", status)
		}
	case <-time.After(time.Second):
		t.Fatal("overlapping next request did not return while first was blocked")
	}
	release()
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("first next status = %d, want 200", status)
	}
	if starts.Load() != 2 || stops.Load() != 1 {
		t.Fatalf("transcode lifecycle starts/stops = %d/%d, want 2/1 including initial start",
			starts.Load(), stops.Load())
	}
	if got := f.hub.Snapshot().RatingKey; got != "e2" {
		t.Fatalf("current episode = %q, want e2", got)
	}
}

func TestNextEpisodeRejectsStaleSource(t *testing.T) {
	f := newTVHubFixture(t)
	f.post(t, `{"action":"load","ratingKey":"e1"}`)
	if w := f.post(t, `{"action":"next","ratingKey":"older-episode"}`); w.Code != http.StatusConflict {
		t.Fatalf("stale next status = %d, want 409", w.Code)
	}
	if got := f.hub.Snapshot().RatingKey; got != "e1" {
		t.Fatalf("stale next changed current episode to %q", got)
	}
}

func TestDiscordEpisodeOmitsMovieLinks(t *testing.T) {
	ev := notifyEvent{
		Kind: notifyStart, MediaType: "episode", Title: "Pilot", SeriesTitle: "Series",
		SeasonNumber: 1, EpisodeNumber: 2, TMDBID: "123",
		NextEpisode: &NextEpisodeSummary{
			RatingKey: "e3", Title: "Third", SeriesTitle: "Series",
			SeasonNumber: 1, EpisodeNumber: 3,
		},
	}
	embed := buildPayload(ev, "https://watch.example").Embeds[0]
	if embed.Title != "Series · S01E02 · Pilot" {
		t.Fatalf("episode embed title = %q", embed.Title)
	}
	for _, field := range embed.Fields {
		if field.Name == "Links" && (strings.Contains(field.Value, "Rotten") || strings.Contains(field.Value, "TMDB")) {
			t.Fatalf("episode carried movie-only links: %q", field.Value)
		}
	}
	if upNext, ok := findField(embed.Fields, "Up Next"); !ok || upNext != "Series · S01E03 · Third" {
		t.Fatalf("episode Up Next field = %q, %v", upNext, ok)
	}
}
