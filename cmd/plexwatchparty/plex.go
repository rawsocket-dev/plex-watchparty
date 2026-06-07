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
	"strings"
	"sync"
	"time"
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
	// container restart doesn't pay the cold-start cost.
	moviesMu    sync.Mutex
	moviesAt    time.Time
	moviesVal   []Movie
	moviesByKey map[string]Movie // O(1) ratingKey lookup, refreshed alongside moviesVal
	cacheFile   string

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
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		http:      &http.Client{Timeout: 15 * time.Second, Transport: tr},
		cacheFile: cacheFile,
		healthy:   true, // optimistic; startup Ping in main flips this if Plex is down
		audit:     audit,
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
	Tagline        string
	Summary        string
	ContentRating  string  // "PG", "R", ...
	CriticRating   float64 // Plex "rating" (0–10)
	AudienceRating float64 // Plex "audienceRating" (0–10)
	Genres         []string
	IMDbID         string // e.g. "tt0089886" ("" if Plex has none)
	TMDBID         string // e.g. "14370" ("" if Plex has none)
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
			RatingKey      string  `json:"ratingKey"`
			Title          string  `json:"title"`
			Year           int     `json:"year"`
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
	At     time.Time `json:"at"`
	Movies []Movie   `json:"movies"`
}

func (p *Plex) loadCacheFromDisk() {
	if p.cacheFile == "" {
		return
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
	p.moviesAt = entry.At
	p.moviesByKey = buildMoviesIndex(entry.Movies)
	p.moviesMu.Unlock()
	log.Printf("library cache: loaded %d titles from %s (saved %s)",
		len(entry.Movies), p.cacheFile,
		time.Since(entry.At).Round(time.Second))
}

func (p *Plex) saveCacheToDisk() {
	if p.cacheFile == "" {
		return
	}
	p.moviesMu.Lock()
	entry := cachedLibrary{At: p.moviesAt, Movies: p.moviesVal}
	p.moviesMu.Unlock()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.cacheFile), 0o755); err != nil {
		log.Printf("library cache: mkdir: %v", err)
		return
	}
	tmp := p.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("library cache: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, p.cacheFile); err != nil {
		log.Printf("library cache: rename: %v", err)
	}
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
	titles := len(p.moviesVal)
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
	p.moviesMu.Unlock()
	log.Printf("library: cache invalidated; next ListMovies will refetch from Plex")
}

// ListMovies returns every item across all movie-type library sections.
// Cached in-memory + on disk for `moviesCacheTTL`.
func (p *Plex) ListMovies() ([]Movie, error) {
	p.moviesMu.Lock()
	if p.moviesVal != nil && time.Since(p.moviesAt) < moviesCacheTTL {
		out := p.moviesVal
		p.moviesMu.Unlock()
		return out, nil
	}
	p.moviesMu.Unlock()

	var sr sectionsResp
	if err := p.get("/library/sections", &sr); err != nil {
		return nil, err
	}
	var out []Movie
	for _, d := range sr.MediaContainer.Directory {
		if d.Type != "movie" {
			continue
		}
		var lr libraryResp
		if err := p.get("/library/sections/"+d.Key+"/all", &lr); err != nil {
			return nil, err
		}
		for _, m := range lr.MediaContainer.Metadata {
			out = append(out, Movie{
				RatingKey:      m.RatingKey,
				Title:          m.Title,
				Year:           m.Year,
				Rating:         m.Rating,
				AudienceRating: m.AudienceRating,
			})
		}
	}

	p.moviesMu.Lock()
	p.moviesVal = out
	p.moviesAt = time.Now()
	p.moviesByKey = buildMoviesIndex(out)
	p.moviesMu.Unlock()
	p.saveCacheToDisk()
	return out, nil
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

type metadataResp struct {
	MediaContainer struct {
		Metadata []struct {
			Title          string  `json:"title"`
			Thumb          string  `json:"thumb"`
			Tagline        string  `json:"tagline"`
			Summary        string  `json:"summary"`
			ContentRating  string  `json:"contentRating"`
			Rating         float64 `json:"rating"`
			AudienceRating float64 `json:"audienceRating"`
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
		len(mr.MediaContainer.Metadata[0].Media) == 0 ||
		len(mr.MediaContainer.Metadata[0].Media[0].Part) == 0 {
		return nil, nil, fmt.Errorf("no playable part for ratingKey %s", ratingKey)
	}
	metadata := mr.MediaContainer.Metadata[0]

	// Pick the best Media variant for browser playback. Plex movies
	// often have multiple Media entries: the original Blu-ray remux
	// (HEVC HDR @ 70+ Mbps) plus one or more Optimized versions
	// (h264 @ 8-12 Mbps, what Plex generates via "Optimize"). The
	// optimized version is dramatically friendlier to MSE / hls.js /
	// VideoToolbox — fewer decoder errors, smaller buffers, broader
	// browser compat. Always prefer it when present.
	mediaIdx := 0
	chosenReason := "default (only variant)"
	if len(metadata.Media) > 1 {
		chosenReason = "default (no optimized variant)"
		for i, m := range metadata.Media {
			if m.OptimizedForStreaming == 1 && len(m.Part) > 0 {
				mediaIdx = i
				chosenReason = "optimizedForStreaming=1"
				break
			}
		}
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
		Tagline:        metadata.Tagline,
		Summary:        metadata.Summary,
		ContentRating:  metadata.ContentRating,
		CriticRating:   metadata.Rating,
		AudienceRating: metadata.AudienceRating,
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
var errNoPoster = errors.New("no poster art for movie")

// PosterStream fetches a movie's Plex poster art for ratingKey. The caller
// owns the returned ReadCloser and must Close it. The Plex token is sent
// only as a query param to Plex and never appears in anything we hand back.
func (p *Plex) PosterStream(ratingKey string) (io.ReadCloser, string, error) {
	var mr metadataResp
	if err := p.get("/library/metadata/"+ratingKey, &mr); err != nil {
		return nil, "", err
	}
	if len(mr.MediaContainer.Metadata) == 0 {
		return nil, "", fmt.Errorf("no metadata for ratingKey %s", ratingKey)
	}
	thumb := mr.MediaContainer.Metadata[0].Thumb
	if thumb == "" {
		return nil, "", errNoPoster
	}
	u := p.BaseURL + thumb
	if strings.Contains(thumb, "?") {
		u += "&"
	} else {
		u += "?"
	}
	u += "X-Plex-Token=" + url.QueryEscape(p.Token)
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
