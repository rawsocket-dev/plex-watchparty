package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// gatedListing is a one-section Plex whose listing holds the given keys and
// whose /library/metadata batch call waits on gate (nil = answer at once),
// tracking how many batch calls are in flight at the same time.
type gatedListing struct {
	mu          sync.Mutex
	keys        []string
	gate        chan struct{}
	delay       time.Duration
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	requested   []string
}

func (g *gatedListing) setKeys(keys ...string) {
	g.mu.Lock()
	g.keys = keys
	g.mu.Unlock()
}

func (g *gatedListing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/library/sections":
		w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie","title":"Movies"}]}}`))
	case r.URL.Path == "/library/sections/1/all":
		g.mu.Lock()
		var items []string
		for _, k := range g.keys {
			items = append(items, `{"ratingKey":"`+k+`","title":"T`+k+`","year":2000}`)
		}
		g.mu.Unlock()
		w.Write([]byte(`{"MediaContainer":{"Metadata":[` + strings.Join(items, ",") + `]}}`))
	case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
		n := g.inFlight.Add(1)
		defer g.inFlight.Add(-1)
		for {
			m := g.maxInFlight.Load()
			if n <= m || g.maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		g.mu.Lock()
		g.requested = append(g.requested, strings.Split(strings.TrimPrefix(r.URL.Path, "/library/metadata/"), ",")...)
		gate := g.gate
		g.mu.Unlock()
		if gate != nil {
			<-gate
		}
		time.Sleep(g.delay)
		metadataBatchHandler(nil, &[]string{}, &sync.Mutex{})(w, r)
	default:
		http.NotFound(w, r)
	}
}

func imdbOf(p *Plex, key string) float64 {
	m, _ := p.MovieByKey(key)
	return m.IMDbRating
}

// Plex can take ~1 s per title the first time it serves a title's metadata
// in a while, so a refresh with new titles used to hold the library page
// (and the admin refresh) for as long as the IMDb lookups took. The listing
// must come back without waiting; the scores land afterwards, in memory
// and in the on-disk cache.
func TestLibraryListingDoesNotWaitForIMDbLookups(t *testing.T) {
	g := &gatedListing{gate: make(chan struct{})}
	g.setKeys("10", "11")
	srv := httptest.NewServer(g)
	defer srv.Close()
	defer func() {
		select {
		case <-g.gate:
		default:
			close(g.gate)
		}
	}()
	cache := filepath.Join(t.TempDir(), "lib.json")
	p := newTestPlex(t, srv.URL, "tok", cache, nil)

	type result struct {
		lib MediaLibrary
		err error
	}
	got := make(chan result, 1)
	go func() {
		lib, err := p.ListLibrary()
		got <- result{lib, err}
	}()
	select {
	case r := <-got:
		if r.err != nil || len(r.lib.Movies) != 2 {
			t.Fatalf("ListLibrary = %d movies, err %v", len(r.lib.Movies), r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListLibrary is still waiting on the IMDb lookups")
	}

	close(g.gate)
	if !eventually(t, 3*time.Second, func() bool { return imdbOf(p, "10") == 5.9 && imdbOf(p, "11") == 6.3 }) {
		t.Fatalf("IMDb scores never landed: 10=%v 11=%v", imdbOf(p, "10"), imdbOf(p, "11"))
	}
	// Persisted: a fresh process reading the same cache file starts with them.
	if !eventually(t, 3*time.Second, func() bool {
		return imdbOf(NewPlex(srv.URL, "tok", cache, nil), "10") == 5.9
	}) {
		t.Error("enriched IMDb score was not saved to the library cache file")
	}
}

// Plex's first answer for cold titles is slower than the 15 s that bounds
// ordinary Plex calls (107 s measured for 127 titles). That is Plex working,
// not Plex down: the lookup must wait it out, and must not flip Plex to
// unhealthy (which audits a false "plex unreachable").
func TestIMDbLookupOutlastsSlowFirstTouch(t *testing.T) {
	g := &gatedListing{delay: 400 * time.Millisecond}
	g.setKeys("10")
	srv := httptest.NewServer(g)
	defer srv.Close()
	p := newTestPlex(t, srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)
	p.http.Timeout = 150 * time.Millisecond // stands in for the 15 s default

	if _, err := p.ListLibrary(); err != nil {
		t.Fatalf("ListLibrary: %v", err)
	}
	if !eventually(t, 3*time.Second, func() bool { return imdbOf(p, "10") == 5.9 }) {
		t.Errorf("slow metadata answer was abandoned: IMDb = %v, want 5.9", imdbOf(p, "10"))
	}
	if !p.IsHealthy() {
		t.Error("a slow IMDb lookup marked Plex unhealthy")
	}
}

// A refresh that lands while lookups from the previous one are still
// running must neither wait for them nor start a second, parallel batch
// stream against Plex — and the titles it adds still get their scores.
func TestRefreshDuringIMDbLookupsQueuesNewTitles(t *testing.T) {
	g := &gatedListing{gate: make(chan struct{})}
	g.setKeys("10")
	srv := httptest.NewServer(g)
	defer srv.Close()
	defer func() {
		select {
		case <-g.gate:
		default:
			close(g.gate)
		}
	}()
	p := newTestPlex(t, srv.URL, "tok", filepath.Join(t.TempDir(), "lib.json"), nil)

	done := make(chan error, 1)
	go func() { _, err := p.ListLibrary(); done <- err }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first ListLibrary is waiting on the IMDb lookups")
	}
	if !eventually(t, 2*time.Second, func() bool { return g.inFlight.Load() == 1 }) {
		t.Fatal("first lookup batch never reached Plex")
	}

	g.setKeys("10", "11")
	p.RefreshLibrary()
	go func() { _, err := p.ListLibrary(); done <- err }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh is waiting on the previous refresh's IMDb lookups")
	}

	close(g.gate)
	if !eventually(t, 3*time.Second, func() bool { return imdbOf(p, "10") == 5.9 && imdbOf(p, "11") == 6.3 }) {
		t.Fatalf("scores after both refreshes: 10=%v 11=%v", imdbOf(p, "10"), imdbOf(p, "11"))
	}
	if m := g.maxInFlight.Load(); m != 1 {
		t.Errorf("%d lookup batches ran against Plex at once, want 1", m)
	}
}
