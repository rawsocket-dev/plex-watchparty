package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Plex talks to a Plex Media Server using a server-side token.
// The token NEVER leaves this process — clients only ever see HLS via our proxy.
type Plex struct {
	BaseURL string // e.g. http://192.168.1.10:32400
	Token   string
	http    *http.Client

	// ListMovies cache: walking every movie section costs several seconds
	// on a large library. The cache is held in memory for the TTL below,
	// and (when cacheFile is non-empty) also persisted to disk so a
	// container restart doesn't pay the cold-start cost. listFlight
	// collapses concurrent TTL-miss refreshes into one upstream walk.
	moviesMu      sync.Mutex
	moviesAt      time.Time
	moviesVal     []Movie
	moviesByKey   map[string]Movie // O(1) ratingKey lookup, refreshed alongside moviesVal
	showsVal      []Show
	showsByKey    map[string]Show
	showsLoaded   bool
	seasonsVal    map[string][]Season  // show ratingKey -> seasons
	episodesVal   map[string][]Episode // season ratingKey -> episodes
	seasonsAt     map[string]time.Time
	episodesAt    map[string]time.Time
	seasonsByKey  map[string]Season
	episodesByKey map[string]Episode
	cacheFile     string
	cacheWriteMu  sync.Mutex
	listFlight    singleflight.Group
	// imdbNoRating remembers titles whose metadata batch came back with
	// no imdb:// entry, so TTL-expiry refreshes don't re-fetch the same
	// unrated titles forever. Cleared by RefreshLibrary — an explicit
	// admin refresh re-checks everything. Guarded by moviesMu.
	imdbNoRating map[string]bool

	// Health state. healthy is the latest known reachability; if any
	// call into Plex returns a transport error we flip it false and
	// kick off a background recovery loop that pings with exponential
	// backoff until Plex answers again. pingerActive prevents a flood
	// of concurrent recovery goroutines when many calls fail in quick
	// succession.
	healthMu     sync.Mutex
	healthy      bool
	pingerActive bool

	audit *AuditLog
}

// 30 minutes is long enough that a typical watch-party session (browsing
// the library, picking a movie, restarting the container if needed)
// never sees a Plex round-trip after the first call; short enough that
// newly-added content shows up within the same evening.
const moviesCacheTTL = 30 * time.Minute
const hierarchyCacheTTL = moviesCacheTTL
const libraryCacheTempPattern = ".library-tmp-*"

func NewPlex(baseURL, token, cacheFile string, audit *AuditLog) *Plex {
	// Plex Media Server's TLS certificate is only valid for hostnames
	// under *.<machine-id>.plex.direct (the auto-generated cert that
	// Plex.tv signs and ships down to each server). Any operator who
	// fronts Plex with their own DNS name (plex.example.com, etc.) or
	// hits it through a reverse proxy will fail standard verification.
	// Plex traffic is private LAN-side anyway and the token in the
	// query string is the real auth — skip verification here to make
	// the integration usable in realistic home setups.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	p := &Plex{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		Token:        token,
		http:         &http.Client{Timeout: 15 * time.Second, Transport: tr},
		cacheFile:    cacheFile,
		healthy:      true, // optimistic; startup Ping in main flips this if Plex is down
		audit:        audit,
		imdbNoRating: make(map[string]bool),
		seasonsAt:    make(map[string]time.Time),
		episodesAt:   make(map[string]time.Time),
	}
	p.loadCacheFromDisk()
	return p
}

// Do is the canonical way to issue a Plex HTTP request. It runs the
// request through Plex's own http.Client and, if the call fails at
// the transport level (DNS, connection refused, timeout, TLS handshake,
// etc.), trips the health-recovery loop. Status-code failures don't
// count — those are application errors, not connectivity errors.
func (p *Plex) Do(req *http.Request) (*http.Response, error) {
	resp, err := p.http.Do(req)
	if err != nil {
		p.MarkUnhealthy(err)
	}
	return resp, err
}

// IsHealthy reports the most recent reachability state.
func (p *Plex) IsHealthy() bool {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	return p.healthy
}

// MarkUnhealthy flips Plex into the unhealthy state and ensures the
// recovery loop is running. Safe to call from anywhere — duplicate
// calls while a pinger is already running are a no-op.
func (p *Plex) MarkUnhealthy(err error) {
	p.healthMu.Lock()
	wasHealthy := p.healthy
	p.healthy = false
	startPinger := !p.pingerActive
	if startPinger {
		p.pingerActive = true
	}
	p.healthMu.Unlock()
	if wasHealthy {
		log.Printf("plex: marking unhealthy: %v", err)
		p.audit.Record(AuditEvent{Type: "plex", Email: "system", Detail: fmt.Sprintf("plex unreachable: %v", err)})
	}
	if startPinger {
		go p.healthRecoveryLoop()
	}
}

// healthRecoveryLoop pings Plex with exponential backoff (5s → 60s
// cap) until it answers, then flips healthy true and exits. Always
// runs in a goroutine and at most one is active at a time, gated by
// pingerActive.
func (p *Plex) healthRecoveryLoop() {
	delay := 5 * time.Second
	const maxDelay = 60 * time.Second
	for attempt := 1; ; attempt++ {
		id, err := p.Ping()
		if err == nil {
			p.healthMu.Lock()
			p.healthy = true
			p.pingerActive = false
			p.healthMu.Unlock()
			log.Printf("plex: recovered on attempt %d — connected to %q (version %s, machine %s)",
				attempt, id.FriendlyName, id.Version, id.MachineIdentifier)
			p.audit.Record(AuditEvent{Type: "plex", Email: "system", Detail: fmt.Sprintf("plex recovered (connected to %q)", id.FriendlyName)})
			return
		}
		log.Printf("plex: recovery attempt %d failed (%v); retry in %s", attempt, err, delay)
		time.Sleep(delay)
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

type Movie struct {
	RatingKey      string  `json:"ratingKey"`
	Title          string  `json:"title"`
	Year           int     `json:"year"`
	Rating         float64 `json:"rating,omitempty"`         // Plex critic "rating" (0–10), 0 = absent
	AudienceRating float64 `json:"audienceRating,omitempty"` // Plex "audienceRating" (0–10), 0 = absent
	// IMDbRating is the IMDb score (0–10), pulled from the per-movie Rating
	// array by enrichIMDb — the listing endpoint doesn't carry it. 0 = absent
	// or not yet enriched. Persisted in the disk cache and carried forward
	// across refreshes so steady-state refreshes don't re-fetch every title.
	IMDbRating float64 `json:"imdbRating,omitempty"`
	// Thumb is the Plex poster path (e.g. "/library/metadata/55/thumb/123").
	// Carried so the poster handler can skip a per-image /library/metadata
	// round-trip. Persisted in the on-disk library cache; harmless to the
	// front end (which addresses posters by ratingKey, not this path).
	Thumb string `json:"thumb,omitempty"`
}

// Show, Season, and Episode are the media-neutral catalog records sent to the
// browser. Hierarchy results are cached on demand; all fields are optional on
// disk so older movie-only library caches remain readable.
type Show struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	Year      int    `json:"year,omitempty"`
	Thumb     string `json:"thumb,omitempty"`
	Art       string `json:"art,omitempty"`
}

type Season struct {
	RatingKey       string `json:"ratingKey"`
	ParentRatingKey string `json:"parentRatingKey,omitempty"`
	Title           string `json:"title"`
	Index           int    `json:"index"`
	Thumb           string `json:"thumb,omitempty"`
	Art             string `json:"art,omitempty"`
}

type Episode struct {
	RatingKey             string `json:"ratingKey"`
	ParentRatingKey       string `json:"parentRatingKey,omitempty"`
	GrandparentRatingKey  string `json:"grandparentRatingKey,omitempty"`
	Title                 string `json:"title"`
	SeriesTitle           string `json:"seriesTitle,omitempty"`
	SeasonNumber          int    `json:"seasonNumber"`
	EpisodeNumber         int    `json:"episodeNumber"`
	Duration              int64  `json:"duration,omitempty"` // milliseconds
	OriginallyAvailableAt string `json:"originallyAvailableAt,omitempty"`
	Thumb                 string `json:"thumb,omitempty"`
	Art                   string `json:"art,omitempty"`
}

type MediaLibrary struct {
	Movies []Movie `json:"movies"`
	Shows  []Show  `json:"shows"`
}

// StreamInfo describes a movie's source metadata in enough detail to log
// what's about to play. Playback itself goes through Plex's Universal
// Transcoder via PlexSession — we don't touch the source URL ourselves.
type StreamInfo struct {
	VideoCodec   string // "h264", "hevc", ...
	VideoProfile string // "Main 10", "High", ...
	Width        int
	Height       int
	Bitrate      int    // kbps, total file
	FrameRate    string // "24p", "60p", ...
	Duration     int64  // ms
}

// MovieMeta carries the human-facing movie metadata used to build a rich
// Discord "Now Playing" embed: tagline/plot, content + audience ratings,
// genres, and the external IDs we turn into IMDb / TMDB links.
type MovieMeta struct {
	MediaType            string
	RatingKey            string
	Title                string
	Year                 int
	Thumb                string
	Art                  string
	SeriesTitle          string
	SeasonNumber         int
	EpisodeNumber        int
	ParentRatingKey      string
	GrandparentRatingKey string
	Tagline              string
	Summary              string
	ContentRating        string  // "PG", "R", ...
	CriticRating         float64 // Plex "rating" (0–10)
	AudienceRating       float64 // Plex "audienceRating" (0–10)
	Genres               []string
	IMDbID               string // e.g. "tt0089886" ("" if Plex has none)
	TMDBID               string // e.g. "14370" ("" if Plex has none)
}

// ServerIdentity is the subset of Plex's root response we care about
// for a startup health check.
type ServerIdentity struct {
	FriendlyName      string `json:"friendlyName"`
	MachineIdentifier string `json:"machineIdentifier"`
	Version           string `json:"version"`
	Platform          string `json:"platform"`
	PlatformVersion   string `json:"platformVersion"`
}

// Ping hits the Plex root endpoint with the configured token. Verifies
// (a) the server is reachable, (b) the token is valid, and (c) returns
// enough identity to log a "talking to <server>, version <X>" line at
// startup. A non-nil error means one of those checks failed.
func (p *Plex) Ping() (*ServerIdentity, error) {
	var resp struct {
		MediaContainer ServerIdentity `json:"MediaContainer"`
	}
	if err := p.get("/", &resp); err != nil {
		return nil, err
	}
	if resp.MediaContainer.MachineIdentifier == "" {
		return nil, fmt.Errorf("plex returned an empty identity (token may be invalid)")
	}
	return &resp.MediaContainer, nil
}

func (p *Plex) get(path string, v any) error {
	u := p.BaseURL + path
	if strings.Contains(path, "?") {
		u += "&"
	} else {
		u += "?"
	}
	u += "X-Plex-Token=" + url.QueryEscape(p.Token)

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plex %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type sectionsResp struct {
	MediaContainer struct {
		Directory []struct {
			Key  string `json:"key"`
			Type string `json:"type"`
		} `json:"Directory"`
	} `json:"MediaContainer"`
}

type libraryResp struct {
	MediaContainer struct {
		Metadata []struct {
			Type           string  `json:"type"`
			RatingKey      string  `json:"ratingKey"`
			Title          string  `json:"title"`
			Year           int     `json:"year"`
			Thumb          string  `json:"thumb"`
			Art            string  `json:"art"`
			Rating         float64 `json:"rating"`
			AudienceRating float64 `json:"audienceRating"`
			// The listing endpoint sends only scalar rating/audienceRating
			// (no capital arrays), unlike /library/metadata. This absorber is
			// defense-in-depth: if Plex ever adds a capital "Rating" array
			// here too, give it an exact-case home so Go's case-insensitive
			// json matching can't misroute it into the float above and fail
			// the whole decode (the collision that 502'd every load).
			RatingArray json.RawMessage `json:"Rating"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

// cachedLibrary is the on-disk shape of the library cache.
type cachedLibrary struct {
	Version    int                  `json:"version,omitempty"`
	At         time.Time            `json:"at"`
	Movies     []Movie              `json:"movies"`
	Shows      []Show               `json:"shows,omitempty"`
	Seasons    map[string][]Season  `json:"seasons,omitempty"`
	Episodes   map[string][]Episode `json:"episodes,omitempty"`
	SeasonsAt  map[string]time.Time `json:"seasonsAt,omitempty"`
	EpisodesAt map[string]time.Time `json:"episodesAt,omitempty"`
}

func (p *Plex) loadCacheFromDisk() {
	if p.cacheFile == "" {
		return
	}
	// A process can die after CreateTemp and before Rename. Those files are
	// never useful on restart, and without a sweep they accumulate forever.
	for _, tmpName := range cacheTempFiles(p.cacheFile) {
		if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("library cache: remove stale temp %s: %v", tmpName, err)
		}
	}
	data, err := os.ReadFile(p.cacheFile)
	if err != nil {
		return // missing/unreadable is fine — first ListMovies will populate
	}
	var entry cachedLibrary
	if err := json.Unmarshal(data, &entry); err != nil {
		log.Printf("library cache: parse %s: %v", p.cacheFile, err)
		return
	}
	p.moviesMu.Lock()
	p.moviesVal = entry.Movies
	p.showsVal = entry.Shows
	p.showsLoaded = entry.Version >= 2
	p.moviesAt = entry.At
	p.moviesByKey = buildMoviesIndex(entry.Movies)
	p.showsByKey = buildShowsIndex(entry.Shows)
	p.seasonsVal = entry.Seasons
	p.episodesVal = entry.Episodes
	legacySeasonTimes := entry.SeasonsAt == nil
	legacyEpisodeTimes := entry.EpisodesAt == nil
	p.seasonsAt = entry.SeasonsAt
	p.episodesAt = entry.EpisodesAt
	if p.seasonsAt == nil {
		p.seasonsAt = make(map[string]time.Time)
	}
	if p.episodesAt == nil {
		p.episodesAt = make(map[string]time.Time)
	}
	// Caches written by the first TV-capable release did not carry
	// per-hierarchy timestamps. Treat their values as fresh as of the
	// catalog save time, preserving restart/offline behavior while still
	// allowing them to expire normally.
	for key := range p.seasonsVal {
		if legacySeasonTimes {
			p.seasonsAt[key] = entry.At
		}
	}
	for key := range p.episodesVal {
		if legacyEpisodeTimes {
			p.episodesAt[key] = entry.At
		}
	}
	p.rebuildHierarchyIndexesLocked()
	p.moviesMu.Unlock()
	log.Printf("library cache: loaded %d movies and %d shows from %s (saved %s)",
		len(entry.Movies), len(entry.Shows), p.cacheFile,
		time.Since(entry.At).Round(time.Second))
}

func cacheTempFiles(cacheFile string) []string {
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(cacheFile), libraryCacheTempPattern))
	if err != nil {
		return nil
	}
	out := matches[:0]
	for _, match := range matches {
		// A custom cache filename may itself begin with ".library-".
		if filepath.Clean(match) != filepath.Clean(cacheFile) {
			out = append(out, match)
		}
	}
	return out
}

func (p *Plex) saveCacheToDisk() {
	if p.cacheFile == "" {
		return
	}
	p.cacheWriteMu.Lock()
	defer p.cacheWriteMu.Unlock()
	p.moviesMu.Lock()
	seasons := make(map[string][]Season, len(p.seasonsVal))
	for key, values := range p.seasonsVal {
		seasons[key] = append([]Season(nil), values...)
	}
	episodes := make(map[string][]Episode, len(p.episodesVal))
	for key, values := range p.episodesVal {
		episodes[key] = append([]Episode(nil), values...)
	}
	entry := cachedLibrary{
		At: p.moviesAt, Movies: append([]Movie(nil), p.moviesVal...), Shows: append([]Show(nil), p.showsVal...),
		Seasons: seasons, Episodes: episodes,
		SeasonsAt: cloneTimes(p.seasonsAt), EpisodesAt: cloneTimes(p.episodesAt),
	}
	if p.showsLoaded {
		entry.Version = 2
	}
	p.moviesMu.Unlock()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.cacheFile), 0o755); err != nil {
		log.Printf("library cache: mkdir: %v", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.cacheFile), libraryCacheTempPattern)
	if err != nil {
		log.Printf("library cache: temp: %v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		log.Printf("library cache: write %s: %v", tmpName, err)
		return
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		log.Printf("library cache: chmod %s: %v", tmpName, err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		log.Printf("library cache: close %s: %v", tmpName, err)
		return
	}
	if err := os.Rename(tmpName, p.cacheFile); err != nil {
		_ = os.Remove(tmpName)
		log.Printf("library cache: rename: %v", err)
	}
}

func cloneTimes(src map[string]time.Time) map[string]time.Time {
	dst := make(map[string]time.Time, len(src))
	for key, at := range src {
		dst[key] = at
	}
	return dst
}

// LibraryStats is the snapshot of the in-memory library cache used by
// the admin panel.
type LibraryStats struct {
	Titles     int       `json:"titles"`
	CachedAt   time.Time `json:"cachedAt"`
	AgeSec     float64   `json:"ageSec"`
	Healthy    bool      `json:"healthy"`
	Identifier string    `json:"identifier"`
}

// Stats returns a snapshot of the library cache + current Plex
// health. Used by /admin/api/stats.
func (p *Plex) Stats() LibraryStats {
	p.moviesMu.Lock()
	titles := len(p.moviesVal) + len(p.showsVal)
	at := p.moviesAt
	p.moviesMu.Unlock()
	age := 0.0
	if !at.IsZero() {
		age = time.Since(at).Seconds()
	}
	return LibraryStats{
		Titles:   titles,
		CachedAt: at,
		AgeSec:   age,
		Healthy:  p.IsHealthy(),
	}
}

// RefreshLibrary invalidates the in-memory library cache so the next
// ListMovies call hits Plex and repopulates. The persisted disk cache
// is left alone — if Plex is currently down, ListMovies will still
// return the old slice rather than failing, because moviesVal isn't
// cleared, just moviesAt is rewound past the TTL.
func (p *Plex) RefreshLibrary() {
	p.moviesMu.Lock()
	p.moviesAt = time.Time{} // forces TTL check to miss on next ListMovies
	for key := range p.seasonsVal {
		p.seasonsAt[key] = time.Time{}
	}
	for key := range p.episodesVal {
		p.episodesAt[key] = time.Time{}
	}
	// An explicit refresh also forgets which titles were unrated, so the
	// next enrichment gives every scoreless title a fresh look.
	p.imdbNoRating = make(map[string]bool)
	p.moviesMu.Unlock()
	// Persist the invalidated timestamps so a restart cannot accidentally
	// make stale hierarchy data fresh again. Values stay in the cache and
	// remain available as fallback if Plex is down.
	p.saveCacheToDisk()
	log.Printf("library: catalog and TV hierarchy caches invalidated; next request will refetch from Plex")
}

// ListMovies returns every item across all movie-type library sections.
// Cached in-memory + on disk for `moviesCacheTTL`. Concurrent TTL-miss
// callers collapse into a single upstream refresh, and a refresh that
// fails outright (Plex down) falls back to the previous cached list —
// stale titles beat an empty library page. The fallback doesn't advance
// moviesAt, so the admin panel's cache age keeps growing (a visible
// signal) and the next call retries Plex.
func (p *Plex) ListMovies() ([]Movie, error) {
	if movies, ok := p.cachedMovies(); ok {
		return movies, nil
	}
	p.moviesMu.Lock()
	haveFullCatalog := p.showsLoaded
	p.moviesMu.Unlock()
	if haveFullCatalog {
		lib, err := p.ListLibrary()
		return lib.Movies, err
	}
	v, err, _ := p.listFlight.Do("movies", func() (any, error) {
		if movies, ok := p.cachedMovies(); ok {
			return movies, nil
		}
		p.moviesMu.Lock()
		haveFullCatalog := p.showsLoaded
		p.moviesMu.Unlock()
		if haveFullCatalog {
			lib, err := p.ListLibrary()
			return lib.Movies, err
		}
		return p.fetchMovies()
	})
	if err != nil {
		p.moviesMu.Lock()
		stale := append([]Movie(nil), p.moviesVal...)
		haveStale := p.moviesVal != nil
		p.moviesMu.Unlock()
		if haveStale {
			return stale, nil
		}
		return nil, err
	}
	return v.([]Movie), nil
}

// ListLibrary returns the cached top-level movie and TV-show catalog.
func (p *Plex) ListLibrary() (MediaLibrary, error) {
	if lib, ok := p.cachedLibrary(); ok {
		return normalizeLibrary(lib), nil
	}
	v, err, _ := p.listFlight.Do("list", func() (any, error) {
		// Re-check inside the flight: a caller that queued behind a
		// just-completed refresh must not immediately refetch.
		if lib, ok := p.cachedLibrary(); ok {
			return lib, nil
		}
		return p.fetchLibrary()
	})
	if err != nil {
		p.moviesMu.Lock()
		stale := MediaLibrary{Movies: p.moviesVal, Shows: p.showsVal}
		haveStale := p.moviesVal != nil || p.showsVal != nil
		p.moviesMu.Unlock()
		if haveStale {
			log.Printf("library: refresh failed (%v); serving stale cache of %d movies and %d shows",
				err, len(stale.Movies), len(stale.Shows))
			return normalizeLibrary(stale), nil
		}
		return MediaLibrary{}, err
	}
	return normalizeLibrary(v.(MediaLibrary)), nil
}

func normalizeLibrary(lib MediaLibrary) MediaLibrary {
	if lib.Movies == nil {
		lib.Movies = []Movie{}
	}
	if lib.Shows == nil {
		lib.Shows = []Show{}
	}
	return lib
}

func (p *Plex) cachedLibrary() (MediaLibrary, bool) {
	p.moviesMu.Lock()
	defer p.moviesMu.Unlock()
	if p.showsLoaded && time.Since(p.moviesAt) < moviesCacheTTL {
		return MediaLibrary{Movies: p.moviesVal, Shows: p.showsVal}, true
	}
	return MediaLibrary{}, false
}

func (p *Plex) cachedMovies() ([]Movie, bool) {
	p.moviesMu.Lock()
	defer p.moviesMu.Unlock()
	if p.moviesVal != nil && time.Since(p.moviesAt) < moviesCacheTTL {
		return p.moviesVal, true
	}
	return nil, false
}

// fetchMovies does the real library walk + IMDb enrichment and publishes
// the result. Callers go through ListMovies' singleflight.
func (p *Plex) fetchMovies() ([]Movie, error) {
	p.moviesMu.Lock()
	prev := p.moviesByKey
	p.moviesMu.Unlock()
	var sr sectionsResp
	if err := p.get("/library/sections", &sr); err != nil {
		return nil, err
	}
	out := make([]Movie, 0)
	seen := make(map[string]bool)
	for _, d := range sr.MediaContainer.Directory {
		if d.Type != "movie" {
			continue
		}
		var lr libraryResp
		if err := p.get("/library/sections/"+d.Key+"/all", &lr); err != nil {
			return nil, err
		}
		for _, m := range lr.MediaContainer.Metadata {
			if m.RatingKey == "" || seen[m.RatingKey] {
				continue
			}
			seen[m.RatingKey] = true
			out = append(out, Movie{
				RatingKey: m.RatingKey, Title: m.Title, Year: m.Year,
				Rating: m.Rating, AudienceRating: m.AudienceRating, Thumb: m.Thumb,
			})
		}
	}
	p.enrichIMDb(out, prev)
	p.moviesMu.Lock()
	p.moviesVal = out
	p.moviesAt = time.Now()
	p.moviesByKey = buildMoviesIndex(out)
	p.moviesMu.Unlock()
	p.saveCacheToDisk()
	return out, nil
}

func (p *Plex) fetchLibrary() (MediaLibrary, error) {
	p.moviesMu.Lock()
	prev := p.moviesByKey // snapshot for IMDb carry-forward; read off-lock below
	p.moviesMu.Unlock()

	var sr sectionsResp
	if err := p.get("/library/sections", &sr); err != nil {
		return MediaLibrary{}, err
	}
	out := make([]Movie, 0)
	shows := make([]Show, 0)
	seenMovies := make(map[string]bool)
	seenShows := make(map[string]bool)
	for _, d := range sr.MediaContainer.Directory {
		if d.Type != "movie" && d.Type != "show" {
			continue
		}
		var lr libraryResp
		if err := p.get("/library/sections/"+d.Key+"/all", &lr); err != nil {
			return MediaLibrary{}, err
		}
		for _, m := range lr.MediaContainer.Metadata {
			if d.Type == "show" {
				if m.RatingKey != "" && !seenShows[m.RatingKey] {
					seenShows[m.RatingKey] = true
					shows = append(shows, Show{RatingKey: m.RatingKey, Title: m.Title, Year: m.Year, Thumb: m.Thumb, Art: m.Art})
				}
				continue
			}
			if m.RatingKey != "" && !seenMovies[m.RatingKey] {
				seenMovies[m.RatingKey] = true
				out = append(out, Movie{
					RatingKey: m.RatingKey, Title: m.Title, Year: m.Year,
					Rating: m.Rating, AudienceRating: m.AudienceRating, Thumb: m.Thumb,
				})
			}
		}
	}

	// Fill in IMDb scores (the listing endpoint omits them). Best-effort and
	// done with no lock held — it makes its own Plex round-trips.
	p.enrichIMDb(out, prev)

	p.moviesMu.Lock()
	p.moviesVal = out
	p.showsVal = shows
	p.showsLoaded = true
	p.moviesAt = time.Now()
	p.moviesByKey = buildMoviesIndex(out)
	p.showsByKey = buildShowsIndex(shows)
	p.pruneHierarchyLocked(p.showsByKey)
	p.moviesMu.Unlock()
	p.saveCacheToDisk()
	return MediaLibrary{Movies: out, Shows: shows}, nil
}

// pruneHierarchyLocked drops cached descendants of shows that disappeared
// from a successful top-level catalog refresh. Unknown legacy entries are
// retained conservatively; only hierarchy whose owning show can be identified
// and is absent from the new catalog is removed.
func (p *Plex) pruneHierarchyLocked(knownShows map[string]Show) {
	seasonOwners := make(map[string]string)
	for showKey, seasons := range p.seasonsVal {
		for _, season := range seasons {
			owner := season.ParentRatingKey
			if owner == "" {
				owner = showKey
			}
			seasonOwners[season.RatingKey] = owner
		}
		if _, ok := knownShows[showKey]; !ok {
			delete(p.seasonsVal, showKey)
			delete(p.seasonsAt, showKey)
		}
	}
	for seasonKey, episodes := range p.episodesVal {
		owner := seasonOwners[seasonKey]
		if owner == "" {
			for _, episode := range episodes {
				if episode.GrandparentRatingKey != "" {
					owner = episode.GrandparentRatingKey
					break
				}
			}
		}
		if owner != "" {
			if _, ok := knownShows[owner]; !ok {
				delete(p.episodesVal, seasonKey)
				delete(p.episodesAt, seasonKey)
			}
		}
	}
	p.rebuildHierarchyIndexesLocked()
}

// plexRating is one entry of a movie's capital "Rating" array, e.g.
// {"image":"imdb://image.rating","value":5.9}.
type plexRating struct {
	Image string  `json:"image"`
	Value float64 `json:"value"`
}

// ratingBatchResp decodes a comma-batched /library/metadata/<k1,k2,…> reply,
// keeping only the ratingKey and the per-source Rating array. The scalar
// "rating" absorber is REQUIRED: Plex sends both a scalar "rating" (float)
// and a capital "Rating" (array) for each movie, and without an exact-case
// home for the scalar, Go's case-insensitive matching routes it into the
// array field and fails the whole decode — see [[plex-json-case-insensitive-collision]].
type ratingBatchResp struct {
	MediaContainer struct {
		Metadata []struct {
			RatingKey string       `json:"ratingKey"`
			Scalar    float64      `json:"rating"` // absorber, ignored
			Ratings   []plexRating `json:"Rating"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

// imdbFromRatings returns the IMDb score from a movie's Rating array, or 0 if
// Plex has none. Plex labels the imdb entry's type "audience", so match on
// the image source, not the type.
func imdbFromRatings(rs []plexRating) float64 {
	for _, r := range rs {
		if strings.HasPrefix(r.Image, "imdb://") {
			return r.Value
		}
	}
	return 0
}

// imdbBatchSize caps how many ratingKeys ride on one /library/metadata call.
// 200 keeps the URL well under any sane limit while collapsing a 5k-title
// library into ~26 requests on a cold cache.
const imdbBatchSize = 200

// enrichIMDb fills movies[i].IMDbRating. Scores already known from the prior
// cache (prev) are carried forward untouched; only titles still at 0 are
// fetched, in comma-batched /library/metadata calls. Best-effort: a failed
// batch is logged and skipped, leaving those titles at 0 (the listing — RT
// audience and all — still loads). prev may be nil on a cold start.
func (p *Plex) enrichIMDb(movies []Movie, prev map[string]Movie) {
	var need []string
	p.moviesMu.Lock()
	for i := range movies {
		if pm, ok := prev[movies[i].RatingKey]; ok && pm.IMDbRating > 0 {
			movies[i].IMDbRating = pm.IMDbRating
			continue
		}
		if p.imdbNoRating[movies[i].RatingKey] {
			continue // known unrated; re-checked only via RefreshLibrary
		}
		need = append(need, movies[i].RatingKey)
	}
	p.moviesMu.Unlock()
	if len(need) == 0 {
		return
	}
	got := make(map[string]float64, len(need))
	// Keys whose batch failed outright are NOT "known unrated" — the
	// fetch never answered; leave them eligible for the next refresh.
	failed := make(map[string]bool)
	for start := 0; start < len(need); start += imdbBatchSize {
		end := start + imdbBatchSize
		if end > len(need) {
			end = len(need)
		}
		keys := need[start:end]
		var br ratingBatchResp
		if err := p.get("/library/metadata/"+strings.Join(keys, ","), &br); err != nil {
			log.Printf("library: IMDb enrich batch of %d failed: %v", len(keys), err)
			for _, k := range keys {
				failed[k] = true
			}
			continue
		}
		for _, m := range br.MediaContainer.Metadata {
			if v := imdbFromRatings(m.Ratings); v > 0 {
				got[m.RatingKey] = v
			}
		}
	}
	for i := range movies {
		if v, ok := got[movies[i].RatingKey]; ok {
			movies[i].IMDbRating = v
		}
	}
	p.moviesMu.Lock()
	for _, k := range need {
		if _, ok := got[k]; !ok && !failed[k] {
			p.imdbNoRating[k] = true
		}
	}
	p.moviesMu.Unlock()
}

// MovieByKey returns the movie metadata for ratingKey from the in-memory
// index, or (Movie{}, false) if absent. O(1) — avoids the linear scan
// over ListMovies() at /control load time.
func (p *Plex) MovieByKey(ratingKey string) (Movie, bool) {
	p.moviesMu.Lock()
	defer p.moviesMu.Unlock()
	m, ok := p.moviesByKey[ratingKey]
	return m, ok
}

func buildMoviesIndex(movies []Movie) map[string]Movie {
	idx := make(map[string]Movie, len(movies))
	for _, m := range movies {
		idx[m.RatingKey] = m
	}
	return idx
}

func buildShowsIndex(shows []Show) map[string]Show {
	idx := make(map[string]Show, len(shows))
	for _, show := range shows {
		idx[show.RatingKey] = show
	}
	return idx
}

type childrenResp struct {
	MediaContainer struct {
		Metadata []struct {
			Type                  string `json:"type"`
			RatingKey             string `json:"ratingKey"`
			ParentRatingKey       string `json:"parentRatingKey"`
			GrandparentRatingKey  string `json:"grandparentRatingKey"`
			Title                 string `json:"title"`
			GrandparentTitle      string `json:"grandparentTitle"`
			Index                 int    `json:"index"`
			ParentIndex           int    `json:"parentIndex"`
			Duration              int64  `json:"duration"`
			OriginallyAvailableAt string `json:"originallyAvailableAt"`
			Thumb                 string `json:"thumb"`
			Art                   string `json:"art"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

// ListSeasons and ListEpisodes lazily walk the Plex hierarchy. A successful
// result is persisted for restart/offline fallback; concurrent requests for
// the same parent collapse into one upstream call.
func (p *Plex) ListSeasons(showKey string) ([]Season, error) {
	p.moviesMu.Lock()
	if seasons, ok := p.seasonsVal[showKey]; ok && cacheTimeFresh(p.seasonsAt[showKey], hierarchyCacheTTL) {
		out := append([]Season(nil), seasons...)
		p.moviesMu.Unlock()
		return out, nil
	}
	p.moviesMu.Unlock()
	v, err, _ := p.listFlight.Do("seasons:"+showKey, func() (any, error) {
		p.moviesMu.Lock()
		if seasons, ok := p.seasonsVal[showKey]; ok && cacheTimeFresh(p.seasonsAt[showKey], hierarchyCacheTTL) {
			out := append([]Season(nil), seasons...)
			p.moviesMu.Unlock()
			return out, nil
		}
		p.moviesMu.Unlock()
		var cr childrenResp
		if err := p.get("/library/metadata/"+showKey+"/children", &cr); err != nil {
			return nil, err
		}
		seasons := make([]Season, 0, len(cr.MediaContainer.Metadata))
		for _, m := range cr.MediaContainer.Metadata {
			if m.Type != "" && m.Type != "season" {
				continue
			}
			seasons = append(seasons, Season{
				RatingKey: m.RatingKey, ParentRatingKey: showKey, Title: m.Title,
				Index: m.Index, Thumb: m.Thumb, Art: m.Art,
			})
		}
		sort.SliceStable(seasons, func(i, j int) bool {
			// Match Plex's browsing convention: regular seasons first and
			// Specials (index 0) last. NextEpisode treats specials separately,
			// so this display ordering does not affect playback progression.
			if seasons[i].Index == 0 && seasons[j].Index != 0 {
				return false
			}
			if seasons[j].Index == 0 && seasons[i].Index != 0 {
				return true
			}
			if seasons[i].Index != seasons[j].Index {
				return seasons[i].Index < seasons[j].Index
			}
			return seasons[i].Title < seasons[j].Title
		})
		p.moviesMu.Lock()
		if p.seasonsVal == nil {
			p.seasonsVal = make(map[string][]Season)
		}
		if p.seasonsAt == nil {
			p.seasonsAt = make(map[string]time.Time)
		}
		p.seasonsVal[showKey] = seasons
		p.seasonsAt[showKey] = time.Now()
		p.rebuildHierarchyIndexesLocked()
		p.moviesMu.Unlock()
		p.saveCacheToDisk()
		return append([]Season(nil), seasons...), nil
	})
	if err != nil {
		p.moviesMu.Lock()
		stale, ok := p.seasonsVal[showKey]
		out := append([]Season(nil), stale...)
		p.moviesMu.Unlock()
		if ok {
			return out, nil
		}
		return nil, err
	}
	return v.([]Season), nil
}

func (p *Plex) ListEpisodes(seasonKey string) ([]Episode, error) {
	p.moviesMu.Lock()
	if episodes, ok := p.episodesVal[seasonKey]; ok && cacheTimeFresh(p.episodesAt[seasonKey], hierarchyCacheTTL) {
		out := append([]Episode(nil), episodes...)
		p.moviesMu.Unlock()
		return out, nil
	}
	season := p.seasonsByKey[seasonKey]
	show := p.showsByKey[season.ParentRatingKey]
	p.moviesMu.Unlock()
	v, err, _ := p.listFlight.Do("episodes:"+seasonKey, func() (any, error) {
		p.moviesMu.Lock()
		if episodes, ok := p.episodesVal[seasonKey]; ok && cacheTimeFresh(p.episodesAt[seasonKey], hierarchyCacheTTL) {
			out := append([]Episode(nil), episodes...)
			p.moviesMu.Unlock()
			return out, nil
		}
		p.moviesMu.Unlock()
		var cr childrenResp
		if err := p.get("/library/metadata/"+seasonKey+"/children", &cr); err != nil {
			return nil, err
		}
		episodes := make([]Episode, 0, len(cr.MediaContainer.Metadata))
		for _, m := range cr.MediaContainer.Metadata {
			if m.Type != "" && m.Type != "episode" {
				continue
			}
			showKey := m.GrandparentRatingKey
			if showKey == "" {
				showKey = season.ParentRatingKey
			}
			seriesTitle := m.GrandparentTitle
			if seriesTitle == "" {
				seriesTitle = show.Title
			}
			seasonNumber := m.ParentIndex
			if m.ParentIndex == 0 && season.Index != 0 {
				seasonNumber = season.Index
			}
			episodes = append(episodes, Episode{
				RatingKey: m.RatingKey, ParentRatingKey: seasonKey,
				GrandparentRatingKey: showKey, Title: m.Title, SeriesTitle: seriesTitle,
				SeasonNumber: seasonNumber, EpisodeNumber: m.Index, Duration: m.Duration,
				OriginallyAvailableAt: m.OriginallyAvailableAt, Thumb: m.Thumb, Art: m.Art,
			})
		}
		sort.SliceStable(episodes, func(i, j int) bool {
			if episodes[i].EpisodeNumber != episodes[j].EpisodeNumber {
				return episodes[i].EpisodeNumber < episodes[j].EpisodeNumber
			}
			return episodes[i].Title < episodes[j].Title
		})
		p.moviesMu.Lock()
		if p.episodesVal == nil {
			p.episodesVal = make(map[string][]Episode)
		}
		if p.episodesAt == nil {
			p.episodesAt = make(map[string]time.Time)
		}
		p.episodesVal[seasonKey] = episodes
		p.episodesAt[seasonKey] = time.Now()
		p.rebuildHierarchyIndexesLocked()
		p.moviesMu.Unlock()
		p.saveCacheToDisk()
		return append([]Episode(nil), episodes...), nil
	})
	if err != nil {
		p.moviesMu.Lock()
		stale, ok := p.episodesVal[seasonKey]
		out := append([]Episode(nil), stale...)
		p.moviesMu.Unlock()
		if ok {
			return out, nil
		}
		return nil, err
	}
	return v.([]Episode), nil
}

func cacheTimeFresh(at time.Time, ttl time.Duration) bool {
	return !at.IsZero() && time.Since(at) < ttl
}

func (p *Plex) HasShow(ratingKey string) bool {
	p.moviesMu.Lock()
	defer p.moviesMu.Unlock()
	_, ok := p.showsByKey[ratingKey]
	return ok
}

func (p *Plex) HasSeason(ratingKey string) bool {
	p.moviesMu.Lock()
	defer p.moviesMu.Unlock()
	season, ok := p.seasonsByKey[ratingKey]
	if !ok {
		return false
	}
	_, showKnown := p.showsByKey[season.ParentRatingKey]
	return showKnown
}

func (p *Plex) rebuildHierarchyIndexesLocked() {
	p.seasonsByKey = make(map[string]Season)
	for _, seasons := range p.seasonsVal {
		for _, season := range seasons {
			p.seasonsByKey[season.RatingKey] = season
		}
	}
	p.episodesByKey = make(map[string]Episode)
	for _, episodes := range p.episodesVal {
		for _, episode := range episodes {
			p.episodesByKey[episode.RatingKey] = episode
		}
	}
}

func (p *Plex) EpisodeByKey(ratingKey string) (Episode, bool) {
	p.moviesMu.Lock()
	defer p.moviesMu.Unlock()
	episode, ok := p.episodesByKey[ratingKey]
	return episode, ok
}

// NextEpisode follows regular seasons in numeric order. Specials (season 0)
// only advance within specials and never enter the regular-season chain.
func (p *Plex) NextEpisode(current Episode) (*Episode, error) {
	if current.ParentRatingKey == "" {
		return nil, fmt.Errorf("episode %s has no parent season", current.RatingKey)
	}
	episodes, err := p.ListEpisodes(current.ParentRatingKey)
	if err != nil {
		return nil, err
	}
	for _, episode := range episodes {
		if episode.EpisodeNumber > current.EpisodeNumber {
			next := episode
			return &next, nil
		}
	}
	if current.SeasonNumber == 0 {
		return nil, nil
	}
	if current.GrandparentRatingKey == "" {
		return nil, fmt.Errorf("episode %s has no parent show", current.RatingKey)
	}
	seasons, err := p.ListSeasons(current.GrandparentRatingKey)
	if err != nil {
		return nil, err
	}
	for _, season := range seasons {
		if season.Index <= current.SeasonNumber || season.Index == 0 {
			continue
		}
		nextEpisodes, err := p.ListEpisodes(season.RatingKey)
		if err != nil {
			return nil, err
		}
		if len(nextEpisodes) > 0 {
			next := nextEpisodes[0]
			return &next, nil
		}
	}
	return nil, nil
}

type metadataResp struct {
	MediaContainer struct {
		Metadata []struct {
			Type                 string  `json:"type"`
			RatingKey            string  `json:"ratingKey"`
			Title                string  `json:"title"`
			Year                 int     `json:"year"`
			Thumb                string  `json:"thumb"`
			Art                  string  `json:"art"`
			ParentRatingKey      string  `json:"parentRatingKey"`
			GrandparentRatingKey string  `json:"grandparentRatingKey"`
			GrandparentTitle     string  `json:"grandparentTitle"`
			Index                int     `json:"index"`
			ParentIndex          int     `json:"parentIndex"`
			Tagline              string  `json:"tagline"`
			Summary              string  `json:"summary"`
			ContentRating        string  `json:"contentRating"`
			Rating               float64 `json:"rating"`
			AudienceRating       float64 `json:"audienceRating"`
			// Plex returns BOTH a scalar "guid"/"rating" and a capital-letter
			// "Guid"/"Rating" array for the same concepts. Go's json matching
			// is case-insensitive, so without these exact-case absorber fields
			// the string "guid" and the array "Rating" get unmarshaled into the
			// scalar fields above and fail the whole decode. Captured + ignored.
			GUIDString  string          `json:"guid"`
			RatingArray json.RawMessage `json:"Rating"`
			Genre       []struct {
				Tag string `json:"tag"`
			} `json:"Genre"`
			Guid []struct {
				ID string `json:"id"`
			} `json:"Guid"`
			Duration int64 `json:"duration"`
			Media    []struct {
				Container      string `json:"container"`
				VideoCodec     string `json:"videoCodec"`
				AudioCodec     string `json:"audioCodec"`
				VideoProfile   string `json:"videoProfile"`
				AudioProfile   string `json:"audioProfile"`
				Width          int    `json:"width"`
				Height         int    `json:"height"`
				Bitrate        int    `json:"bitrate"`
				AudioChannels  int    `json:"audioChannels"`
				VideoFrameRate string `json:"videoFrameRate"`
				Duration       int64  `json:"duration"`
				// Plex sets this to 1 on Optimized (pre-transcoded)
				// versions and on Direct Stream-friendly originals.
				// We treat 1 as "browser-friendly variant" and prefer
				// it over the raw original (which can be 70+ Mbps HEVC
				// HDR that no browser wants to deal with).
				OptimizedForStreaming int `json:"optimizedForStreaming"`
				Part                  []struct {
					Key    string `json:"key"`
					Size   int64  `json:"size"`
					Stream []struct {
						StreamType int    `json:"streamType"` // 1=video, 2=audio
						Codec      string `json:"codec"`
						Bitrate    int    `json:"bitrate"`
					} `json:"Stream"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

// Resolve turns a ratingKey into a direct, range-capable progressive URL plus
// the codec/profile/size/etc. info we want to log and act on.
func (p *Plex) Resolve(ratingKey string) (*StreamInfo, *MovieMeta, error) {
	var mr metadataResp
	if err := p.get("/library/metadata/"+ratingKey, &mr); err != nil {
		return nil, nil, err
	}
	if len(mr.MediaContainer.Metadata) == 0 ||
		len(mr.MediaContainer.Metadata[0].Media) == 0 {
		return nil, nil, fmt.Errorf("no playable part for ratingKey %s", ratingKey)
	}
	metadata := mr.MediaContainer.Metadata[0]
	if metadata.Type != "" && metadata.Type != "movie" && metadata.Type != "episode" {
		return nil, nil, fmt.Errorf("ratingKey %s is %s, not playable movie or episode", ratingKey, metadata.Type)
	}

	// Pick the best Media variant for browser playback. Plex movies
	// often have multiple Media entries: the original Blu-ray remux
	// (HEVC HDR @ 70+ Mbps) plus one or more Optimized versions
	// (h264 @ 8-12 Mbps, what Plex generates via "Optimize"). The
	// optimized version is dramatically friendlier to MSE / hls.js /
	// VideoToolbox — fewer decoder errors, smaller buffers, broader
	// browser compat. Always prefer it when present.
	mediaIdx := -1
	chosenReason := "first playable variant"
	for i, m := range metadata.Media {
		if len(m.Part) > 0 && mediaIdx == -1 {
			mediaIdx = i
		}
		if m.OptimizedForStreaming == 1 && len(m.Part) > 0 {
			mediaIdx = i
			chosenReason = "optimizedForStreaming=1"
			break
		}
	}
	if mediaIdx == -1 {
		return nil, nil, fmt.Errorf("no playable part for ratingKey %s", ratingKey)
	}
	media := metadata.Media[mediaIdx]
	if len(media.Part) == 0 {
		return nil, nil, fmt.Errorf("chosen media variant has no Part for ratingKey %s", ratingKey)
	}
	part := media.Part[0]
	if len(metadata.Media) > 1 {
		log.Printf("plex: ratingKey %s has %d Media variants; picked #%d (%s, %dx%d %s @ %d kbps)",
			ratingKey, len(metadata.Media), mediaIdx, chosenReason,
			media.Width, media.Height, media.VideoCodec, media.Bitrate)
	}

	// Older Plex responses only populate Duration at the Metadata level,
	// not inside the Media block. Prefer the inner value (it's per-version
	// and more accurate when a library item has multiple Media entries)
	// but fall back to the outer one so the scrub bar always has a real
	// movie length and not 0 / v.duration.
	duration := media.Duration
	if duration == 0 {
		duration = metadata.Duration
		if duration > 0 {
			log.Printf("plex: ratingKey %s — Media.Duration was 0, falling back to Metadata.Duration=%dms",
				ratingKey, duration)
		} else {
			log.Printf("plex: ratingKey %s — no duration at either level; scrub bar will fall back to v.duration",
				ratingKey)
		}
	}

	si := &StreamInfo{
		VideoCodec:   strings.ToLower(media.VideoCodec),
		VideoProfile: media.VideoProfile,
		Width:        media.Width,
		Height:       media.Height,
		Bitrate:      media.Bitrate,
		FrameRate:    media.VideoFrameRate,
		Duration:     duration,
	}
	// Fall back to Stream entries if the top-level VideoCodec is empty
	// (older Plex responses sometimes only populate one level).
	for _, s := range part.Stream {
		if s.StreamType == 1 && si.VideoCodec == "" {
			si.VideoCodec = strings.ToLower(s.Codec)
		}
	}

	meta := &MovieMeta{
		MediaType:            metadata.Type,
		RatingKey:            metadata.RatingKey,
		Title:                metadata.Title,
		Year:                 metadata.Year,
		Thumb:                metadata.Thumb,
		Art:                  metadata.Art,
		SeriesTitle:          metadata.GrandparentTitle,
		SeasonNumber:         metadata.ParentIndex,
		EpisodeNumber:        metadata.Index,
		ParentRatingKey:      metadata.ParentRatingKey,
		GrandparentRatingKey: metadata.GrandparentRatingKey,
		Tagline:              metadata.Tagline,
		Summary:              metadata.Summary,
		ContentRating:        metadata.ContentRating,
		CriticRating:         metadata.Rating,
		AudienceRating:       metadata.AudienceRating,
	}
	if meta.MediaType == "" {
		meta.MediaType = "movie" // legacy Plex/fixtures omitted type
	}
	if meta.RatingKey == "" {
		meta.RatingKey = ratingKey
	}
	for _, g := range metadata.Genre {
		if g.Tag != "" {
			meta.Genres = append(meta.Genres, g.Tag)
		}
	}
	// Plex's Guid array carries external IDs like "imdb://tt0089886" and
	// "tmdb://14370". (The Rating array's "imdb://image.rating" lives
	// elsewhere and is not parsed here.)
	for _, gu := range metadata.Guid {
		switch {
		case strings.HasPrefix(gu.ID, "imdb://tt"):
			meta.IMDbID = strings.TrimPrefix(gu.ID, "imdb://")
		case strings.HasPrefix(gu.ID, "tmdb://"):
			meta.TMDBID = strings.TrimPrefix(gu.ID, "tmdb://")
		}
	}
	return si, meta, nil
}

// errNoPoster signals the movie has no thumb art (handler maps it to 404).
var errNoPoster = errors.New("no poster art for media")

// posterTranscodeW/H are the box Plex scales the poster into. 400×600 (2:3)
// covers the library grid's ~150–250px columns at 2x/retina, so the result
// looks identical to the source on screen while transferring a fraction of
// the bytes of a full-res poster.
const posterTranscodeW = 400
const posterTranscodeH = 600

// PosterStream fetches a movie's Plex poster art for ratingKey. The caller
// owns the returned ReadCloser and must Close it. The Plex token is sent
// only as a query param to Plex and never appears in anything we hand back.
//
// Two optimizations over a naive fetch: (1) the thumb path is taken from the
// in-memory library cache when present, avoiding a per-image /library/metadata
// round-trip (it falls back to a metadata fetch when the cache lacks it — a
// key outside the library, or a disk cache restored from before thumbs were
// stored); (2) the image is pulled through Plex's photo transcoder at card
// size instead of full resolution.
func (p *Plex) PosterStream(ratingKey string) (io.ReadCloser, string, error) {
	// Only serve titles the library cache knows. The poster endpoint is
	// unauthenticated (Discord fetches embed images from the public
	// internet), so an unknown key must not become a free metadata probe
	// against Plex — refusing here keeps alphanumeric key scans from
	// enumerating the server or hammering it with per-key fetches.
	p.moviesMu.Lock()
	thumb := ""
	if m, ok := p.moviesByKey[ratingKey]; ok {
		thumb = m.Thumb
	} else if show, ok := p.showsByKey[ratingKey]; ok {
		thumb = show.Thumb
	} else if season, ok := p.seasonsByKey[ratingKey]; ok {
		thumb = season.Thumb
	} else if episode, ok := p.episodesByKey[ratingKey]; ok {
		thumb = episode.Thumb
	} else {
		p.moviesMu.Unlock()
		return nil, "", errNoPoster
	}
	p.moviesMu.Unlock()
	if thumb == "" {
		// Library caches persisted before thumbs were stored lack the
		// path; a one-off metadata fetch fills it for this request.
		var mr metadataResp
		if err := p.get("/library/metadata/"+ratingKey, &mr); err != nil {
			return nil, "", err
		}
		if len(mr.MediaContainer.Metadata) == 0 {
			return nil, "", fmt.Errorf("no metadata for ratingKey %s", ratingKey)
		}
		thumb = mr.MediaContainer.Metadata[0].Thumb
	}
	if thumb == "" {
		return nil, "", errNoPoster
	}
	// Ask Plex's photo transcoder for a card-sized JPEG of the thumb rather
	// than streaming the full-res source. minSize=1 fits within the box
	// preserving aspect; upscale=1 keeps small sources from coming back tiny.
	u := p.BaseURL + "/photo/:/transcode" +
		"?width=" + strconv.Itoa(posterTranscodeW) +
		"&height=" + strconv.Itoa(posterTranscodeH) +
		"&minSize=1&upscale=1" +
		"&url=" + url.QueryEscape(thumb) +
		"&X-Plex-Token=" + url.QueryEscape(p.Token)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := p.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("plex thumb: status %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	return resp.Body, ct, nil
}
